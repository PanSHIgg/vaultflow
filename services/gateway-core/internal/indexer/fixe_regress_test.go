package indexer

// FIX-E 回归测试 (PG 集成; 未设 DSN 自动 skip):
//   E1 — 人工审核结算与链上结算共用 RefPrefixOrderSettle 前缀,
//         深回滚必须找回人工结算腿完成整单红冲;
//   E2 — settlePayoutSuccess 在记账前做迁移守卫: payout 已取消时
//         链上成交不再二次记账, 索引器不硬卡.

import (
	"context"
	"math/big"
	"testing"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/ledger"
	"vaultflow/gateway-core/internal/order"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// E1: 订单 PARTIALLY_PAID(400/1000) -> 过期重评 UNDERPAID_EXPIRED_REVIEW
// -> 人工 ACCEPT_UNDERPAID 结算 PAID (review.go 写 ordsettle:) ->
// 关联充值所在块深分叉 -> unwindSettledOrder 找回人工结算腿红冲,
// 订单 SETTLED_REVERSED, 账本全维归零. (修复前 review.go 写
// "order_settle:" 而反查找 "ordsettle:", 回滚将永远卡死在无腿错误上.)
func TestManualReviewSettleDeepRollback(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 100) // 1% 费率
	v1, o1, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")

	real := ledger.NewService(db)
	blk := chainOf(8, 'A')
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 8},
		headers: blk,
		evs: map[uint64][]chain.TransferEvent{6: {
			transferEv(v1, usdcContract, ext, 400, 6, types.HexToHash("0x09"))}},
		receipts: map[types.Hash]*ethtypes.Receipt{},
	}
	seedCursor(t, db, 5, blk[5].Hash)
	ix := newIx(db, fq, real, &fakeRebc{})
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st, recv := orderState(t, db, o1); st != "PARTIALLY_PAID" || recv != "400" {
		t.Fatalf("order %s recv=%s want PARTIALLY_PAID/400", st, recv)
	}

	// 过期 -> UNDERPAID_EXPIRED_REVIEW (escrow 转挂 SUSPENSE_REFUNDABLE).
	if _, err := db.Exec(ctx,
		`UPDATE payment_orders SET expires_at=now()-interval '1 hour' WHERE id=$1`, o1); err != nil {
		t.Fatal(err)
	}
	svc := order.NewService(db,
		create2.NewDeriver(types.HexToAddress("0x1111111111111111111111111111111111111111"),
			types.HexToAddress("0x2222222222222222222222222222222222222222")), real)
	if _, err := svc.ReevaluateExpiredOrders(ctx, time.Now(), 10); err != nil {
		t.Fatal(err)
	}
	if st, _ := orderState(t, db, o1); st != "UNDERPAID_EXPIRED_REVIEW" {
		t.Fatalf("order %s want UNDERPAID_EXPIRED_REVIEW", st)
	}

	// 人工折让接受 -> PAID; 结算事务必须使用共享规范前缀 (E1).
	if _, err := svc.ReviewOrder(ctx, ports.ReviewOrderRequest{
		OrderID: o1, Action: ports.ReviewAcceptUnderpaid,
	}); err != nil {
		t.Fatal(err)
	}
	if st, _ := orderState(t, db, o1); st != "PAID" {
		t.Fatalf("order %s want PAID", st)
	}
	var settleRef string
	if err := db.QueryRow(ctx,
		`SELECT reference_id FROM ledger_transactions WHERE reference_type='ORDER_SETTLE'`).
		Scan(&settleRef); err != nil {
		t.Fatal(err)
	}
	if settleRef != types.RefPrefixOrderSettle+o1.String() {
		t.Fatalf("settle reference_id=%s want %s (E1 canonical prefix)",
			settleRef, types.RefPrefixOrderSettle+o1.String())
	}
	if got := ledgerBal(t, db, m, types.AccountMerchantAvailable); got != "396" {
		t.Fatalf("MA %s want 396 (400 - 1%% fee)", got)
	}
	if got := ledgerBal(t, db, sysID, types.AccountFeeRevenue); got != "4" {
		t.Fatalf("FEE %s want 4", got)
	}

	// 深分叉: 高度 6 起 B 链 (公共祖先 a5) — dep ORPHANED_DEEP 触发整单红冲.
	remote := map[uint64]*chain.BlockHeader{}
	for i := uint64(0); i <= 5; i++ {
		remote[i] = blk[i]
	}
	b6 := &chain.BlockHeader{Number: 6, Hash: types.HexToHash("0x000b06"), ParentHash: blk[5].Hash}
	b7 := &chain.BlockHeader{Number: 7, Hash: types.HexToHash("0x000b07"), ParentHash: b6.Hash}
	b8 := &chain.BlockHeader{Number: 8, Hash: types.HexToHash("0x000b08"), ParentHash: b7.Hash}
	remote[6], remote[7], remote[8] = b6, b7, b8
	fq.headers = remote
	fq.heads = chain.Heights{Latest: 8, Safe: 8}
	fq.evs = map[uint64][]chain.TransferEvent{}

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatalf("deep rollback after manual settle must not stall: %v", err)
	}
	if st, recv := orderState(t, db, o1); st != "SETTLED_REVERSED" || recv != "0" {
		t.Fatalf("order %s recv=%s want SETTLED_REVERSED/0", st, recv)
	}
	for _, b := range []struct {
		owner uuid.UUID
		acct  types.LedgerAccountCode
	}{{m, types.AccountMerchantAvailable},
		{sysID, types.AccountFeeRevenue},
		{sysID, types.AccountCustodyUnswept},
		{m, types.AccountSuspenseRefundableLiab},
		{m, types.AccountOrderEscrowLiab}} {
		if got := ledgerBal(t, db, b.owner, b.acct); got != "0" {
			t.Fatalf("%s %s want 0 after unwind", b.owner, b.acct)
		}
	}
	var reorgs int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE reference_type='REORG_REVERSAL'`).
		Scan(&reorgs); err != nil || reorgs != 1 {
		t.Fatalf("REORG_REVERSAL txns=%d err=%v want 1", reorgs, err)
	}
	if s := depStatus(t, db, types.HexToHash("0x09")); s != "ORPHANED_DEEP" {
		t.Fatalf("dep %s want ORPHANED_DEEP", s)
	}
}

// E2: QueueIntent→首播窗口内 CancelPayout 竞速获胜后, 链上候选仍真实
// 成交 — settlePayoutSuccess 必须先判迁移守卫: 跳过 PAYOUT_FLOW 执行腿
// (PAYOUT_LOCKED 不双扣 / CUSTODY 不双贷), 不改单据状态, 发 CRITICAL
// 结构化告警, 索引器照常推进 (attempt FINALIZED / intent CONFIRMED).
func TestSettlePayoutExecAfterCancelSkipsAccounting(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 0)
	payoutID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO merchant_payouts (id, merchant_id, idempotency_key, request_hash,
		   payout_kind, source_account_type, chain_id, asset_symbol, to_address, amount, status)
		 VALUES ($1,$2,'ik','rh','WITHDRAWAL','MERCHANT_AVAILABLE',$3,'USDC',$4,300,'CANCELLED_UNLOCKED')`,
		payoutID, m, int64(testChain),
		addrStr(types.HexToAddress("0x7777777777777777777777777777777777777777"))); err != nil {
		t.Fatal(err)
	}
	intentID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_intents (id, intent_type, reference_id, chain_id, wallet_address, nonce, status)
		 VALUES ($1,'MERCHANT_PAYOUT',$2,$3,$4,7,'IN_FLIGHT')`,
		intentID, payoutID, int64(testChain), addrStr(payoutHot)); err != nil {
		t.Fatal(err)
	}
	txh := types.HexToHash("0xabc9")
	attID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_attempts
		   (id, intent_id, attempt_no, attempt_kind, chain_id, tx_hash,
		    max_fee_per_gas, max_priority_fee_per_gas, status)
		 VALUES ($1,$2,1,'PRIMARY_INTENT',$3,$4,3000000000,1000000000,'BROADCASTED')`,
		attID, intentID, int64(testChain), hashStr(txh)); err != nil {
		t.Fatal(err)
	}

	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 9},
		headers: chainOf(10, 'A'),
		evs:     map[uint64][]chain.TransferEvent{},
		receipts: map[types.Hash]*ethtypes.Receipt{txh: {
			Status: 1, GasUsed: 21000, EffectiveGasPrice: big.NewInt(2_000_000_000),
			BlockNumber: big.NewInt(6), BlockHash: mkHeader(6, 'A').Hash}},
	}
	seedCursor(t, db, 5, chainOf(10, 'A')[5].Hash)
	led := newFakeLedger(db)
	led.seedBalance(sysID, testChain, "ETH", types.AccountHotWalletNative, 1_000_000_000_000_000)
	var alerts []Alert
	ix := New(db, fq, led, nil, &fakeRebc{}, Config{
		ChainID: testChain, BatchBlocks: 100,
		PayoutHotWallet: payoutHot, RelayerHotWallet: relayerHot,
		ColdWallet: coldWallet, BatchSweeper: sweeperAddr,
	}, nil, func(a Alert) { alerts = append(alerts, a) })

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatalf("indexer must not stall on cancelled-payout execution: %v", err)
	}
	var attSt, intSt string
	if err := db.QueryRow(ctx,
		`SELECT a.status, i.status FROM outbound_tx_attempts a
		   JOIN outbound_tx_intents i ON i.id=a.intent_id WHERE a.id=$1`, attID).
		Scan(&attSt, &intSt); err != nil {
		t.Fatal(err)
	}
	if attSt != "FINALIZED" || intSt != "CONFIRMED" {
		t.Fatalf("attempt=%s intent=%s want FINALIZED/CONFIRMED", attSt, intSt)
	}
	// 业务行绝不被链上结果复活, 执行腿绝不二次记账.
	var pst string
	if err := db.QueryRow(ctx, `SELECT status FROM merchant_payouts WHERE id=$1`, payoutID).
		Scan(&pst); err != nil {
		t.Fatal(err)
	}
	if pst != "CANCELLED_UNLOCKED" {
		t.Fatalf("payout %s want CANCELLED_UNLOCKED (not resurrected)", pst)
	}
	var execLegs int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE reference_id LIKE 'payoutexec:%'`).
		Scan(&execLegs); err != nil {
		t.Fatal(err)
	}
	if execLegs != 0 {
		t.Fatalf("exec legs=%d want 0 — PAYOUT_LOCKED must not be double-debited", execLegs)
	}
	var completedEvts int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_outbox WHERE event_type='PAYOUT_COMPLETED'`).
		Scan(&completedEvts); err != nil {
		t.Fatal(err)
	}
	if completedEvts != 0 {
		t.Fatalf("PAYOUT_COMPLETED outbox=%d want 0", completedEvts)
	}
	var saw bool
	for _, a := range alerts {
		if a.Code == "PAYOUT_EXEC_AFTER_CANCEL" && a.Severity == "CRITICAL" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("missing CRITICAL PAYOUT_EXEC_AFTER_CANCEL alert, got %+v", alerts)
	}
}

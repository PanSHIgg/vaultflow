package sweeper

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

func testDSN() string {
	if d := os.Getenv("VF_TEST_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("VF_TEST_PG_DSN"); d != "" {
		return d
	}
	return "postgres://postgres:postgres@localhost:55432/vaultflow?sslmode=disable"
}

func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db, err := pgxpool.New(context.Background(), testDSN())
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(context.Background(), `TRUNCATE
		chain_sync_cursors, supported_assets, merchants,
		merchant_withdrawal_addresses, deposit_addresses, payment_orders,
		chain_blocks, onchain_deposits, wallet_nonce_cursors,
		outbound_tx_intents, outbound_tx_attempts, sweep_batches,
		sweep_batch_items, merchant_payouts, webhook_outbox, signer_audit_anchor,
		ledger_accounts, ledger_transactions, ledger_entries
		RESTART IDENTITY CASCADE`); err != nil {
		t.Skipf("truncate: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
		 VALUES ('00000000-0000-0000-0000-000000000000','__SYSTEM__','__SYSTEM_SENTINEL_NO_API_KEY__',0) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	return db
}

var (
	testChain   = types.ChainID(31337)
	usdc        = types.HexToAddress("0x2222222222222222222222222222222222222222")
	relayer     = types.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	payoutW     = types.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cold        = types.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	sweeperAddr = types.HexToAddress("0xdddddddddddddddddddddddddddddddddddddddd")
)

// fakeTxM 记录 QueueIntentRequest 的 TxManager (err 可注入失败 — B2 reaper).
type fakeTxM struct {
	reqs []ports.QueueIntentRequest
	seq  int
	err  error
}

func (f *fakeTxM) QueueIntent(ctx context.Context, r ports.QueueIntentRequest) (types.OutboundTxIntent, error) {
	if f.err != nil {
		return types.OutboundTxIntent{}, f.err
	}
	f.reqs = append(f.reqs, r)
	f.seq++
	return types.OutboundTxIntent{ID: uuid.New(), IntentType: r.IntentType,
		ReferenceID: r.ReferenceID, ChainID: r.ChainID,
		WalletAddress: r.WalletAddress, Nonce: uint64(f.seq - 1), Status: types.IntentQueued}, nil
}
func (f *fakeTxM) GetIntent(context.Context, uuid.UUID) (types.OutboundTxIntent, error) {
	return types.OutboundTxIntent{}, nil
}
func (f *fakeTxM) ListAttempts(context.Context, uuid.UUID) ([]types.OutboundTxAttempt, error) {
	return nil, nil
}
func (f *fakeTxM) RequestCancel(context.Context, uuid.UUID) error { return nil }
func (f *fakeTxM) NonceCursorStatus(context.Context, types.ChainID, types.Address) (ports.NonceCursorStatus, error) {
	return ports.NonceCursorStatus{}, nil
}

// fakeWater 水位 fake.
type fakeWater struct {
	erc20   *big.Int
	native  *big.Int
	callErr error
}

func (f *fakeWater) ERC20BalanceOf(ctx context.Context, token, owner types.Address) (*big.Int, error) {
	return f.erc20, f.callErr
}
func (f *fakeWater) NativeBalance(ctx context.Context, owner types.Address) (*big.Int, error) {
	return f.native, f.callErr
}

// seedVaultDeposits 造一个 vault + n 笔 FINALIZED USDC 充值.
func seedVaultDeposits(t *testing.T, db *pgxpool.Pool, amounts []int64, quarantined bool) (uuid.UUID, types.Address, []byte) {
	daID := uuid.New()
	vault := common.BytesToAddress(crypto.Keccak256(daID[:])[12:])
	salt := crypto.Keccak256(daID[:])
	if _, err := db.Exec(context.Background(),
		`INSERT INTO deposit_addresses (id, chain_id, address, create2_salt, is_quarantined)
		 VALUES ($1,$2,$3,$4,$5)`, daID, int64(testChain), addrStr(vault), salt, quarantined); err != nil {
		t.Fatal(err)
	}
	for i, amt := range amounts {
		txh := crypto.Keccak256Hash([]byte(fmt.Sprintf("tx-%s-%d", daID, i)))
		if _, err := db.Exec(context.Background(),
			`INSERT INTO onchain_deposits
			   (chain_id, block_hash, tx_hash, log_index, block_number,
			    token_contract, from_address, to_address, amount, deposit_category, status)
			 VALUES ($1,$2,$3,0,100,$4,$5,$6,$7,'ORDER_MATCH','FINALIZED')`,
			int64(testChain), "0xbb", txh.Hex(),
			addrStr(usdc), "0x9999999999999999999999999999999999999999",
			addrStr(vault), amt); err != nil {
			t.Fatal(err)
		}
	}
	return daID, vault, salt
}

func seedBase(t *testing.T, db *pgxpool.Pool) {
	if _, err := db.Exec(context.Background(),
		`INSERT INTO supported_assets (chain_id, asset_symbol, token_contract, decimals)
		 VALUES ($1,'USDC',$2,6)`, int64(testChain), addrStr(usdc)); err != nil {
		t.Fatal(err)
	}
}

func TestAggregateAndCreateBatch(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedBase(t, db)

	_, v1, salt1 := seedVaultDeposits(t, db, []int64{1000, 500}, false) // 1500
	_, v2, salt2 := seedVaultDeposits(t, db, []int64{700}, false)       // 700
	seedVaultDeposits(t, db, []int64{9999}, true)                       // 隔离剔除
	daP, _, _ := seedVaultDeposits(t, db, []int64{300, 200}, false)     // 500-400在途占用=100
	// 在途 PENDING 占用 400.
	pb := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'IN_FLIGHT')`, pb, int64(testChain), addrStr(cold)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (batch_id, deposit_address_id, exact_sweep_amount, item_status)
		 VALUES ($1,$2,400,'PENDING')`, pb, daP); err != nil {
		t.Fatal(err)
	}
	// 已归集历史 250 -> v2 净 700-250=450.
	done := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'COMPLETED')`, done, int64(testChain), addrStr(cold)); err != nil {
		t.Fatal(err)
	}
	var da2 uuid.UUID
	if err := db.QueryRow(ctx,
		`SELECT id FROM deposit_addresses WHERE address=$1`, addrStr(v2)).Scan(&da2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (batch_id, deposit_address_id, exact_sweep_amount, actual_swept_amount, item_status)
		 VALUES ($1,$2,250,250,'SWEPT')`, done, da2); err != nil {
		t.Fatal(err)
	}

	ftxm := &fakeTxM{}
	sw := New(db, ftxm, nil, nil, Config{
		ChainID: testChain, BatchSweeper: sweeperAddr, RelayerHot: relayer,
		PayoutHot: payoutW, ColdWallet: cold,
		MaxFeePerGas:   types.U256FromBig(big.NewInt(2e9)),
		MaxPriorityFee: types.U256FromBig(big.NewInt(1e9)),
	}, nil, nil)

	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ftxm.reqs) != 1 {
		t.Fatalf("want 1 queued intent, got %d", len(ftxm.reqs))
	}
	req := ftxm.reqs[0]
	if req.IntentType != types.IntentTypeSweepBatch || req.To != sweeperAddr ||
		req.WalletAddress != relayer {
		t.Fatalf("intent routing %+v", req)
	}
	if req.GasLimit != 80_000+120_000*3 {
		t.Fatalf("gas %d want 440000", req.GasLimit)
	}
	// 三项条目: v1=1500, v2=450, daP=100.
	var n int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM sweep_batch_items WHERE batch_id<>$1 AND batch_id<>$2`,
		pb, done).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("batch items=%d want 3", n)
	}
	var batchID uuid.UUID
	if err := db.QueryRow(ctx,
		`SELECT batch_id FROM sweep_batch_items WHERE batch_id<>$1 AND batch_id<>$2 LIMIT 1`,
		pb, done).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	// 精确额逐项核对.
	type iv struct {
		v types.Address
		a int64
	}
	var got []iv
	rows, err := db.Query(ctx,
		`SELECT da.address, i.exact_sweep_amount::text
		   FROM sweep_batch_items i JOIN deposit_addresses da ON da.id=i.deposit_address_id
		  WHERE i.batch_id=$1 ORDER BY da.address`, batchID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, amtStr string
		var g iv
		if err := rows.Scan(&a, &amtStr); err != nil {
			t.Fatal(err)
		}
		g.a, _ = strconv.ParseInt(amtStr, 10, 64)
		g.v = types.HexToAddress(a)
		got = append(got, g)
	}
	rows.Close()
	exp := map[string]int64{addrStr(v1): 1500, addrStr(v2): 450}
	var pdaS string
	if err := db.QueryRow(ctx,
		`SELECT address FROM deposit_addresses WHERE id=$1`, daP).Scan(&pdaS); err != nil {
		t.Fatal(err)
	}
	exp[pdaS] = 100
	for _, g := range got {
		if exp[addrStr(g.v)] != g.a {
			t.Fatalf("item %s amount %d want %d", g.v, g.a, exp[addrStr(g.v)])
		}
	}
	// 批次进入 IN_FLIGHT.
	var bst string
	if err := db.QueryRow(ctx, `SELECT status FROM sweep_batches WHERE id=$1`, batchID).Scan(&bst); err != nil || bst != "IN_FLIGHT" {
		t.Fatalf("batch status %s", bst)
	}
	// calldata 可解出 3 个 SweepItem (selector 校验).
	sel := crypto.Keccak256([]byte("deployAndSweepBatch((bytes32,uint256)[],address,address)"))[:4]
	for i := range sel {
		if req.Calldata[i] != sel[i] {
			t.Fatal("calldata selector mismatch")
		}
	}
	_ = salt1
	_ = salt2
}

func TestWaterLowAlert(t *testing.T) {
	db := freshDB(t)
	seedBase(t, db)
	var alerts []Alert
	fw := &fakeWater{erc20: big.NewInt(50), native: big.NewInt(1e15)}
	sw := New(db, &fakeTxM{}, fw, nil, Config{
		ChainID: testChain, RelayerHot: relayer, PayoutHot: payoutW,
		LowWaterToken:  map[string]*big.Int{"USDC": big.NewInt(1000)},
		LowWaterNative: big.NewInt(1e18),
	}, nil, func(a Alert) { alerts = append(alerts, a) })
	if err := sw.WaterOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var tok, nat bool
	for _, a := range alerts {
		if a.Code == "LOW_TOKEN_WATER" {
			tok = true
		}
		if a.Code == "LOW_NATIVE_WATER" {
			nat = true
		}
	}
	if !tok || !nat {
		t.Fatalf("alerts %+v", alerts)
	}
}

var _ = chain.SweepItem{} // anchor
var _ = time.Now          // anchor

// ---------------------------------------------------------------------------
// FIX-B B2 回归测试: 批次泄漏回收 (reaper / RELEASED / 余额恢复)
// ---------------------------------------------------------------------------

func newTestSweeper(db *pgxpool.Pool, txm ports.TxManager,
	extra func(*Config), alertf AlertFunc) *Sweeper {
	cfg := Config{
		ChainID: testChain, BatchSweeper: sweeperAddr, RelayerHot: relayer,
		PayoutHot: payoutW, ColdWallet: cold,
		MaxFeePerGas:   types.U256FromBig(big.NewInt(2e9)),
		MaxPriorityFee: types.U256FromBig(big.NewInt(1e9)),
	}
	if extra != nil {
		extra(&cfg)
	}
	return New(db, txm, nil, nil, cfg, nil, alertf)
}

// B2-②: IN_FLIGHT 批次的关联意图进失败终态 (链上 Revert / 取消 / 拒签) →
// reaper 整批 FAILED + items RELEASED, 聚合余额立即恢复并重新建批.
func TestReconcileReleasesFailedIntentBatch(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedBase(t, db)
	daID, _, _ := seedVaultDeposits(t, db, []int64{1000}, false)

	pb := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'IN_FLIGHT')`, pb, int64(testChain), addrStr(cold)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (batch_id, deposit_address_id, exact_sweep_amount, item_status)
		 VALUES ($1,$2,1000,'PENDING')`, pb, daID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_intents (intent_type, reference_id, chain_id, wallet_address, nonce, status)
		 VALUES ('SWEEP_BATCH',$1,$2,$3,0,'FAILED_REVERTED')`,
		pb, int64(testChain), addrStr(relayer)); err != nil {
		t.Fatal(err)
	}

	ftxm := &fakeTxM{}
	var alerts []Alert
	sw := newTestSweeper(db, ftxm, nil, func(a Alert) { alerts = append(alerts, a) })
	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var bst, ist string
	if err := db.QueryRow(ctx, `SELECT status FROM sweep_batches WHERE id=$1`, pb).Scan(&bst); err != nil {
		t.Fatal(err)
	}
	if bst != "FAILED" {
		t.Fatalf("batch status=%s want FAILED", bst)
	}
	if err := db.QueryRow(ctx,
		`SELECT item_status FROM sweep_batch_items WHERE batch_id=$1`, pb).Scan(&ist); err != nil {
		t.Fatal(err)
	}
	if ist != "RELEASED" {
		t.Fatalf("item_status=%s want RELEASED", ist)
	}
	var alerted bool
	for _, a := range alerts {
		if a.Code == "SWEEP_BATCH_RELEASED" {
			alerted = true
		}
	}
	if !alerted {
		t.Fatalf("missing SWEEP_BATCH_RELEASED alert: %+v", alerts)
	}
	// vault 占用解除 → 同一轮聚合已把 1000 重新建批 (新 batch, 非 pb).
	if len(ftxm.reqs) != 1 {
		t.Fatalf("vault not re-swept: queued=%d", len(ftxm.reqs))
	}
	if ftxm.reqs[0].ReferenceID == pb {
		t.Fatal("re-sweep reused leaked batch id")
	}
	var amt string
	if err := db.QueryRow(ctx,
		`SELECT i.exact_sweep_amount::text FROM sweep_batch_items i
		  WHERE i.batch_id=$1 AND i.item_status='PENDING'`, ftxm.reqs[0].ReferenceID).Scan(&amt); err != nil {
		t.Fatal(err)
	}
	if amt != "1000" {
		t.Fatalf("re-swept amount=%s want 1000", amt)
	}
}

// B2-③: PENDING 批次 (QueueIntent 曾失败, 无关联意图) reaper — 窗口期内
// 重建载荷重试入队; 失败计次; 超时后 FAILED + RELEASED 释放占用.
func TestPendingBatchReaperRequeueFailAndTimeout(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedBase(t, db)
	da1, _, _ := seedVaultDeposits(t, db, []int64{500}, false)

	pb1 := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'PENDING')`, pb1, int64(testChain), addrStr(cold)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (batch_id, deposit_address_id, exact_sweep_amount, item_status)
		 VALUES ($1,$2,500,'PENDING')`, pb1, da1); err != nil {
		t.Fatal(err)
	}

	ftxm := &fakeTxM{}
	sw := newTestSweeper(db, ftxm, nil, nil)
	// 无意图 PENDING 批次 → reaper 重建 calldata 重试 QueueIntent → IN_FLIGHT.
	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ftxm.reqs) != 1 || ftxm.reqs[0].ReferenceID != pb1 {
		t.Fatalf("reaper did not requeue batch: %+v", ftxm.reqs)
	}
	var bst string
	if err := db.QueryRow(ctx, `SELECT status FROM sweep_batches WHERE id=$1`, pb1).Scan(&bst); err != nil {
		t.Fatal(err)
	}
	if bst != "IN_FLIGHT" {
		t.Fatalf("batch status=%s want IN_FLIGHT after requeue", bst)
	}
	// calldata 可解出 1 个 SweepItem.
	sel := crypto.Keccak256([]byte("deployAndSweepBatch((bytes32,uint256)[],address,address)"))[:4]
	for i := range sel {
		if ftxm.reqs[0].Calldata[i] != sel[i] {
			t.Fatal("requeued calldata selector mismatch")
		}
	}

	// QueueIntent 持续失败: 批次保持 PENDING + queue_attempts 递增.
	da2, _, _ := seedVaultDeposits(t, db, []int64{300}, false)
	pb2 := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'PENDING')`, pb2, int64(testChain), addrStr(cold)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (batch_id, deposit_address_id, exact_sweep_amount, item_status)
		 VALUES ($1,$2,300,'PENDING')`, pb2, da2); err != nil {
		t.Fatal(err)
	}
	ftxm.err = fmt.Errorf("nonce cursor unavailable")
	ftxm.reqs = nil
	sw2 := newTestSweeper(db, ftxm, func(c *Config) { c.PendingBatchTimeout = time.Hour }, nil)
	if err := sw2.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var qa int
	if err := db.QueryRow(ctx,
		`SELECT status, queue_attempts FROM sweep_batches WHERE id=$1`, pb2).
		Scan(&bst, &qa); err != nil {
		t.Fatal(err)
	}
	if bst != "PENDING" || qa < 1 {
		t.Fatalf("batch status=%s attempts=%d — failed requeue must stay PENDING with counter", bst, qa)
	}
	// PENDING 占用仍在: vault 不重复建批 (fakeTxM.reqs 仍空 — 无新建批成功入队).
	if len(ftxm.reqs) != 0 {
		t.Fatalf("double-counted vaults into new batch: %+v", ftxm.reqs)
	}

	// 超时 → FAILED + RELEASED; vault 余额同轮恢复 → fakeTxM 恢复后立即重新建批.
	if _, err := db.Exec(ctx,
		`UPDATE sweep_batches SET created_at=now()-interval '2 hours' WHERE id=$1`, pb2); err != nil {
		t.Fatal(err)
	}
	ftxm.err = nil
	ftxm.reqs = nil
	var alerts []Alert
	sw3 := newTestSweeper(db, ftxm, func(c *Config) { c.PendingBatchTimeout = time.Hour },
		func(a Alert) { alerts = append(alerts, a) })
	if err := sw3.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var ist string
	if err := db.QueryRow(ctx,
		`SELECT status FROM sweep_batches WHERE id=$1`, pb2).Scan(&bst); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		`SELECT item_status FROM sweep_batch_items WHERE batch_id=$1`, pb2).Scan(&ist); err != nil {
		t.Fatal(err)
	}
	if bst != "FAILED" || ist != "RELEASED" {
		t.Fatalf("timeout batch status=%s item=%s want FAILED/RELEASED", bst, ist)
	}
	var crit bool
	for _, a := range alerts {
		if a.Code == "SWEEP_BATCH_RELEASED" && a.Severity == "CRITICAL" {
			crit = true
		}
	}
	if !crit {
		t.Fatalf("missing CRITICAL release alert: %+v", alerts)
	}
	// 释放后 vault 已在本轮重新建批 (新 batch_id 包含 da2).
	var reswept bool
	for _, r := range ftxm.reqs {
		var cnt int
		if err := db.QueryRow(ctx,
			`SELECT COUNT(*) FROM sweep_batch_items WHERE batch_id=$1 AND deposit_address_id=$2`,
			r.ReferenceID, da2).Scan(&cnt); err == nil && cnt == 1 {
			reswept = true
		}
	}
	if !reswept {
		t.Fatalf("released vault not re-swept: %+v", ftxm.reqs)
	}
}

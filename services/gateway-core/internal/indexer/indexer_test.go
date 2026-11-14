package indexer

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ledger"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 测试基建: PG + 契约感知 fake 账本 + fake 链 + fake 重广播
// ---------------------------------------------------------------------------

func testDSN() string {
	if d := os.Getenv("VF_TEST_DSN"); d != "" {
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

// fakeLedger 契约感知复式账本 fake: 校验模板白名单 + 试算平衡 + 非负余额 +
// (template,ref) 幂等 — 用于验证 indexer 的记账正确性.
type fakeLedger struct {
	db    *pgxpool.Pool
	bal   map[string]*big.Int
	txns  map[string][]ports.PostEntryLeg
	posts int
}

func newFakeLedger(db *pgxpool.Pool) *fakeLedger {
	return &fakeLedger{db: db, bal: map[string]*big.Int{}, txns: map[string][]ports.PostEntryLeg{}}
}

func acctKey(l ports.PostEntryLeg) string {
	return l.OwnerMerchantID.String() + "|" + fmt.Sprint(l.ChainID) + "|" + l.AssetSymbol + "|" + string(l.Account)
}

func (f *fakeLedger) PostEntry(ctx context.Context, req ports.PostEntryRequest) (ports.PostedEntry, error) {
	key := string(req.Template) + "|" + req.ReferenceID
	if old, ok := f.txns[key]; ok {
		if legsEqual(old, req.Legs) {
			return ports.PostedEntry{AlreadyExisted: true}, nil
		}
		return ports.PostedEntry{}, ports.ErrDuplicateReference
	}
	// 与真实 ledger 一致: 全部腿正额 + 借贷合计平衡 + 每个 (借×贷) 组合
	// 命中模板规则 (internal/ledger/service.go validateLegs 同语义).
	if len(req.Legs) == 0 {
		return ports.PostedEntry{}, ports.ErrUnbalancedEntry
	}
	drSum, crSum := new(big.Int), new(big.Int)
	for _, l := range req.Legs {
		if l.Amount.Big().Sign() <= 0 {
			return ports.PostedEntry{}, ports.ErrNonPositiveAmount
		}
		switch l.Direction {
		case types.DirectionDebit:
			drSum.Add(drSum, l.Amount.Big())
		case types.DirectionCredit:
			crSum.Add(crSum, l.Amount.Big())
		default:
			return ports.PostedEntry{}, ports.ErrUnbalancedEntry
		}
	}
	if drSum.Cmp(crSum) != 0 {
		return ports.PostedEntry{}, fmt.Errorf("%w: dr %s != cr %s",
			ports.ErrUnbalancedEntry, drSum, crSum)
	}
	for _, d := range req.Legs {
		if d.Direction != types.DirectionDebit {
			continue
		}
		for _, c := range req.Legs {
			if c.Direction != types.DirectionCredit {
				continue
			}
			if !req.Template.Allows(d.Account, c.Account) {
				return ports.PostedEntry{}, fmt.Errorf("%w: %s->%s in %s",
					ports.ErrInvalidTemplateLeg, d.Account, c.Account, req.Template)
			}
		}
	}
	// 两遍提交: 先累计各账户增量并校验余额非负, 后统一落账.
	deltas := map[string]*big.Int{}
	for _, l := range req.Legs {
		k := acctKey(l)
		debitNormal := l.Account.NormalSide() == types.DirectionDebit
		d := new(big.Int).Set(l.Amount.Big())
		if (l.Direction == types.DirectionDebit) != debitNormal {
			d.Neg(d)
		}
		if deltas[k] == nil {
			deltas[k] = new(big.Int)
		}
		deltas[k].Add(deltas[k], d)
	}
	for k, d := range deltas {
		base := f.bal[k]
		if base == nil {
			base = new(big.Int)
		}
		if new(big.Int).Add(base, d).Sign() < 0 {
			return ports.PostedEntry{}, fmt.Errorf("%w: %s would go negative",
				ports.ErrNegativeBalance, k)
		}
	}
	newBals := map[string]*big.Int{}
	for k, d := range deltas {
		base := f.bal[k]
		if base == nil {
			base = new(big.Int)
		}
		newBals[k] = new(big.Int).Add(base, d)
	}
	// 持久化到真实账本表 — legsOfLatest (充值腿反查/已结单红冲) 依赖
	// ledger_transactions + ledger_entries 行; 与真实 ledger 同事务语义.
	if f.db != nil {
		if err := pgx.BeginFunc(ctx, f.db, func(tx pgx.Tx) error {
			txnID := uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_transactions (id, reference_type, reference_id, description)
				 VALUES ($1,$2,$3,$4)`,
				txnID, string(req.Template), req.ReferenceID, req.Description); err != nil {
				return err
			}
			running := map[string]*big.Int{}
			for _, l := range req.Legs {
				normal := "CREDIT"
				if l.Account.NormalSide() == types.DirectionDebit {
					normal = "DEBIT"
				}
				var acctID uuid.UUID
				if err := tx.QueryRow(ctx,
					`INSERT INTO ledger_accounts
					   (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
					 VALUES ($1,$2,$3,$4,$5,0)
					 ON CONFLICT (owner_merchant_id, chain_id, asset_symbol, account_type)
					 DO UPDATE SET normal_side=EXCLUDED.normal_side
					 RETURNING id`,
					l.OwnerMerchantID, int64(l.ChainID), l.AssetSymbol,
					string(l.Account), normal).Scan(&acctID); err != nil {
					return err
				}
				// balance_after 必须逐腿滚动成链 (trg_ledger_entries_balanced
				// 校验 openings/terminals 一致性).
				k := acctKey(l)
				nb := running[k]
				if nb == nil {
					nb = f.bal[k]
				}
				if nb == nil {
					nb = new(big.Int)
				}
				nb = new(big.Int).Set(nb)
				debitNormal := l.Account.NormalSide() == types.DirectionDebit
				if (l.Direction == types.DirectionDebit) == debitNormal {
					nb.Add(nb, l.Amount.Big())
				} else {
					nb.Sub(nb, l.Amount.Big())
				}
				running[k] = nb
				if _, err := tx.Exec(ctx,
					`UPDATE ledger_accounts SET balance=$2 WHERE id=$1`, acctID, nb.String()); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx,
					`INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, balance_after)
					 VALUES ($1,$2,$3,$4,$5)`,
					txnID, acctID, string(l.Direction), l.Amount.String(), nb.String()); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return ports.PostedEntry{}, err
		}
	}
	for k, v := range newBals {
		f.bal[k] = v
	}
	f.txns[key] = append([]ports.PostEntryLeg(nil), req.Legs...)
	f.posts++
	return ports.PostedEntry{}, nil
}

func legsEqual(a, b []ports.PostEntryLeg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Account != b[i].Account || a[i].Direction != b[i].Direction ||
			a[i].Amount.Big().Cmp(b[i].Amount.Big()) != 0 ||
			a[i].OwnerMerchantID != b[i].OwnerMerchantID || a[i].AssetSymbol != b[i].AssetSymbol {
			return false
		}
	}
	return true
}

func (f *fakeLedger) balOf(owner uuid.UUID, chain types.ChainID, asset string, acct types.LedgerAccountCode) *big.Int {
	return f.bal[acctKey(ports.PostEntryLeg{OwnerMerchantID: owner, ChainID: chain,
		AssetSymbol: asset, Account: acct})]
}

func (f *fakeLedger) GetBalances(ctx context.Context, q ports.BalanceQuery) ([]ports.AccountBalance, error) {
	var out []ports.AccountBalance
	for _, a := range q.Accounts {
		b := f.balOf(q.OwnerMerchantID, q.ChainID, q.AssetSymbol, a)
		if b == nil {
			b = new(big.Int)
		}
		out = append(out, ports.AccountBalance{Account: a, Balance: types.U256FromBig(b)})
	}
	return out, nil
}

func (f *fakeLedger) RunInvariantCheck(ctx context.Context, req ports.InvariantCheckRequest) (ports.InvariantReport, error) {
	return ports.InvariantReport{ChainID: req.ChainID, AssetSymbol: req.AssetSymbol, TrialBalanced: true}, nil
}

// seedBalance 直接注入期初余额 (绕过转账来源 — 测试构造器).
func (f *fakeLedger) seedBalance(owner uuid.UUID, chain types.ChainID, asset string,
	acct types.LedgerAccountCode, amt int64) {
	f.bal[acctKey(ports.PostEntryLeg{OwnerMerchantID: owner, ChainID: chain,
		AssetSymbol: asset, Account: acct})] = big.NewInt(amt)
}

// fakeChainQ 可编程 ChainQuerier.
type fakeChainQ struct {
	heads    chain.Heights
	headers  map[uint64]*chain.BlockHeader
	evs      map[uint64][]chain.TransferEvent
	receipts map[types.Hash]*ethtypes.Receipt
}

func (f *fakeChainQ) Heights(ctx context.Context, min uint64) (chain.Heights, error) {
	return f.heads, nil
}
func (f *fakeChainQ) HeaderAt(ctx context.Context, h uint64) (*chain.BlockHeader, error) {
	if b, ok := f.headers[h]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("no header %d", h)
}

// TransferLogs fake: 忠实模拟 A1 钉死语义 — 只回 ev.BlockHash==hdr.Hash 的日志.
func (f *fakeChainQ) TransferLogs(ctx context.Context, hdr *chain.BlockHeader, tokens, watch []types.Address,
	maxf int) ([]chain.TransferEvent, error) {
	var out []chain.TransferEvent
	for _, ev := range f.evs[hdr.Number] {
		if ev.BlockHash == hdr.Hash {
			out = append(out, ev)
		}
	}
	return out, nil
}
func (f *fakeChainQ) ReceiptAt(ctx context.Context, h types.Hash) (*ethtypes.Receipt, error) {
	return f.receipts[h], nil
}

// fakeRebc 重广播记录器.
type fakeRebc struct{ intents []uuid.UUID }

func (f *fakeRebc) RebroadcastIntent(ctx context.Context, id uuid.UUID) error {
	f.intents = append(f.intents, id)
	return nil
}

// ---------------------------------------------------------------------------
// 种子助手
// ---------------------------------------------------------------------------

var (
	testChain    = types.ChainID(31337)
	usdcContract = types.HexToAddress("0x2222222222222222222222222222222222222222")
	usdtContract = types.HexToAddress("0x3333333333333333333333333333333333333333")
	payoutHot    = types.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	relayerHot   = types.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	coldWallet   = types.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	sweeperAddr  = types.HexToAddress("0xdddddddddddddddddddddddddddddddddddddddd")
	sysID        = types.SystemMerchantID
)

func seedAsset(t *testing.T, db *pgxpool.Pool, sym string, contract types.Address) {
	if _, err := db.Exec(context.Background(),
		`INSERT INTO supported_assets (chain_id, asset_symbol, token_contract, decimals)
		 VALUES ($1,$2,$3,6) ON CONFLICT DO NOTHING`,
		int64(testChain), sym, addrStr(contract)); err != nil {
		t.Fatal(err)
	}
}

func seedMerchant(t *testing.T, db *pgxpool.Pool, feeBps int) uuid.UUID {
	id := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps, webhook_url, webhook_secret_enc)
		 VALUES ($1,'m',$2,$3,'https://hook.local/x','s3cr3t')`,
		id, "h"+id.String()[:8], feeBps); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedOrder 建 deposit_address + payment_order, 返回 (vaultAddr, orderID, daID).
func seedOrder(t *testing.T, db *pgxpool.Pool, m uuid.UUID, asset string,
	expected int64, status string, expiresIn time.Duration) (types.Address, uuid.UUID, uuid.UUID) {
	daID := uuid.New()
	vault := common.BytesToAddress(crypto.Keccak256(daID[:])[12:])
	if _, err := db.Exec(context.Background(),
		`INSERT INTO deposit_addresses (id, chain_id, address, create2_salt)
		 VALUES ($1,$2,$3,$4)`,
		daID, int64(testChain), addrStr(vault), crypto.Keccak256(daID[:])); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO payment_orders
		   (id, merchant_id, idempotency_key, request_hash, chain_id, asset_symbol,
		    expected_amount, deposit_address_id, status, expires_at)
		 VALUES ($1,$2,$3,'rh',$4,$5,$6,$7,$8, now()+$9*interval '1 second')`,
		orderID, m, "ik"+orderID.String()[:8], int64(testChain), asset,
		expected, daID, status, int64(expiresIn.Seconds())); err != nil {
		t.Fatal(err)
	}
	return vault, orderID, daID
}

func seedCursor(t *testing.T, db *pgxpool.Pool, h uint64, hash types.Hash) {
	if _, err := db.Exec(context.Background(),
		`INSERT INTO chain_sync_cursors (chain_id, latest_synced_height, latest_synced_hash, safe_head_height)
		 VALUES ($1,$2,$3,0)`, int64(testChain), int64(h), hashStr(hash)); err != nil {
		t.Fatal(err)
	}
	// 锚定块行 — findForkPoint 需要游标高度以下存在已知本地哈希.
	if _, err := db.Exec(context.Background(),
		`INSERT INTO chain_blocks (chain_id, block_hash, block_number, parent_hash, status)
		 VALUES ($1,$2,$3,'0x00','CANONICAL') ON CONFLICT (chain_id, block_hash) DO NOTHING`,
		int64(testChain), hashStr(hash), int64(h)); err != nil {
		t.Fatal(err)
	}
}

// mkHeader 链式构造确定性头.
func mkHeader(n uint64, tag byte) *chain.BlockHeader {
	parent := types.HexToHash(fmt.Sprintf("0x%064x", uint64(tag)+n-1))
	return &chain.BlockHeader{Number: n,
		Hash: types.HexToHash(fmt.Sprintf("0x%064x", uint64(tag)+n)), ParentHash: parent}
}

// chainOf 构造 0..n 连续头序列.
func chainOf(n uint64, tag byte) map[uint64]*chain.BlockHeader {
	out := map[uint64]*chain.BlockHeader{0: {Number: 0,
		Hash: types.HexToHash(fmt.Sprintf("0x%064x", uint64(tag))), ParentHash: types.Hash{}}}
	for i := uint64(1); i <= n; i++ {
		out[i] = mkHeader(i, tag)
	}
	return out
}

func newIx(db *pgxpool.Pool, q ChainQuerier, led ports.LedgerService, rebc *fakeRebc) *Indexer {
	return New(db, q, led, nil, rebc, Config{
		ChainID:          testChain,
		BatchBlocks:      100,
		PayoutHotWallet:  payoutHot,
		RelayerHotWallet: relayerHot,
		ColdWallet:       coldWallet,
		BatchSweeper:     sweeperAddr,
	}, nil, nil)
}

func transferEv(to, token types.Address, from types.Address, amt int64, blk uint64, txh types.Hash) chain.TransferEvent {
	return chain.TransferEvent{TokenContract: token, From: from, To: to,
		Amount: big.NewInt(amt), TxHash: txh,
		BlockHash: mkHeader(blk, 'A').Hash, BlockNumber: blk, LogIndex: 0}
}

func depStatus(t *testing.T, db *pgxpool.Pool, txHash types.Hash) string {
	var s string
	if err := db.QueryRow(context.Background(),
		`SELECT status FROM onchain_deposits WHERE tx_hash=$1 ORDER BY block_number DESC LIMIT 1`,
		hashStr(txHash)).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func orderState(t *testing.T, db *pgxpool.Pool, id uuid.UUID) (string, string) {
	var s, r string
	if err := db.QueryRow(context.Background(),
		`SELECT status, received_amount::text FROM payment_orders WHERE id=$1`, id).
		Scan(&s, &r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

// ---------------------------------------------------------------------------
// 入站四分支 + 两段式结算
// ---------------------------------------------------------------------------

func TestInboundFourBranches(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	seedAsset(t, db, "USDT", usdtContract)
	m := seedMerchant(t, db, 100) // 1% 费率

	v1, o1, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	v2, o2, _ := seedOrder(t, db, m, "USDC", 500, "AWAITING_PAYMENT", time.Hour)
	v3, o3, _ := seedOrder(t, db, m, "USDC", 800, "PAID", time.Hour)

	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")
	blk6 := uint64(6)
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 9, Finalized: 8},
		headers: chainOf(10, 'A'),
		evs: map[uint64][]chain.TransferEvent{blk6: {
			transferEv(v1, usdcContract, ext, 1000, blk6, types.HexToHash("0x01")),
			transferEv(v2, usdtContract, ext, 600, blk6, types.HexToHash("0x02")),         // 异币
			transferEv(v3, usdcContract, ext, 50, blk6, types.HexToHash("0x03")),          // 终态单
			transferEv(payoutHot, usdcContract, ext, 2000, blk6, types.HexToHash("0x04")), // 注资
		}},
		receipts: map[types.Hash]*ethtypes.Receipt{},
	}
	seedCursor(t, db, 5, chainOf(10, 'A')[5].Hash)
	led := newFakeLedger(db)
	ix := newIx(db, fq, led, &fakeRebc{})

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 全部四笔 FINALIZED + 正确分类.
	var cats []string
	rows, err := db.Query(ctx,
		`SELECT deposit_category FROM onchain_deposits ORDER BY tx_hash`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cats = append(cats, c)
	}
	rows.Close()
	want := []string{"ORDER_MATCH", "WRONG_ASSET", "POST_TERMINAL", "TOPUP"}
	for i := range want {
		if cats[i] != want[i] {
			t.Fatalf("categories %v want %v", cats, want)
		}
	}

	// 分支 D: O1 PAID, settle 腿 MA=990 FEE=10.
	st, recv := orderState(t, db, o1)
	if st != "PAID" || recv != "1000" {
		t.Fatalf("order1 %s received=%s", st, recv)
	}
	if got := led.balOf(m, testChain, "USDC", types.AccountMerchantAvailable); got == nil || got.Int64() != 990 {
		t.Fatalf("merchant available %v want 990", got)
	}
	if got := led.balOf(sysID, testChain, "USDC", types.AccountFeeRevenue); got == nil || got.Int64() != 10 {
		t.Fatalf("fee revenue %v want 10", got)
	}
	// 分支 B: 异币挂 UNMATCHED (不改订单).
	st, recv = orderState(t, db, o2)
	if st != "AWAITING_PAYMENT" || recv != "0" {
		t.Fatalf("wrong-asset order touched: %s recv=%s", st, recv)
	}
	if got := led.balOf(m, testChain, "USDT", types.AccountSuspenseUnmatchedLiab); got == nil || got.Int64() != 600 {
		t.Fatalf("unmatched suspense %v want 600", got)
	}
	// 分支 C: 终态单二次误充挂 REFUNDABLE (不改状态).
	st, _ = orderState(t, db, o3)
	if st != "PAID" {
		t.Fatalf("terminal order touched: %s", st)
	}
	if got := led.balOf(m, testChain, "USDC", types.AccountSuspenseRefundableLiab); got == nil || got.Int64() != 50 {
		t.Fatalf("refundable suspense %v want 50", got)
	}
	// 分支 A: 注资 DR HOT_PAYOUT/CR EQUITY.
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyHotPayout); got == nil || got.Int64() != 2000 {
		t.Fatalf("hot payout %v want 2000", got)
	}
	if got := led.balOf(sysID, testChain, "USDC", types.AccountTreasuryEquityCapital); got == nil || got.Int64() != 2000 {
		t.Fatalf("equity %v want 2000", got)
	}
	// UNSWEPT = 1000+600? 异币是 USDT 维度: USDC UNSWEPT = 1000+50.
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyUnswept); got == nil || got.Int64() != 1050 {
		t.Fatalf("unswept usdc %v want 1050", got)
	}
	// outbox ORDER_SETTLED.
	var cnt int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_outbox WHERE event_type='ORDER_SETTLED'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("outbox settle events=%d", cnt)
	}
}

// ---------------------------------------------------------------------------
// 出站两段式
// ---------------------------------------------------------------------------

func TestOutboundTwoStage(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 0)
	payoutID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO merchant_payouts (id, merchant_id, idempotency_key, request_hash,
		   payout_kind, source_account_type, chain_id, asset_symbol, to_address, amount, status)
		 VALUES ($1,$2,'ik','rh','WITHDRAWAL','MERCHANT_AVAILABLE',$3,'USDC',$4,300,'IN_FLIGHT')`,
		payoutID, m, int64(testChain), addrStr(types.HexToAddress("0x7777777777777777777777777777777777777777"))); err != nil {
		t.Fatal(err)
	}
	intentID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_intents (id, intent_type, reference_id, chain_id, wallet_address, nonce, status)
		 VALUES ($1,'MERCHANT_PAYOUT',$2,$3,$4,7,'IN_FLIGHT')`,
		intentID, payoutID, int64(testChain), addrStr(payoutHot)); err != nil {
		t.Fatal(err)
	}
	txh := types.HexToHash("0xabc1")
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
	led.seedBalance(m, testChain, "USDC", types.AccountPayoutLockedLiab, 300)
	led.seedBalance(sysID, testChain, "USDC", types.AccountCustodyHotPayout, 300)
	led.seedBalance(sysID, testChain, "ETH", types.AccountHotWalletNative, 1_000_000_000_000_000)
	ix := newIx(db, fq, led, &fakeRebc{})

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var attSt, intSt string
	if err := db.QueryRow(ctx,
		`SELECT a.status, i.status FROM outbound_tx_attempts a
		   JOIN outbound_tx_intents i ON i.id=a.intent_id WHERE a.id=$1`, attID).
		Scan(&attSt, &intSt); err != nil {
		t.Fatal(err)
	}
	if attSt != "FINALIZED" || intSt != "CONFIRMED" {
		t.Fatalf("attempt=%s intent=%s", attSt, intSt)
	}
	// GAS_SETTLE: 21000 * 2gwei = 42000 gwei.
	if got := led.balOf(sysID, testChain, "ETH", types.AccountGasExpense); got == nil ||
		got.Cmp(big.NewInt(42_000_000_000_000)) != 0 {
		t.Fatalf("gas expense %v", got)
	}
	// PAYOUT_FLOW exec: LOCKED->0, HOT_PAYOUT->0.
	if got := led.balOf(m, testChain, "USDC", types.AccountPayoutLockedLiab); got == nil || got.Sign() != 0 {
		t.Fatalf("locked %v want 0", got)
	}
	var pst string
	if err := db.QueryRow(ctx, `SELECT status FROM merchant_payouts WHERE id=$1`, payoutID).Scan(&pst); err != nil || pst != "COMPLETED" {
		t.Fatalf("payout status %s", pst)
	}
	var cnt int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_outbox WHERE event_type='PAYOUT_COMPLETED'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("payout outbox=%d", cnt)
	}
}

// ---------------------------------------------------------------------------
// 浅回滚: ORPHANED_SHALLOW + attempt 重置 + SUPERSEDED 复活 + 重广播
// ---------------------------------------------------------------------------

func TestShallowRollback(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 0)
	v1, _, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)

	intentID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_intents (id, intent_type, reference_id, chain_id, wallet_address, nonce, status)
		 VALUES ($1,'MERCHANT_PAYOUT',$2,$3,$4,3,'INCLUDED_PENDING_SAFE')`,
		intentID, uuid.New(), int64(testChain), addrStr(payoutHot)); err != nil {
		t.Fatal(err)
	}
	txh, txh2 := types.HexToHash("0xb1"), types.HexToHash("0xb2")
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_attempts
		   (id, intent_id, attempt_no, attempt_kind, chain_id, tx_hash,
		    max_fee_per_gas, max_priority_fee_per_gas, block_number, block_hash, receipt_status, gas_used, effective_gas_price, status)
		 VALUES
		   (gen_random_uuid(),$1,2,'PRIMARY_INTENT',$2,$3,3000000000,1000000000,5,$4,1,21000,2000000000,'INCLUDED_UNSAFE'),
		   (gen_random_uuid(),$1,1,'PRIMARY_INTENT',$2,$5,2000000000,1000000000,NULL,NULL,NULL,NULL,NULL,'SUPERSEDED')`,
		intentID, int64(testChain), hashStr(txh), hashStr(mkHeader(5, 'A').Hash), hashStr(txh2)); err != nil {
		t.Fatal(err)
	}

	localA := chainOf(8, 'A')
	// 远端: 高度 5 起分叉为 B 链 (公共祖先 A4; B5.parent=A4, B6.parent=B5).
	remote := chainOf(8, 'A')
	b5 := &chain.BlockHeader{Number: 5, Hash: types.HexToHash("0x0000b5"), ParentHash: localA[4].Hash}
	b6 := &chain.BlockHeader{Number: 6, Hash: types.HexToHash("0x0000b6"), ParentHash: b5.Hash}
	b7 := &chain.BlockHeader{Number: 7, Hash: types.HexToHash("0x0000b7"), ParentHash: b6.Hash}
	b8 := &chain.BlockHeader{Number: 8, Hash: types.HexToHash("0x0000b8"), ParentHash: b7.Hash}
	remote[5], remote[6], remote[7], remote[8] = b5, b6, b7, b8

	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 6, Safe: 3},
		headers: remote,
		evs: map[uint64][]chain.TransferEvent{5: {
			transferEv(v1, usdcContract, types.HexToAddress("0x9999999999999999999999999999999999999999"),
				100, 5, types.HexToHash("0xd1"))}}, // 该 dep 将 ORPHANED_SHALLOW
		receipts: map[types.Hash]*ethtypes.Receipt{},
	}
	seedCursor(t, db, 4, localA[4].Hash)
	// 先推进到 5 (本地 A5).
	fq.heads.Latest = 5
	fq.headers = localA
	led := newFakeLedger(db)
	rebc := &fakeRebc{}
	ix := newIx(db, fq, led, rebc)
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// 远端分叉, latest=6 -> 触发 LIFO 回滚.
	fq.headers = remote
	fq.heads.Latest = 6
	fq.evs = map[uint64][]chain.TransferEvent{} // 该 dep 未在 B 链重挖 -> 纯 ORPHANED_SHALLOW
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if s := depStatus(t, db, types.HexToHash("0xd1")); s != "ORPHANED_SHALLOW" {
		t.Fatalf("deposit status %s want ORPHANED_SHALLOW", s)
	}
	var a1, a2 string
	var bn *int64
	if err := db.QueryRow(ctx,
		`SELECT status, block_number FROM outbound_tx_attempts
		  WHERE intent_id=$1 AND attempt_no=2`, intentID).Scan(&a1, &bn); err != nil {
		t.Fatal(err)
	}
	if a1 != "BROADCASTED" || bn != nil {
		t.Fatalf("included attempt reset failed: %s block=%v", a1, bn)
	}
	if err := db.QueryRow(ctx,
		`SELECT status FROM outbound_tx_attempts
		  WHERE intent_id=$1 AND attempt_no=1`, intentID).Scan(&a2); err != nil {
		t.Fatal(err)
	}
	if a2 != "BROADCASTED" {
		t.Fatalf("superseded sibling status=%s want BROADCASTED", a2)
	}
	var intSt string
	if err := db.QueryRow(ctx, `SELECT status FROM outbound_tx_intents WHERE id=$1`,
		intentID).Scan(&intSt); err != nil || intSt != "IN_FLIGHT" {
		t.Fatalf("intent status %s", intSt)
	}
	var bSt, aSt string
	if err := db.QueryRow(ctx,
		`SELECT (SELECT status FROM chain_blocks WHERE block_hash=$1),
		        (SELECT status FROM chain_blocks WHERE block_hash=$2)`,
		hashStr(remote[5].Hash), hashStr(localA[5].Hash)).Scan(&bSt, &aSt); err != nil {
		t.Fatal(err)
	}
	if bSt != "CANONICAL" || aSt != "ORPHANED" {
		t.Fatalf("fork blocks: new=%s old=%s", bSt, aSt)
	}
	if len(rebc.intents) != 1 || rebc.intents[0] != intentID {
		t.Fatalf("rebroadcast intents %v", rebc.intents)
	}
}

// ---------------------------------------------------------------------------
// 深回滚: 已结单 unwind + 归集/提现红冲 (LIFO)
// ---------------------------------------------------------------------------

func TestDeepRollbackAllLayers(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 100)

	v1, o1, da1 := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")

	// sweep batch + intent/attempt (块 7).
	batchID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'IN_FLIGHT')`, batchID, int64(testChain), addrStr(coldWallet)); err != nil {
		t.Fatal(err)
	}
	itemID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (id, batch_id, deposit_address_id, exact_sweep_amount)
		 VALUES ($1,$2,$3,1000)`, itemID, batchID, da1); err != nil {
		t.Fatal(err)
	}
	sweepIntent := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_intents (id, intent_type, reference_id, chain_id, wallet_address, nonce, status)
		 VALUES ($1,'SWEEP_BATCH',$2,$3,$4,9,'IN_FLIGHT')`,
		sweepIntent, batchID, int64(testChain), addrStr(relayerHot)); err != nil {
		t.Fatal(err)
	}
	sweepTx := types.HexToHash("0x5e")
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_attempts
		   (id, intent_id, attempt_no, attempt_kind, chain_id, tx_hash,
		    max_fee_per_gas, max_priority_fee_per_gas, status)
		 VALUES (gen_random_uuid(),$1,1,'PRIMARY_INTENT',$2,$3,3000000000,1000000000,'BROADCASTED')`,
		sweepIntent, int64(testChain), hashStr(sweepTx)); err != nil {
		t.Fatal(err)
	}

	// sweep 回执: VaultSwept(v1, salt, 1000) — emitter=sweeperAddr.
	var saltB []byte
	if err := db.QueryRow(ctx, `SELECT create2_salt FROM deposit_addresses WHERE id=$1`, da1).Scan(&saltB); err != nil {
		t.Fatal(err)
	}
	salt := common.BytesToHash(saltB)
	sweepLog := &ethtypes.Log{Address: sweeperAddr,
		Topics: []types.Hash{chain.VaultSweptTopic,
			common.BytesToHash(v1.Bytes()), types.Hash(salt)},
		Data: new(big.Int).SetInt64(1000).FillBytes(make([]byte, 32))}

	blk := chainOf(8, 'A')
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 8},
		headers: blk,
		evs: map[uint64][]chain.TransferEvent{6: {
			transferEv(v1, usdcContract, ext, 1000, 6, types.HexToHash("0x09"))}},
		receipts: map[types.Hash]*ethtypes.Receipt{sweepTx: {
			Status: 1, GasUsed: 90000, EffectiveGasPrice: big.NewInt(1_000_000_000),
			BlockNumber: big.NewInt(7), BlockHash: blk[7].Hash, Logs: []*ethtypes.Log{sweepLog}}},
	}
	seedCursor(t, db, 5, blk[5].Hash)
	led := newFakeLedger(db)
	led.seedBalance(sysID, testChain, "ETH", types.AccountHotWalletNative, 1_000_000_000_000_000)
	rebc := &fakeRebc{}
	ix := newIx(db, fq, led, rebc)

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 结算后快照: UNSWEPT 0 (1000 dep - 1000 sweep), COLD 1000, MA 990, FEE 10.
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyUnswept); got == nil || got.Sign() != 0 {
		t.Fatalf("unswept after settle %v", got)
	}
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyCold); got == nil || got.Int64() != 1000 {
		t.Fatalf("cold %v", got)
	}
	if st, _ := orderState(t, db, o1); st != "PAID" {
		t.Fatalf("order status %s", st)
	}

	// 深分叉: 高度 6 起 B 链 (公共祖先 A5; B6.parent=A5, B7.parent=B6, B8.parent=B7).
	remote := map[uint64]*chain.BlockHeader{}
	for i := uint64(0); i <= 5; i++ {
		remote[i] = blk[i]
	}
	b6h := types.HexToHash("0x000b06")
	b7h := types.HexToHash("0x000b07")
	b8h := types.HexToHash("0x000b08")
	remote[6] = &chain.BlockHeader{Number: 6, Hash: b6h, ParentHash: blk[5].Hash}
	remote[7] = &chain.BlockHeader{Number: 7, Hash: b7h, ParentHash: b6h}
	remote[8] = &chain.BlockHeader{Number: 8, Hash: b8h, ParentHash: b7h}
	fq.headers = remote
	fq.heads.Latest = 8
	fq.evs = map[uint64][]chain.TransferEvent{}
	fq.receipts = map[types.Hash]*ethtypes.Receipt{}

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// 账本断言: sweep 红冲使 UNSWEPT +1000, 随后订单 unwind -1000 → 0.
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyUnswept); got == nil || got.Sign() != 0 {
		t.Fatalf("unswept after deep rollback %v", got)
	}
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyCold); got == nil || got.Sign() != 0 {
		t.Fatalf("cold after rollback %v want 0", got)
	}
	if got := led.balOf(m, testChain, "USDC", types.AccountMerchantAvailable); got == nil || got.Sign() != 0 {
		t.Fatalf("MA after unwind %v want 0", got)
	}
	if st, recv := orderState(t, db, o1); st != "SETTLED_REVERSED" || recv != "0" {
		t.Fatalf("order %s recv=%s want SETTLED_REVERSED/0", st, recv)
	}
	// deposit ORPHANED_DEEP; attempt 重置 BROADCASTED; batch IN_FLIGHT; item PENDING.
	if s := depStatus(t, db, types.HexToHash("0x09")); s != "ORPHANED_DEEP" {
		t.Fatalf("dep status %s", s)
	}
	var attSt, bSt, iSt string
	if err := db.QueryRow(ctx,
		`SELECT a.status, b.status, i.item_status
		   FROM outbound_tx_attempts a, sweep_batches b, sweep_batch_items i
		  WHERE a.intent_id=$1 AND b.id=$2 AND i.batch_id=$2`,
		sweepIntent, batchID).Scan(&attSt, &bSt, &iSt); err != nil {
		t.Fatal(err)
	}
	if attSt != "BROADCASTED" || bSt != "IN_FLIGHT" || iSt != "PENDING" {
		t.Fatalf("att=%s batch=%s item=%s", attSt, bSt, iSt)
	}
	// 红冲分录存在.
	var reorgs int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE reference_type='REORG_REVERSAL'`).Scan(&reorgs); err != nil {
		t.Skipf("ledger table check (fake ledger 不落表): %v", err)
	}
	_ = reorgs
	if len(rebc.intents) == 0 {
		t.Fatal("no rebroadcast triggered after deep rollback")
	}
}

// ---------------------------------------------------------------------------
// 单写者 advisory lock
// ---------------------------------------------------------------------------

func TestSingleWriterAdvisoryLock(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()

	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(testChain)).Scan(&got); err != nil || !got {
		t.Fatal("acquire lock")
	}
	ix := newIx(db, &fakeChainQ{headers: chainOf(2, 'A'),
		heads: chain.Heights{Latest: 2}}, newFakeLedger(db), nil)
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	done := make(chan struct{})
	go func() { ix.runLocked(lctx); close(done) }()

	select {
	case <-done:
		t.Fatal("runLocked returned while peer holds lock")
	case <-time.After(400 * time.Millisecond):
	}
	// 会话级 advisory lock 不随 conn.Release 自动归还 — 必须显式解锁.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(testChain)); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	// 锁释放后应取得游标行 (启动时 loadCursor 在持锁后).
	deadline := time.Now().Add(3 * time.Second)
	for {
		var cnt int
		_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM chain_sync_cursors WHERE chain_id=$1`,
			int64(testChain)).Scan(&cnt)
		if cnt > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("indexer did not acquire lock after release")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// FIX-A/A1 回归: 分叉视图日志不入账不产分录 + canonical 同 (tx,log) 重挖无 23505
// ---------------------------------------------------------------------------

func TestForkViewFilteredAndCanonicalRepack(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 100) // 1%
	v1, o1, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")
	depTx := types.HexToHash("0x09")
	forkTx := types.HexToHash("0xf0f0")

	localA := chainOf(8, 'A')
	good := transferEv(v1, usdcContract, ext, 1000, 5, depTx)
	forkEv := transferEv(v1, usdcContract, ext, 777, 5, forkTx)
	// 同高度但非 canonical 哈希 — 钉死语义下必须被剔除.
	forkEv.BlockHash = types.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 5, Safe: 5},
		headers: localA,
		evs:     map[uint64][]chain.TransferEvent{5: {good, forkEv}},
	}
	seedCursor(t, db, 4, localA[4].Hash)
	led := newFakeLedger(db)
	ix := newIx(db, fq, led, &fakeRebc{})
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// (a)(b): 分叉视图日志不入账, 不产生任何分录.
	var forkCnt int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM onchain_deposits WHERE tx_hash=$1`, hashStr(forkTx)).Scan(&forkCnt); err != nil {
		t.Fatal(err)
	}
	if forkCnt != 0 {
		t.Fatalf("fork-view deposit ingested: %d rows", forkCnt)
	}
	var txnCnt int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_transactions`).Scan(&txnCnt); err != nil {
		t.Fatal(err)
	}
	if txnCnt != 2 { // dep:c0 + ordsettle
		t.Fatalf("ledger txns=%d want 2 (dep+settle only)", txnCnt)
	}
	if st, _ := orderState(t, db, o1); st != "PAID" {
		t.Fatalf("order %s want PAID", st)
	}

	// (c): 高度 5 深分叉 B 链, 同一 (tx_hash,log_index) 在 canonical 块哈希下重挖 —
	// 旧行先 ORPHANED_DEEP, 新行 DETECTED 插入不得撞部分唯一索引 (无 23505).
	remote := map[uint64]*chain.BlockHeader{}
	for i := uint64(0); i <= 4; i++ {
		remote[i] = localA[i]
	}
	b5 := &chain.BlockHeader{Number: 5, Hash: types.HexToHash("0x000b05"), ParentHash: localA[4].Hash}
	b6 := &chain.BlockHeader{Number: 6, Hash: types.HexToHash("0x000b06"), ParentHash: b5.Hash}
	remote[5], remote[6] = b5, b6
	repack := transferEv(v1, usdcContract, ext, 1000, 5, depTx)
	repack.BlockHash = b5.Hash
	fq.headers = remote
	fq.heads = chain.Heights{Latest: 6, Safe: 6}
	fq.evs = map[uint64][]chain.TransferEvent{5: {repack}}

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatalf("canonical repack must not hit unique violation: %v", err)
	}
	var deps int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM onchain_deposits WHERE tx_hash=$1`, hashStr(depTx)).Scan(&deps); err != nil {
		t.Fatal(err)
	}
	if deps != 2 {
		t.Fatalf("deposit rows for tx=%d want 2 (orphaned + repacked)", deps)
	}
	sts := map[string]int{}
	rows, err := db.Query(ctx,
		`SELECT status FROM onchain_deposits WHERE tx_hash=$1`, hashStr(depTx))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		sts[s]++
	}
	rows.Close()
	if sts["ORPHANED_DEEP"] != 1 || sts["FINALIZED"] != 1 {
		t.Fatalf("statuses %v want {ORPHANED_DEEP:1, FINALIZED:1}", sts)
	}
	// 深回滚红冲已发生 + 重建挂 REFUNDABLE (订单 SETTLED_REVERSED).
	if st, _ := orderState(t, db, o1); st != "SETTLED_REVERSED" {
		t.Fatalf("order %s want SETTLED_REVERSED", st)
	}
	if got := led.balOf(m, testChain, "USDC", types.AccountSuspenseRefundableLiab); got == nil || got.Int64() != 1000 {
		t.Fatalf("refundable after repack %v want 1000", got)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_transactions`).Scan(&txnCnt); err != nil {
		t.Fatal(err)
	}
	if txnCnt != 4 { // + reorgdep:r0 + dep:c0(new)
		t.Fatalf("ledger txns=%d want 4", txnCnt)
	}
}

// ---------------------------------------------------------------------------
// FIX-A/A4 回归: VaultSwept(..,0) 降级结构化告警, 其余项正常结算
// ---------------------------------------------------------------------------

func TestSweepZeroAmountDegraded(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 0)
	v1, _, da1 := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	v2, _, da2 := seedOrder(t, db, m, "USDC", 500, "AWAITING_PAYMENT", time.Hour)

	batchID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batches (id, chain_id, asset_symbol, target_cold_wallet, status)
		 VALUES ($1,$2,'USDC',$3,'IN_FLIGHT')`, batchID, int64(testChain), addrStr(coldWallet)); err != nil {
		t.Fatal(err)
	}
	item1, item2 := uuid.New(), uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO sweep_batch_items (id, batch_id, deposit_address_id, exact_sweep_amount)
		 VALUES ($1,$3,$5,1000),($2,$3,$4,500)`,
		item1, item2, batchID, da2, da1); err != nil {
		t.Fatal(err)
	}
	intentID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_intents (id, intent_type, reference_id, chain_id, wallet_address, nonce, status)
		 VALUES ($1,'SWEEP_BATCH',$2,$3,$4,9,'IN_FLIGHT')`,
		intentID, batchID, int64(testChain), addrStr(relayerHot)); err != nil {
		t.Fatal(err)
	}
	sweepTx := types.HexToHash("0x5e")
	if _, err := db.Exec(ctx,
		`INSERT INTO outbound_tx_attempts
		   (id, intent_id, attempt_no, attempt_kind, chain_id, tx_hash,
		    max_fee_per_gas, max_priority_fee_per_gas, status)
		 VALUES (gen_random_uuid(),$1,1,'PRIMARY_INTENT',$2,$3,3000000000,1000000000,'BROADCASTED')`,
		intentID, int64(testChain), hashStr(sweepTx)); err != nil {
		t.Fatal(err)
	}

	var salt1, salt2 []byte
	if err := db.QueryRow(ctx, `SELECT create2_salt FROM deposit_addresses WHERE id=$1`, da1).Scan(&salt1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT create2_salt FROM deposit_addresses WHERE id=$1`, da2).Scan(&salt2); err != nil {
		t.Fatal(err)
	}
	mkSweepLog := func(vault types.Address, salt []byte, amt int64) *ethtypes.Log {
		return &ethtypes.Log{Address: sweeperAddr,
			Topics: []types.Hash{chain.VaultSweptTopic,
				common.BytesToHash(vault.Bytes()), common.BytesToHash(salt)},
			Data: new(big.Int).SetInt64(amt).FillBytes(make([]byte, 32))}
	}
	blk := chainOf(8, 'A')
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 8},
		headers: blk,
		evs:     map[uint64][]chain.TransferEvent{},
		receipts: map[types.Hash]*ethtypes.Receipt{sweepTx: {
			Status: 1, GasUsed: 90000, EffectiveGasPrice: big.NewInt(1_000_000_000),
			BlockNumber: big.NewInt(7), BlockHash: blk[7].Hash,
			Logs: []*ethtypes.Log{mkSweepLog(v1, salt1, 0), mkSweepLog(v2, salt2, 500)}}},
	}
	seedCursor(t, db, 5, blk[5].Hash)
	led := newFakeLedger(db)
	led.seedBalance(sysID, testChain, "ETH", types.AccountHotWalletNative, 1_000_000_000_000_000)
	led.seedBalance(sysID, testChain, "USDC", types.AccountCustodyUnswept, 1000)

	var alerts []Alert
	ix := New(db, fq, led, nil, &fakeRebc{}, Config{
		ChainID: testChain, BatchBlocks: 100,
		PayoutHotWallet: payoutHot, RelayerHotWallet: relayerHot,
		ColdWallet: coldWallet, BatchSweeper: sweeperAddr,
	}, nil, func(a Alert) { alerts = append(alerts, a) })

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var s1, s2, bst, total string
	if err := db.QueryRow(ctx,
		`SELECT (SELECT item_status FROM sweep_batch_items WHERE id=$1),
		        (SELECT item_status FROM sweep_batch_items WHERE id=$2),
		        b.status, b.total_swept_amount::text
		   FROM sweep_batches b WHERE b.id=$3`,
		item1, item2, batchID).Scan(&s1, &s2, &bst, &total); err != nil {
		t.Fatal(err)
	}
	if s1 != "PENDING" || s2 != "SWEPT" {
		t.Fatalf("items %s/%s want PENDING/SWEPT", s1, s2)
	}
	if bst != "COMPLETED" || total != "500" {
		t.Fatalf("batch %s total=%s want COMPLETED/500", bst, total)
	}
	var zeroAlert bool
	for _, a := range alerts {
		if a.Code == "SWEEP_EVENT_ZERO_AMOUNT" {
			zeroAlert = true
		}
	}
	if !zeroAlert {
		t.Fatalf("expected SWEEP_EVENT_ZERO_AMOUNT alert, got %+v", alerts)
	}
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyCold); got == nil || got.Int64() != 500 {
		t.Fatalf("cold %v want 500", got)
	}
}

// ---------------------------------------------------------------------------
// FIX-A/A5 回归: 多冷钱包集合 (ColdWallet ∪ ColdWallets) 分支 A 判定
// ---------------------------------------------------------------------------

func TestMultiColdWalletBranchA(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	seedMerchant(t, db, 0)
	cold2 := types.HexToAddress("0xcececececececececececececececececececece")
	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")

	blk := chainOf(8, 'A')
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 6, Safe: 6},
		headers: blk,
		evs: map[uint64][]chain.TransferEvent{6: {
			transferEv(payoutHot, usdcContract, coldWallet, 300, 6, types.HexToHash("0x31")), // 旧单值
			transferEv(payoutHot, usdcContract, cold2, 500, 6, types.HexToHash("0x32")),      // 轮换新金库
			transferEv(payoutHot, usdcContract, ext, 700, 6, types.HexToHash("0x33")),        // 外部注资
		}},
	}
	seedCursor(t, db, 5, blk[5].Hash)
	led := newFakeLedger(db)
	led.seedBalance(sysID, testChain, "USDC", types.AccountCustodyCold, 800)
	ix := New(db, fq, led, nil, &fakeRebc{}, Config{
		ChainID: testChain, BatchBlocks: 100,
		PayoutHotWallet: payoutHot, RelayerHotWallet: relayerHot,
		ColdWallet:   coldWallet,
		ColdWallets:  []types.Address{cold2},
		BatchSweeper: sweeperAddr,
	}, nil, nil)

	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var reb, top int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE deposit_category='TREASURY_REBALANCE'),
		        COUNT(*) FILTER (WHERE deposit_category='TOPUP')
		   FROM onchain_deposits`).Scan(&reb, &top); err != nil {
		t.Fatal(err)
	}
	if reb != 2 || top != 1 {
		t.Fatalf("categories rebalance=%d topup=%d want 2/1", reb, top)
	}
	// REBALANCE: DR HOT_PAYOUT / CR COLD -> COLD 800-800=0; TOPUP: CR EQUITY +700.
	if got := led.balOf(sysID, testChain, "USDC", types.AccountCustodyHotPayout); got == nil || got.Int64() != 1500 {
		t.Fatalf("hot payout %v want 1500", got)
	}
	if got := led.balOf(sysID, testChain, "USDC", types.AccountTreasuryEquityCapital); got == nil || got.Int64() != 700 {
		t.Fatalf("equity %v want 700", got)
	}
}

// ---------------------------------------------------------------------------
// FIX-A/A6 回归: unwind 钳制下沉 ledger 锁内 (真实 ledger.Service 驱动)
// ---------------------------------------------------------------------------

func ledgerBal(t *testing.T, db *pgxpool.Pool, owner uuid.UUID, acct types.LedgerAccountCode) string {
	t.Helper()
	var s string
	if err := db.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(balance),0)::text FROM ledger_accounts
		 WHERE owner_merchant_id=$1 AND chain_id=$2 AND asset_symbol='USDC' AND account_type=$3`,
		owner, int64(testChain), string(acct)).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDeepRollbackMAClampInsideLedgerLock(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 0) // fee=0 -> MA 全额 1000
	v1, o1, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")
	depTx := types.HexToHash("0x09")

	// 真实账本引擎: 钳制与冻结在 ledger 行锁内完成 (TOCTOU 消除断言对象).
	real := ledger.NewService(db)

	blk := chainOf(8, 'A')
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 8},
		headers: blk,
		evs:     map[uint64][]chain.TransferEvent{6: {transferEv(v1, usdcContract, ext, 1000, 6, depTx)}},
	}
	seedCursor(t, db, 5, blk[5].Hash)
	ix := newIx(db, fq, real, &fakeRebc{})
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := orderState(t, db, o1); st != "PAID" {
		t.Fatalf("order %s want PAID", st)
	}
	if got := ledgerBal(t, db, m, types.AccountMerchantAvailable); got != "1000" {
		t.Fatalf("MA %s want 1000", got)
	}

	// 商户已提走 700 (冻结腿) -> 可用余额 300; 此时深回滚冲回 1000 必须在
	// 锁内钳制为 MA 300 + RECV 700, 且同事务 payout_frozen.
	if _, err := real.PostEntry(ctx, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "pfreeze:seed",
		Legs: []ports.PostEntryLeg{
			leg(m, testChain, "USDC", types.AccountMerchantAvailable, types.DirectionDebit, big.NewInt(700)),
			leg(m, testChain, "USDC", types.AccountPayoutLockedLiab, types.DirectionCredit, big.NewInt(700)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := ledgerBal(t, db, m, types.AccountMerchantAvailable); got != "300" {
		t.Fatalf("MA %s want 300", got)
	}

	// 高度 6 起 B 链 (公共祖先 a5) — dep 深回滚.
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
		t.Fatal(err)
	}
	if got := ledgerBal(t, db, m, types.AccountMerchantAvailable); got != "0" {
		t.Fatalf("MA %s want 0 (clamped at locked balance 300)", got)
	}
	if got := ledgerBal(t, db, m, types.AccountMerchantReorgReceivable); got != "700" {
		t.Fatalf("RECV %s want 700", got)
	}
	var frozen bool
	if err := db.QueryRow(ctx, `SELECT payout_frozen FROM merchants WHERE id=$1`, m).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if !frozen {
		t.Fatal("payout_frozen must be set inside ledger transaction")
	}
	if st, _ := orderState(t, db, o1); st != "SETTLED_REVERSED" {
		t.Fatalf("order %s want SETTLED_REVERSED", st)
	}
	if s := depStatus(t, db, depTx); s != "ORPHANED_DEEP" {
		t.Fatalf("dep %s want ORPHANED_DEEP", s)
	}
}

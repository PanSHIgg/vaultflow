package txmanager

import (
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 测试基建
// ---------------------------------------------------------------------------

func testDSN(t *testing.T) string {
	if dsn := os.Getenv("VF_TEST_DSN"); dsn != "" {
		return dsn
	}
	if dsn := os.Getenv("VF_TEST_PG_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://postgres:postgres@localhost:55432/vaultflow?sslmode=disable"
}

// freshDB 清空全表并重建系统哨兵商户 (FK 依赖).
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(db.Close)
	_, err = db.Exec(context.Background(), `TRUNCATE
		chain_sync_cursors, supported_assets, merchants,
		merchant_withdrawal_addresses, deposit_addresses, payment_orders,
		chain_blocks, onchain_deposits, wallet_nonce_cursors,
		outbound_tx_intents, outbound_tx_attempts, sweep_batches,
		sweep_batch_items, merchant_payouts, webhook_outbox,
		signer_audit_anchor,
		ledger_accounts, ledger_transactions, ledger_entries
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Skipf("truncate: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
		 VALUES ('00000000-0000-0000-0000-000000000000','__SYSTEM__','__SYSTEM_SENTINEL_NO_API_KEY__',0) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("reseed sentinel: %v", err)
	}
	return db
}

// fakeChain ChainProvider 可编程实现.
type fakeChain struct {
	sent         [][]byte
	present      map[types.Hash]bool // mempool/pending 可见
	receipts     map[types.Hash]*ethtypes.Receipt
	latestNonce  uint64
	pendingNonce uint64
	sendErr      error
	receiptErr   error // B4: 回执查询注入错误
	nonceErr     error // B5: nonce 探测注入错误
}

func (f *fakeChain) SendRawTx(ctx context.Context, raw []byte) (types.Hash, error) {
	if f.sendErr != nil {
		return types.Hash{}, f.sendErr
	}
	f.sent = append(f.sent, raw)
	h := types.Hash{}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(raw); err == nil {
		h = types.Hash(tx.Hash())
	}
	return h, nil
}

func (f *fakeChain) TxPresent(ctx context.Context, h types.Hash) (bool, bool, error) {
	if f.receipts != nil {
		if _, ok := f.receipts[h]; ok {
			return true, true, nil
		}
	}
	return f.present[h], false, nil
}

func (f *fakeChain) Receipt(ctx context.Context, h types.Hash) (*ethtypes.Receipt, error) {
	if f.receiptErr != nil {
		return nil, f.receiptErr
	}
	return f.receipts[h], nil
}

func (f *fakeChain) Nonce(ctx context.Context, _ types.Address, pending bool) (uint64, error) {
	if f.nonceErr != nil {
		return 0, f.nonceErr
	}
	if pending {
		return f.pendingNonce, nil
	}
	return f.latestNonce, nil
}

// fakeSigner 真实签名: 解 unsigned payload, 用固定测试密钥签出合法 Type-2 tx.
// reject 无条件拒签; rejectFor 按 PolicyHint 定向拒签 (B6 测试区分
// 业务载荷被拒 vs GAP_FILLER 兜底被拒).
type fakeSigner struct {
	calls     int
	reject    *ports.Rejection
	rejectFor map[ports.TxPolicy]*ports.Rejection
}

var signerTestKey, _ = crypto.GenerateKey()

type unsignedDynPayload struct {
	ChainID    *big.Int
	Nonce      uint64
	GasTipCap  *big.Int
	GasFeeCap  *big.Int
	Gas        uint64
	To         *common.Address `rlp:"nil"`
	Value      *big.Int
	Data       []byte
	AccessList ethtypes.AccessList
}

func (f *fakeSigner) SignTx(ctx context.Context, req ports.SignTxRequest) (ports.SignTxResponse, error) {
	f.calls++
	if f.reject != nil {
		return ports.SignTxResponse{Rejected: f.reject}, nil
	}
	if r, ok := f.rejectFor[req.PolicyHint]; ok {
		return ports.SignTxResponse{Rejected: r}, nil
	}
	var p unsignedDynPayload
	if err := rlp.DecodeBytes(req.UnsignedTx[1:], &p); err != nil {
		return ports.SignTxResponse{}, err
	}
	signed, err := ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID: p.ChainID, Nonce: p.Nonce, GasTipCap: p.GasTipCap,
		GasFeeCap: p.GasFeeCap, Gas: p.Gas, To: p.To, Value: p.Value,
		Data: p.Data, AccessList: p.AccessList}),
		ethtypes.NewCancunSigner(p.ChainID), signerTestKey)
	if err != nil {
		return ports.SignTxResponse{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return ports.SignTxResponse{}, err
	}
	return ports.SignTxResponse{Signed: &ports.SignedTx{
		RawTx: raw, TxHash: types.Hash(signed.Hash()), AuditSeq: uint64(f.calls)}}, nil
}
func (f *fakeSigner) RegisterWhitelist(context.Context, ports.RegisterWhitelistRequest) (ports.WhitelistOpResponse, error) {
	return ports.WhitelistOpResponse{Accepted: true}, nil
}
func (f *fakeSigner) RevokeWhitelist(context.Context, ports.RevokeWhitelistRequest) (ports.WhitelistOpResponse, error) {
	return ports.WhitelistOpResponse{Accepted: true}, nil
}
func (f *fakeSigner) VerifyAuditChain(context.Context, ports.VerifyAuditChainRequest) (ports.VerifyAuditChainResponse, error) {
	return ports.VerifyAuditChainResponse{Valid: true}, nil
}
func (f *fakeSigner) Health(context.Context) (ports.SignerHealth, error) {
	return ports.SignerHealth{Version: "fake"}, nil
}

func wallet() types.Address {
	return types.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
}

// seedPayout 造 rebuild 所需的 supported_assets + merchant + merchant_payouts 行.
func seedPayout(t *testing.T, db *pgxpool.Pool) uuid.UUID {
	ctx := context.Background()
	mid := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO merchants (id, name, api_key_hash) VALUES ($1,'m',$2)`,
		mid, "h"+mid.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO supported_assets (chain_id, asset_symbol, token_contract, decimals)
		 VALUES (31337,'USDC','0x2222222222222222222222222222222222222222',6)
		 ON CONFLICT (chain_id, asset_symbol) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	pid := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO merchant_payouts
		   (id, merchant_id, idempotency_key, request_hash, payout_kind, source_account_type,
		    chain_id, asset_symbol, to_address, amount, status)
		 VALUES ($1,$2,$3,$4,'WITHDRAWAL','MERCHANT_AVAILABLE',31337,'USDC',
		   '0x9999999999999999999999999999999999999999',1000,'IN_FLIGHT')`,
		pid, mid, "k"+pid.String()[:8], "h"+pid.String()[:8]); err != nil {
		t.Fatal(err)
	}
	return pid
}

func queueReq(ref uuid.UUID) ports.QueueIntentRequest {
	// calldata 与 seedPayout 的确定性重建一致: transfer(0x9999.., 1000) @ USDC(0x2222..)
	cd, _ := chain.EncodeERC20Transfer(
		types.HexToAddress("0x9999999999999999999999999999999999999999"), big.NewInt(1000))
	return ports.QueueIntentRequest{
		IntentType:           types.IntentTypeMerchantPayout,
		ReferenceID:          ref,
		ChainID:              31337,
		WalletAddress:        wallet(),
		To:                   types.HexToAddress("0x2222222222222222222222222222222222222222"),
		Value:                types.U256FromBig(big.NewInt(0)),
		Calldata:             cd,
		GasLimit:             100_000,
		MaxFeePerGas:         types.U256FromBig(big.NewInt(2_000_000_000)),
		MaxPriorityFeePerGas: types.U256FromBig(big.NewInt(1_000_000_000)),
	}
}

// ---------------------------------------------------------------------------
// 黄金向量: EIP-2718 Type-2 未签名编码
// ---------------------------------------------------------------------------

func TestBuildUnsignedTxGoldenVector(t *testing.T) {
	got, err := BuildUnsignedTx(31337, 0,
		big.NewInt(1_000_000_000), big.NewInt(2_000_000_000), 21000,
		types.HexToAddress("0x3333333333333333333333333333333333333333"),
		big.NewInt(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "02e9827a6980843b9aca0084773594008252089433333333333333333333333333333333333333338080c0"
	hexed := "02" + hexEnc(got[1:])
	if "0x"+hexed != "0x"+want && hexed != want {
		t.Fatalf("unsigned tx mismatch:\n got %x\nwant %s", got, want)
	}
}

func hexEnc(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = d[c>>4]
		out[i*2+1] = d[c&0x0f]
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// PG 集成: Nonce 分配 / 首播 / 巡检矩阵 / 取消 / 重广播
// ---------------------------------------------------------------------------

func TestQueueIntentContiguousNonce(t *testing.T) {
	db := freshDB(t)
	m := New(db, &fakeChain{}, &fakeSigner{}, Config{}, nil, nil)
	ctx := context.Background()

	var nonces []uint64
	for i := 0; i < 3; i++ {
		it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
		if err != nil {
			t.Fatal(err)
		}
		nonces = append(nonces, it.Nonce)
	}
	for i, n := range nonces {
		if n != uint64(i) {
			t.Fatalf("nonces %v not contiguous", nonces)
		}
	}
	var next int64
	if err := db.QueryRow(ctx,
		`SELECT next_nonce FROM wallet_nonce_cursors WHERE chain_id=31337 AND wallet_address=$1`,
		addrStr(wallet())).Scan(&next); err != nil || next != 3 {
		t.Fatalf("cursor next_nonce=%d err=%v", next, err)
	}
	// 异钱包独立序号.
	other := types.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	req := queueReq(uuid.New())
	req.WalletAddress = other
	it, err := m.QueueIntent(ctx, req)
	if err != nil || it.Nonce != 0 {
		t.Fatalf("other wallet nonce=%d err=%v", it.Nonce, err)
	}
}

func TestBroadcastPipelinePersistsAttempt(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	n, err := m.BroadcastQueued(ctx)
	if err != nil || n != 1 {
		t.Fatalf("broadcast n=%d err=%v", n, err)
	}

	atts, err := m.ListAttempts(ctx, it.ID)
	if err != nil || len(atts) != 1 {
		t.Fatalf("attempts %+v err=%v", atts, err)
	}
	a := atts[0]
	if a.Status != types.AttemptBroadcasted || a.Kind != types.AttemptKindPrimary || a.AttemptNo != 1 {
		t.Fatalf("attempt %+v", a)
	}
	got, _ := m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status %s", got.Status)
	}
	// 已广播字节可解码且 nonce/to/gas 正确.
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[0]); err != nil {
		t.Fatal(err)
	}
	if tx.Nonce() != 0 || tx.Gas() != 100_000 || tx.GasFeeCap().Cmp(big.NewInt(2e9)) != 0 ||
		tx.GasTipCap().Cmp(big.NewInt(1e9)) != 0 {
		t.Fatalf("tx fields nonce=%d gas=%d feecap=%s tipcap=%s",
			tx.Nonce(), tx.Gas(), tx.GasFeeCap(), tx.GasTipCap())
	}
	want := types.HexToAddress("0x2222222222222222222222222222222222222222")
	if *tx.To() != [20]byte(want) {
		t.Fatalf("to=%x want %x", tx.To(), want)
	}
}

func TestWatchdogRBFBump(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}, latestNonce: 0}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{InFlightTimeout: time.Millisecond}, nil, nil)
	ctx := context.Background()

	it, _ := m.QueueIntent(ctx, queueReq(seedPayout(t, db)))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	// 回拨广播时间触发超时.
	if _, err := db.Exec(ctx,
		`UPDATE outbound_tx_attempts SET broadcasted_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	fc.present[firstHash(t, m, it.ID)] = true // mempool 仍可见 -> RBF 而非 DROPPED
	if err := m.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	atts, _ := m.ListAttempts(ctx, it.ID)
	if len(atts) != 2 || atts[1].AttemptNo != 2 || atts[1].Status != types.AttemptBroadcasted {
		t.Fatalf("attempts %+v", atts)
	}
	// RBF 费率提升 >= 12.5%.
	old := atts[0].MaxFeePerGas.Big()
	newF := atts[1].MaxFeePerGas.Big()
	min := new(big.Int).Mul(old, big.NewInt(11250))
	min.Div(min, big.NewInt(10000))
	if newF.Cmp(min) < 0 {
		t.Fatalf("rbf fee %s < old*1.125 (%s)", newF, min)
	}
}

func TestWatchdogDroppedResend(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}, pendingNonce: 0}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{DroppedMinAge: time.Millisecond, InFlightTimeout: time.Hour}, nil, nil)
	ctx := context.Background()

	it, _ := m.QueueIntent(ctx, queueReq(seedPayout(t, db)))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE outbound_tx_attempts SET broadcasted_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := m.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	atts, _ := m.ListAttempts(ctx, it.ID)
	if len(atts) != 2 {
		t.Fatalf("want resend attempt, got %+v", atts)
	}
	if atts[0].Status != types.AttemptDropped {
		t.Fatalf("dropped attempt status=%s (DROPPED 不可逆)", atts[0].Status)
	}
	if atts[1].Status != types.AttemptBroadcasted || atts[1].AttemptNo != 2 {
		t.Fatalf("resend attempt %+v", atts[1])
	}
	// 补发费率沿最近一档提价 — tx_hash 必异 (唯一约束安全).
	if atts[0].TxHash == atts[1].TxHash {
		t.Fatal("resend produced identical tx_hash")
	}
}

func TestWatchdogNonceStolenHalt(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}} // latestNonce=0: 游标/意图 nonce 从 0 起步
	fs := &fakeSigner{}
	var alerts []Alert
	zl, _ := zap.NewDevelopment()
	m := New(db, fc, fs, Config{}, zl, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	it, _ := m.QueueIntent(ctx, queueReq(uuid.New()))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	fc.latestNonce = 5 // 带外交易消费 nonce 0..4 (私钥泄露/他处签名) — 触发 NONCE_STOLEN_HALT
	if err := m.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentNonceStolenHalt {
		t.Fatalf("intent status=%s want NONCE_STOLEN_HALT", got.Status)
	}
	found := false
	for _, a := range alerts {
		if a.Code == AlertCodeNonceStolen && a.Severity == "CRITICAL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing CRITICAL NONCE_STOLEN_HALT alert: %+v", alerts)
	}
	// 熔断后拒绝新意图.
	if _, err := m.QueueIntent(ctx, queueReq(uuid.New())); !errors.Is(err, ports.ErrNonceHalted) {
		t.Fatalf("want ErrNonceHalted, got %v", err)
	}
}

func TestRequestCancelGapFiller(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	it, _ := m.QueueIntent(ctx, queueReq(uuid.New()))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestCancel(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	atts, _ := m.ListAttempts(ctx, it.ID)
	var filler *types.OutboundTxAttempt
	for i := range atts {
		if atts[i].Kind == types.AttemptKindGapFiller {
			filler = &atts[i]
		}
	}
	if filler == nil {
		t.Fatalf("no GAP_FILLER attempt: %+v", atts)
	}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[len(fc.sent)-1]); err != nil {
		t.Fatal(err)
	}
	if *tx.To() != [20]byte(wallet()) || tx.Gas() != 21_000 || tx.Value().Sign() != 0 {
		t.Fatalf("filler tx to=%x gas=%d value=%s", tx.To(), tx.Gas(), tx.Value())
	}
	// 非 IN_FLIGHT 拒绝取消.
	if err := m.RequestCancel(ctx, uuid.New()); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRebroadcastIntentResignsSameParams(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	it, _ := m.QueueIntent(ctx, queueReq(seedPayout(t, db)))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	// 模拟 indexer 浅回滚: attempt 清块字段回 BROADCASTED (已本为 BROADCASTED).
	if err := m.RebroadcastIntent(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	if len(fc.sent) != 2 {
		t.Fatalf("want 2 broadcasts, got %d", len(fc.sent))
	}
	// 确定性签名: 同参数字节 -> 同 tx_hash / 同字节流.
	if string(fc.sent[0]) != string(fc.sent[1]) {
		t.Fatal("deterministic resign produced different raw bytes")
	}
}

func firstHash(t *testing.T, m *Manager, intentID uuid.UUID) types.Hash {
	atts, err := m.ListAttempts(context.Background(), intentID)
	if err != nil || len(atts) == 0 {
		t.Fatal(err)
	}
	return atts[0].TxHash
}

// ---------------------------------------------------------------------------
// FIX-B 回归测试: B1 载荷持久化 / B3 幂等键 / B4 熔断证据 / B5 fail-closed / B6 拒签
// ---------------------------------------------------------------------------

// relayReq EIP3009_RELAY 请求 (载荷只能经 payload_* 列重建 — B1 主战场).
func relayReq(ref uuid.UUID) ports.QueueIntentRequest {
	return ports.QueueIntentRequest{
		IntentType:           types.IntentTypeEIP3009Relay,
		ReferenceID:          ref,
		ChainID:              31337,
		WalletAddress:        wallet(),
		To:                   types.HexToAddress("0x7777777777777777777777777777777777777777"),
		Value:                types.U256{},
		Calldata:             []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02},
		GasLimit:             200_000,
		MaxFeePerGas:         types.U256FromBig(big.NewInt(3e9)),
		MaxPriorityFeePerGas: types.U256FromBig(big.NewInt(1e9)),
	}
}

// B1: 重启后 (新 Manager, 空 pendingParams) QUEUED EIP3009_RELAY 可从
// payload_* 列重建广播 — 原实现 ErrRebuildUnsupported 会死锁 nonce.
func TestRelayPayloadRebuildAfterRestart(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m1 := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	it, err := m1.QueueIntent(ctx, relayReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟进程重启: 全新 Manager 实例, pendingParams 为空.
	m2 := New(db, fc, fs, Config{}, nil, nil)
	n, err := m2.BroadcastQueued(ctx)
	if err != nil || n != 1 {
		t.Fatalf("broadcast after restart n=%d err=%v", n, err)
	}
	got, _ := m2.GetIntent(ctx, it.ID)
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status %s want IN_FLIGHT", got.Status)
	}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[0]); err != nil {
		t.Fatal(err)
	}
	want := types.HexToAddress("0x7777777777777777777777777777777777777777")
	if *tx.To() != [20]byte(want) || tx.Gas() != 200_000 ||
		tx.GasFeeCap().Cmp(big.NewInt(3e9)) != 0 || tx.GasTipCap().Cmp(big.NewInt(1e9)) != 0 {
		t.Fatalf("rebuilt tx to=%x gas=%d fee=%s tip=%s",
			tx.To(), tx.Gas(), tx.GasFeeCap(), tx.GasTipCap())
	}
	if string(tx.Data()) != string([]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02}) {
		t.Fatalf("rebuilt calldata %x", tx.Data())
	}
}

// B1: 重启后 DROPPED 补发 / RBF 路径同样从 payload_* 重建 (relayer 钱包).
func TestRelayDroppedResendAfterRestart(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}, pendingNonce: 0}
	fs := &fakeSigner{}
	m1 := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	it, _ := m1.QueueIntent(ctx, relayReq(uuid.New()))
	// 重启实例负责首播 + 巡检 (pendingParams 空 → 全靠 payload_* 列).
	m2 := New(db, fc, fs, Config{DroppedMinAge: time.Millisecond, InFlightTimeout: time.Hour}, nil, nil)
	if _, err := m2.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE outbound_tx_attempts SET broadcasted_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	// mempool 驱逐判定 (无回执+不可见+pendingNonce<=K) → DROPPED 补发.
	if err := m2.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	atts, _ := m2.ListAttempts(ctx, it.ID)
	if len(atts) != 2 || atts[0].Status != types.AttemptDropped ||
		atts[1].Status != types.AttemptBroadcasted {
		t.Fatalf("dropped resend attempts %+v", atts)
	}
	if atts[0].TxHash == atts[1].TxHash {
		t.Fatal("resend produced identical tx_hash")
	}
}

// B3: 同 (intent_type, reference_id) 重入幂等 — 只产一个意图一个 nonce.
func TestQueueIntentIdempotentSameReference(t *testing.T) {
	db := freshDB(t)
	m := New(db, &fakeChain{}, &fakeSigner{}, Config{}, nil, nil)
	ctx := context.Background()

	ref := uuid.New()
	it1, err := m.QueueIntent(ctx, queueReq(ref))
	if err != nil {
		t.Fatal(err)
	}
	it2, err := m.QueueIntent(ctx, queueReq(ref))
	if err != nil {
		t.Fatal(err)
	}
	if it1.ID != it2.ID || it1.Nonce != it2.Nonce {
		t.Fatalf("idempotent requeue produced different intent %s vs %s", it1.ID, it2.ID)
	}
	var n, next int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbound_tx_intents WHERE reference_id=$1`, ref).Scan(&n); err != nil || n != 1 {
		t.Fatalf("intents for ref=%d want 1", n)
	}
	if err := db.QueryRow(ctx,
		`SELECT next_nonce FROM wallet_nonce_cursors
		  WHERE chain_id=31337 AND wallet_address=$1`, addrStr(wallet())).Scan(&next); err != nil || next != 1 {
		t.Fatalf("cursor next_nonce=%d err=%v (nonce 不应被重试消耗)", next, err)
	}
	// 不同 reference 正常分配下一个 nonce.
	it3, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil || it3.Nonce != 1 {
		t.Fatalf("new reference nonce=%d err=%v", it3.Nonce, err)
	}
	// 终态意图不挡同引用重发 (FAILED_RETRYABLE -> IN_FLIGHT 换 nonce 重试模型).
	if _, err := db.Exec(ctx,
		`UPDATE outbound_tx_intents SET status='FAILED_REVERTED' WHERE id=$1`, it3.ID); err != nil {
		t.Fatal(err)
	}
	it4, err := m.QueueIntent(ctx, queueReq(it3.ReferenceID))
	if err != nil {
		t.Fatalf("retry after terminal intent: %v", err)
	}
	if it4.ID == it3.ID || it4.Nonce != 2 {
		t.Fatalf("post-terminal retry got %s nonce=%d want new intent nonce=2", it4.ID, it4.Nonce)
	}
}

// B5: 首见钱包 nonce 探测失败 → fail-closed, 不落游标不落意图.
func TestQueueIntentNonceProbeFailClosed(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{nonceErr: errors.New("rpc unreachable")}
	m := New(db, fc, &fakeSigner{}, Config{}, nil, nil)
	ctx := context.Background()

	if _, err := m.QueueIntent(ctx, queueReq(uuid.New())); err == nil {
		t.Fatal("want fail-closed error on nonce probe failure")
	}
	var c1, c2 int
	_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM wallet_nonce_cursors`).Scan(&c1)
	_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM outbound_tx_intents`).Scan(&c2)
	if c1 != 0 || c2 != 0 {
		t.Fatalf("probe failure persisted cursor=%d intents=%d", c1, c2)
	}
	// 探测恢复后正常初始化 (链上 latest=7 → 首个 nonce 为 7).
	fc.nonceErr = nil
	fc.latestNonce = 7
	it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil || it.Nonce != 7 {
		t.Fatalf("after recovery nonce=%d err=%v", it.Nonce, err)
	}
}

// B4: K<latestNonce 但 Receipt 查询全报错 → 不熔断; 确定性无回执 → 熔断.
func TestWatchdogNoFalseHaltOnReceiptProbeError(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{},
		receiptErr: errors.New("rpc flaky")}
	fs := &fakeSigner{}
	var alerts []Alert
	m := New(db, fc, fs, Config{}, nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	it, _ := m.QueueIntent(ctx, queueReq(uuid.New()))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	fc.latestNonce = 5 // 带外消费嫌疑 — 但回执查询全部失败, 证据不足.
	if err := m.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status=%s — receipt probe error must NOT trip halt", got.Status)
	}
	var found bool
	for _, a := range alerts {
		if a.Code == "NONCE_STOLEN_INCONCLUSIVE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want NONCE_STOLEN_INCONCLUSIVE alert, got %+v", alerts)
	}
	// 查询恢复且确定无回执 → 正常熔断.
	fc.receiptErr = nil
	fc.receipts = map[types.Hash]*ethtypes.Receipt{}
	if err := m.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentNonceStolenHalt {
		t.Fatalf("intent status=%s want NONCE_STOLEN_HALT on proven empty receipts", got.Status)
	}
}

// B6: 业务载荷被 signer 拒签 → 同 nonce GAP_FILLER 兜底 (填洞防级联死锁),
// 意图走 IN_FLIGHT -> (入块后) CANCELLED_BY_FILLER 终态, 非静默重试.
func TestSignerRejectedCancelsViaGapFiller(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{rejectFor: map[ports.TxPolicy]*ports.Rejection{
		ports.TxPolicyPayout: {Code: ports.RejectCodeWhitelistNotFound, Reason: "whitelist missing"},
	}}
	var alerts []Alert
	m := New(db, fc, fs, Config{}, nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	pid := seedPayout(t, db)
	it, _ := m.QueueIntent(ctx, queueReq(pid))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status=%s want IN_FLIGHT (filler candidate)", got.Status)
	}
	atts, _ := m.ListAttempts(ctx, it.ID)
	if len(atts) != 1 || atts[0].Kind != types.AttemptKindGapFiller {
		t.Fatalf("want single GAP_FILLER attempt, got %+v", atts)
	}
	// filler 是自转账 0 value 21000 gas (GAP_FILLER_POLICY).
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[len(fc.sent)-1]); err != nil {
		t.Fatal(err)
	}
	if *tx.To() != [20]byte(wallet()) || tx.Gas() != 21_000 || tx.Value().Sign() != 0 {
		t.Fatalf("filler tx malformed: to=%x gas=%d value=%s", tx.To(), tx.Gas(), tx.Value())
	}
	// last_error 透出拒签原因.
	var le *string
	if err := db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, it.ID).Scan(&le); err != nil {
		t.Fatal(err)
	}
	if le == nil || !strings.Contains(*le, "SIGNER_REJECTED") {
		t.Fatalf("last_error=%v want SIGNER_REJECTED*", le)
	}
	var alerted bool
	for _, a := range alerts {
		if a.Code == "SIGNER_REJECTED" {
			alerted = true
		}
	}
	if !alerted {
		t.Fatalf("missing SIGNER_REJECTED alert: %+v", alerts)
	}
}

// B6: signer 连 GAP_FILLER 也拒签 → 意图落 REJECTED_BY_SIGNER 显式终态 +
// CRITICAL 告警; 关联 payout 回 FAILED_RETRYABLE, 不静默空转.
func TestSignerRejectedTerminalState(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{reject: &ports.Rejection{Code: ports.RejectCodeInternal, Reason: "signer broken"}}
	var alerts []Alert
	m := New(db, fc, fs, Config{}, nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	pid := seedPayout(t, db)
	it, _ := m.QueueIntent(ctx, queueReq(pid))
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetIntent(ctx, it.ID)
	if got.Status != types.TxIntentStatus("REJECTED_BY_SIGNER") {
		t.Fatalf("intent status=%s want REJECTED_BY_SIGNER", got.Status)
	}
	// 关联 payout 兜底 FAILED_RETRYABLE (不悬挂).
	var pst string
	if err := db.QueryRow(ctx,
		`SELECT status FROM merchant_payouts WHERE id=$1`, pid).Scan(&pst); err != nil {
		t.Fatal(err)
	}
	if pst != "FAILED_RETRYABLE" {
		t.Fatalf("payout status=%s want FAILED_RETRYABLE", pst)
	}
	var crit bool
	for _, a := range alerts {
		if a.Code == "SIGNER_REJECTED_TERMINAL" && a.Severity == "CRITICAL" {
			crit = true
		}
	}
	if !crit {
		t.Fatalf("missing CRITICAL SIGNER_REJECTED_TERMINAL alert: %+v", alerts)
	}
	// 再跑一轮 — 终态意图不再被 BroadcastQueued 拾起 (非静默循环).
	if n, err := m.BroadcastQueued(ctx); err != nil || n != 0 {
		t.Fatalf("second drain n=%d err=%v — terminal intent must not be re-picked", n, err)
	}
}

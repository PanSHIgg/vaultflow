package order

// FIX-D2 回归: PayoutDispatcher 扫描 LOCKED_QUEUED -> QueueIntent ->
// IN_FLIGHT, 幂等恢复与失败退避.

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// fakeTxManager ports.TxManager 桩 — 记录 QueueIntent/RequestCancel 调用.
type fakeTxManager struct {
	mu       sync.Mutex
	queued   []ports.QueueIntentRequest
	cancels  []uuid.UUID
	calls    int
	queueErr error
	// FIX-E/E2: QueueIntent 成功返回后、Dispatcher 调 OnPayoutInFlight 之前
	// 触发的竞速钩子 — 用于模拟 "意图已建但 payout 尚未流转" 窗口内的
	// CancelPayout 竞速.
	onQueue func()
}

func (f *fakeTxManager) QueueIntent(_ context.Context, req ports.QueueIntentRequest) (types.OutboundTxIntent, error) {
	f.mu.Lock()
	f.calls++
	if f.queueErr != nil {
		f.mu.Unlock()
		return types.OutboundTxIntent{}, f.queueErr
	}
	f.queued = append(f.queued, req)
	hook := f.onQueue
	f.mu.Unlock()
	if hook != nil {
		hook() // 竞速窗口: 意图已排队, 业务行尚未 OnPayoutInFlight
	}
	return types.OutboundTxIntent{
		ID:          uuid.New(),
		IntentType:  req.IntentType,
		ReferenceID: req.ReferenceID,
		ChainID:     req.ChainID,
		Status:      types.TxIntentStatus("QUEUED"),
	}, nil
}
func (f *fakeTxManager) GetIntent(context.Context, uuid.UUID) (types.OutboundTxIntent, error) {
	return types.OutboundTxIntent{}, ports.ErrNotFound
}
func (f *fakeTxManager) ListAttempts(context.Context, uuid.UUID) ([]types.OutboundTxAttempt, error) {
	return nil, nil
}
func (f *fakeTxManager) RequestCancel(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	return nil
}
func (f *fakeTxManager) NonceCursorStatus(_ context.Context, chainID types.ChainID, wallet types.Address) (ports.NonceCursorStatus, error) {
	return ports.NonceCursorStatus{ChainID: chainID, WalletAddress: wallet}, nil
}

var testPayoutWallet = types.HexToAddress("0x9999999999999999999999999999999999999999")

func dispatchCfg() PayoutDispatcherConfig {
	return PayoutDispatcherConfig{
		Chains: map[types.ChainID]PayoutChainConfig{
			testChain: {
				PayoutWallet:         testPayoutWallet,
				MaxFeePerGas:         types.MustParseU256("2000000000"),
				MaxPriorityFeePerGas: types.MustParseU256("1000000000"),
			},
		},
		PollInterval: time.Millisecond,
		BatchSize:    16,
		BaseBackoff:  time.Hour, // 长退避便于断言重入被跳过
	}
}

func newDispatcher(t *testing.T) (*Service, *pgxpool.Pool, *fakeTxManager, *PayoutDispatcher) {
	t.Helper()
	s, pool := newService(t)
	fake := &fakeTxManager{}
	return s, pool, fake, NewPayoutDispatcher(s, fake, dispatchCfg(), nil)
}

// seedRefundPayout 造一条 LOCKED_QUEUED CUSTOMER_REFUND (携带票据).
func seedRefundPayout(t *testing.T, s *Service, pool *pgxpool.Pool, amount string) types.MerchantPayout {
	t.Helper()
	seedSuspenseBalance(t, pool, types.AccountSuspenseRefundableLiab, amount)
	to := types.HexToAddress("0x7777777777777777777777777777777777777777")
	puuid := uuid.New()
	p, err := s.RequestPayout(context.Background(), ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "rf-d-" + puuid.String()[:8],
		RequestHash: "h-" + puuid.String(), Kind: types.PayoutKindCustomerRefund,
		SourceAccount: types.AccountSuspenseRefundableLiab,
		ChainID:       testChain, AssetSymbol: "USDC",
		ToAddress: to, Amount: types.MustParseU256(amount),
		PayoutID: puuid,
		RefundTicket: buildTestTicket(uint64(testChain), testToken, to,
			types.MustParseU256(amount), puuid, 0,
			uint64(time.Now().Add(time.Hour).Unix())),
	})
	if err != nil {
		t.Fatalf("seed refund payout: %v", err)
	}
	return p
}

func TestPayoutDispatcher_QueuesRefundIntent(t *testing.T) {
	s, pool, fake, d := newDispatcher(t)
	ctx := context.Background()
	p := seedRefundPayout(t, s, pool, "60")

	n, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched = %d, want 1", n)
	}
	if len(fake.queued) != 1 {
		t.Fatalf("QueueIntent calls = %d, want 1", len(fake.queued))
	}
	req := fake.queued[0]
	if req.IntentType != types.IntentTypeMerchantPayout {
		t.Fatalf("intent_type = %s", req.IntentType)
	}
	if req.ReferenceID != p.ID {
		t.Fatalf("reference_id = %s, want payout id %s", req.ReferenceID, p.ID)
	}
	if req.WalletAddress != testPayoutWallet {
		t.Fatalf("wallet = %s, want payout hot wallet %s", req.WalletAddress, testPayoutWallet)
	}
	if req.To != testToken {
		t.Fatalf("to = %s, want token %s", req.To, testToken)
	}
	if req.Value.Sign() != 0 {
		t.Fatalf("value = %s, want 0", req.Value)
	}
	if req.RetrySeq != 0 {
		t.Fatalf("retry_seq = %d, want 0", req.RetrySeq)
	}
	if req.GasLimit == 0 {
		t.Fatal("gas_limit must default to 100_000")
	}
	// ERC-20 transfer(to, amount) calldata 字节级断言 (a9059cbb).
	want := append([]byte{0xa9, 0x05, 0x9c, 0xbb},
		append(make([]byte, 12), p.ToAddress.Bytes()...)...)
	var amt [32]byte
	p.Amount.Big().FillBytes(amt[:])
	want = append(want, amt[:]...)
	if !bytes.Equal(req.Calldata, want) {
		t.Fatalf("calldata = %x, want %x", req.Calldata, want)
	}
	// 退款票据透传 (127B payload || 64B sig 解包).
	if req.RefundTicket == nil || len(req.RefundTicket.SignedPayload) != RefundTicketPayloadLen ||
		len(req.RefundTicket.AdminSignature) != 64 {
		t.Fatalf("refund ticket passthrough malformed: %+v", req.RefundTicket)
	}
	if !bytes.Equal(req.RefundTicket.SignedPayload[99:115], p.ID[:]) {
		t.Fatal("ticket-embedded payout_uuid != payout id — signer binding broken")
	}
	// 状态推进 IN_FLIGHT (OnPayoutInFlight).
	if st := payoutStatus(t, pool, p.ID); st != types.PayoutInFlight {
		t.Fatalf("payout = %s, want IN_FLIGHT", st)
	}
	// 幂等重入: 已 IN_FLIGHT 不再扫描 -> 0 新意图.
	n2, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 || len(fake.queued) != 1 {
		t.Fatalf("re-dispatch must be idempotent: n=%d queued=%d", n2, len(fake.queued))
	}
}

func TestPayoutDispatcher_WithdrawalNoTicket(t *testing.T) {
	s, pool, fake, d := newDispatcher(t)
	ctx := context.Background()
	to := types.HexToAddress("0x6666666666666666666666666666666666666666")
	seedWhitelist(t, pool, to)
	seedSuspenseBalance(t, pool, types.AccountMerchantAvailable, "100")
	puuid := uuid.New()
	p, err := s.RequestPayout(ctx, ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "wd-d-1", RequestHash: "h-wd-d",
		Kind:          types.PayoutKindWithdrawal,
		SourceAccount: types.AccountMerchantAvailable,
		ChainID:       testChain, AssetSymbol: "USDC",
		ToAddress: to, Amount: types.MustParseU256("40"), PayoutID: puuid,
	})
	if err != nil {
		t.Fatalf("seed withdrawal: %v", err)
	}
	n, err := d.DispatchOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("DispatchOnce n=%d err=%v", n, err)
	}
	if len(fake.queued) != 1 || fake.queued[0].RefundTicket != nil {
		t.Fatalf("withdrawal intent must not carry ticket: %+v", fake.queued)
	}
	if st := payoutStatus(t, pool, p.ID); st != types.PayoutInFlight {
		t.Fatalf("payout = %s, want IN_FLIGHT", st)
	}
}

func TestPayoutDispatcher_ExistingIntentRecovery(t *testing.T) {
	s, pool, fake, d := newDispatcher(t)
	ctx := context.Background()
	p := seedRefundPayout(t, s, pool, "60")
	// 模拟崩溃窗口: 意图已建但 payout 仍 LOCKED_QUEUED — 预读命中后
	// 不得再 QueueIntent (与 B3 冲突返回既有意图契约互补).
	if _, err := pool.Exec(ctx, `
		INSERT INTO outbound_tx_intents (intent_type, reference_id, chain_id, wallet_address, nonce, status)
		VALUES ('MERCHANT_PAYOUT', $1, 31337, $2, 0, 'QUEUED')`,
		p.ID, addrHex(testPayoutWallet)); err != nil {
		t.Fatalf("insert intent: %v", err)
	}
	n, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recovery dispatch n=%d, want 1 (status transition only)", n)
	}
	if len(fake.queued) != 0 {
		t.Fatalf("existing intent must not be re-queued: %d calls", len(fake.queued))
	}
	if st := payoutStatus(t, pool, p.ID); st != types.PayoutInFlight {
		t.Fatalf("payout = %s, want IN_FLIGHT", st)
	}
}

func TestPayoutDispatcher_QueueErrorBackoff(t *testing.T) {
	s, pool, fake, d := newDispatcher(t)
	ctx := context.Background()
	p := seedRefundPayout(t, s, pool, "60")
	fake.queueErr = errors.New("txmanager down")

	n, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || fake.calls != 1 {
		t.Fatalf("n=%d calls=%d", n, fake.calls)
	}
	// 退避期内重入: 不再调 QueueIntent.
	if _, err := d.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatalf("backoff must skip retry: calls=%d", fake.calls)
	}
	// 恢复 + 清除退避 -> 成功入队.
	fake.queueErr = nil
	d.clearFail(p.ID)
	n, err = d.DispatchOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recovery n=%d err=%v", n, err)
	}
	if st := payoutStatus(t, pool, p.ID); st != types.PayoutInFlight {
		t.Fatalf("payout = %s, want IN_FLIGHT", st)
	}
}

// NF-1 回归: 终态（死）意图不得阻挡重新派发 —— 预读只认存活态集合。
// 触发序列: QueueIntent 已建但 OnPayoutInFlight 未生效, 意图随后达终态;
// 若预读命中死意图, payout 将永久楔死 IN_FLIGHT 且无生产者/消费者.
func TestPayoutDispatcher_DeadIntentDoesNotWedge(t *testing.T) {
	s, pool, fake, d := newDispatcher(t)
	ctx := context.Background()
	p := seedRefundPayout(t, s, pool, "60")

	if _, err := pool.Exec(ctx, `
		INSERT INTO outbound_tx_intents (intent_type, reference_id, chain_id, wallet_address, nonce, status)
		VALUES ('MERCHANT_PAYOUT', $1, 31337, $2, 7, 'FAILED_REVERTED')`,
		p.ID, addrHex(testPayoutWallet)); err != nil {
		t.Fatalf("insert dead intent: %v", err)
	}

	n, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("dispatch n=%d, want 1 (dead intent must not count as existing)", n)
	}
	if len(fake.queued) != 1 {
		t.Fatalf("dead intent must not suppress QueueIntent: calls=%d", len(fake.queued))
	}
	if fake.queued[0].ReferenceID != p.ID {
		t.Fatalf("reference_id = %s, want payout %s", fake.queued[0].ReferenceID, p.ID)
	}
	if st := payoutStatus(t, pool, p.ID); st != types.PayoutInFlight {
		t.Fatalf("payout = %s, want IN_FLIGHT", st)
	}
}

package order

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/ledger"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// 集成测试需要真实 PostgreSQL (VF_TEST_PG_DSN), 未设置自动 skip.
// 仅标准库 testing (testify 传递依赖缺 go.sum, 见交付报告).

var (
	testChain    = types.ChainID(31337)
	testMerchant = uuid.MustParse("11111111-2222-3333-4444-555555555555")
	testDeriver  = create2.NewDeriver(
		types.HexToAddress("0x2222222222222222222222222222222222222222"),
		types.HexToAddress("0x1111111111111111111111111111111111111111"),
	)
)

func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("VF_TEST_PG_DSN"); dsn != "" {
		return dsn
	}
	t.Skip("VF_TEST_PG_DSN not set; skipping PostgreSQL integration test")
	return ""
}

func newService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	reset(t, pool)
	return NewService(pool, testDeriver, ledger.NewService(pool)), pool
}

// reset 清空测试域表并播种商户与 supported_assets (FK 依赖).
func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`TRUNCATE onchain_deposits, payment_orders, merchant_payouts, deposit_addresses,
		         merchant_withdrawal_addresses, outbound_tx_attempts, outbound_tx_intents,
		         wallet_nonce_cursors, ledger_entries, ledger_transactions, ledger_accounts,
		         webhook_outbox, signer_audit_anchor
		 RESTART IDENTITY CASCADE`,
		`DELETE FROM merchants WHERE id <> '00000000-0000-0000-0000-000000000000'`,
		`INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
		 VALUES ($1, 'w3-order-merchant', $2, 100)
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO supported_assets (chain_id, asset_symbol, token_contract, decimals)
		 VALUES (31337, 'USDC', '0x4444444444444444444444444444444444444444', 6)
		 ON CONFLICT (chain_id, asset_symbol) DO NOTHING`,
	}
	for i, q := range stmts {
		var err error
		if i == 2 {
			_, err = pool.Exec(ctx, q, testMerchant, "hash-"+testMerchant.String())
		} else {
			_, err = pool.Exec(ctx, q)
		}
		if err != nil {
			t.Fatalf("reset stmt %d: %v", i, err)
		}
	}
}

func balanceOf(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, asset string,
	acc types.LedgerAccountCode) string {
	t.Helper()
	var bal string
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(balance),0)::text FROM ledger_accounts
		WHERE owner_merchant_id=$1 AND chain_id=$2 AND asset_symbol=$3 AND account_type=$4`,
		owner, int64(testChain), asset, acc.String()).Scan(&bal)
	if err != nil {
		t.Fatalf("balanceOf %s: %v", acc, err)
	}
	return bal
}

func createTestOrder(t *testing.T, s *Service, key string, expected string, expiresAt time.Time) types.PaymentOrder {
	t.Helper()
	o, err := s.CreateOrder(context.Background(), ports.CreateOrderRequest{
		MerchantID:     testMerchant,
		IdempotencyKey: key,
		RequestHash:    "hash-" + key,
		ChainID:        testChain,
		AssetSymbol:    "USDC",
		ExpectedAmount: types.MustParseU256(expected),
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateOrder(%s): %v", key, err)
	}
	return o
}

// seedEscrow 模拟入站确认: 贷记商户 escrow 并同步 received_amount.
func seedEscrow(t *testing.T, pool *pgxpool.Pool, s *Service, o types.PaymentOrder, amount string) {
	t.Helper()
	_, err := s.ledger.PostEntry(context.Background(), ports.PostEntryRequest{
		Template:    types.TemplateDepositFinalized,
		ReferenceID: "dep:" + o.ID.String(),
		Legs: []ports.PostEntryLeg{
			{OwnerMerchantID: types.SystemMerchantID, ChainID: testChain, AssetSymbol: "USDC",
				Account: types.AccountCustodyUnswept, Direction: types.DirectionDebit,
				Amount: types.MustParseU256(amount)},
			{OwnerMerchantID: o.MerchantID, ChainID: testChain, AssetSymbol: "USDC",
				Account: types.AccountOrderEscrowLiab, Direction: types.DirectionCredit,
				Amount: types.MustParseU256(amount)},
		},
	})
	if err != nil {
		t.Fatalf("seed escrow: %v", err)
	}
	// 模拟 indexer 分支 D 联动: 累计入账并使订单处 PARTIALLY_PAID.
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_orders SET received_amount = $2::numeric, status = 'PARTIALLY_PAID' WHERE id = $1`,
		o.ID, amount); err != nil {
		t.Fatalf("set received_amount: %v", err)
	}
}

// testToken seeded USDC 合约 (reset() 播种, 与 supported_assets 一致).
var testToken = types.HexToAddress("0x4444444444444444444444444444444444444444")

// buildTestTicket 手工拼装 127B REFUND_V1 载荷 (AGENTS §2.3-B 冻结布局,
// 与 api.BuildRefundTicketPayload 逐字节一致的镜像实现) + 64B 伪签名.
func buildTestTicket(chainID uint64, token, to types.Address, amount types.U256,
	payoutID uuid.UUID, retrySeq uint32, expiry uint64) *ports.RefundTicket {
	payload := make([]byte, 0, RefundTicketPayloadLen)
	payload = append(payload, []byte("VAULTFLOW_REFUND_V1")...)
	var b8 [8]byte
	binary.BigEndian.PutUint64(b8[:], chainID)
	payload = append(payload, b8[:]...)
	payload = append(payload, token.Bytes()...)
	payload = append(payload, to.Bytes()...)
	var amt [32]byte
	amount.Big().FillBytes(amt[:])
	payload = append(payload, amt[:]...)
	payload = append(payload, payoutID[:]...)
	var b4 [4]byte
	binary.BigEndian.PutUint32(b4[:], retrySeq)
	payload = append(payload, b4[:]...)
	binary.BigEndian.PutUint64(b8[:], expiry)
	payload = append(payload, b8[:]...)
	return &ports.RefundTicket{SignedPayload: payload, AdminSignature: make([]byte, 64)}
}

// seedSuspenseBalance 直接播种挂账负债科目余额 (退款冻结腿来源).
func seedSuspenseBalance(t *testing.T, pool *pgxpool.Pool, acc types.LedgerAccountCode, amount string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
		VALUES ($1, 31337, 'USDC', $2, 'CREDIT', $3::numeric)
		ON CONFLICT (owner_merchant_id, chain_id, asset_symbol, account_type)
		DO UPDATE SET balance = ledger_accounts.balance + EXCLUDED.balance`,
		testMerchant, acc.String(), amount); err != nil {
		t.Fatalf("seed suspense %s: %v", acc, err)
	}
}

// seedWhitelist 播种 ACTIVE 提现白名单记录 (valid_until = now+24h).
// 测试结束即清除 — mwa 行残留会绊倒其他包的 reset (fk_mwa_merchant
// 拦截 merchants 清理, 共享测试库卫生).
func seedWhitelist(t *testing.T, pool *pgxpool.Pool, addr types.Address) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO merchant_withdrawal_addresses
		  (merchant_id, chain_id, address, admin_approval_sig, valid_until, status)
		VALUES ($1, 31337, $2, '\x00'::bytea, now() + interval '24 hours', 'ACTIVE')
		ON CONFLICT (merchant_id, chain_id, address)
		DO UPDATE SET status = 'ACTIVE', valid_until = now() + interval '24 hours'`,
		testMerchant, addrHex(addr)); err != nil {
		t.Fatalf("seed whitelist: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM merchant_withdrawal_addresses WHERE merchant_id = $1 AND chain_id = 31337`,
			testMerchant)
	})
}

func orderStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) types.OrderStatus {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM payment_orders WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return types.OrderStatus(st)
}

func payoutStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) types.PayoutStatus {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM merchant_payouts WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return types.PayoutStatus(st)
}

// ---------------------------------------------------------------------------
// CreateOrder 幂等编排 + CREATE2 字节级断言
// ---------------------------------------------------------------------------

func TestCreateOrder_CREATE2ByteExact(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "k1", "100000000", time.Now().Add(time.Hour))

	if o.Status != types.OrderAwaitingPayment {
		t.Fatalf("status = %s, want AWAITING_PAYMENT", o.Status)
	}
	if !o.ReceivedAmount.IsZero() {
		t.Fatalf("received = %s, want 0", o.ReceivedAmount)
	}
	// deposit_addresses 行断言: salt 与地址与离线推导逐字节一致.
	var addrHexStr string
	var salt []byte
	err := pool.QueryRow(context.Background(),
		`SELECT address, create2_salt FROM deposit_addresses WHERE id = $1`,
		o.DepositAddressID).Scan(&addrHexStr, &salt)
	if err != nil {
		t.Fatalf("read deposit address: %v", err)
	}
	wantSalt := testDeriver.Salt(testChain, testMerchant, o.ID)
	if len(salt) != 32 || hex.EncodeToString(salt) != hex.EncodeToString(wantSalt[:]) {
		t.Fatalf("create2_salt = %x, want %x", salt, wantSalt[:])
	}
	wantAddr := testDeriver.VaultAddress(wantSalt)
	if addrHexStr != "0x"+hex.EncodeToString(wantAddr.Bytes()) {
		t.Fatalf("deposit address = %s, want %s", addrHexStr, wantAddr.Hex())
	}
}

func TestCreateOrder_Idempotent(t *testing.T) {
	s, _ := newService(t)
	req := ports.CreateOrderRequest{
		MerchantID: testMerchant, IdempotencyKey: "idem-1", RequestHash: "h1",
		ChainID: testChain, AssetSymbol: "USDC",
		ExpectedAmount: types.MustParseU256("100"), ExpiresAt: time.Now().Add(time.Hour),
	}
	o1, err := s.CreateOrder(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := s.CreateOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if o1.ID != o2.ID {
		t.Fatalf("idempotent replay must return same order %s != %s", o1.ID, o2.ID)
	}
	// 异 hash -> 409 冲突.
	req.RequestHash = "different"
	if _, err := s.CreateOrder(context.Background(), req); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("different request_hash must conflict, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 过期重评器
// ---------------------------------------------------------------------------

func TestReevaluateExpired_AwaitingToExpired(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "exp-1", "100", time.Now().Add(-time.Minute))
	n, err := s.ReevaluateExpiredOrders(context.Background(), time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}
	if st := orderStatus(t, pool, o.ID); st != types.OrderExpired {
		t.Fatalf("status = %s, want EXPIRED", st)
	}
	// 无分录发生: escrow 保持 0.
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountOrderEscrowLiab); got != "0" {
		t.Fatalf("escrow = %s, want 0", got)
	}
}

func TestReevaluateExpired_PartialToUnderpaidReview(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "exp-2", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "60")

	n, err := s.ReevaluateExpiredOrders(context.Background(), time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}
	if st := orderStatus(t, pool, o.ID); st != types.OrderUnderpaidExpiredReview {
		t.Fatalf("status = %s, want UNDERPAID_EXPIRED_REVIEW", st)
	}
	// 模板 5: escrow -> refundable.
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountOrderEscrowLiab); got != "0" {
		t.Fatalf("escrow = %s, want 0", got)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountSuspenseRefundableLiab); got != "60" {
		t.Fatalf("refundable = %s, want 60", got)
	}
	// 重评幂等: 再次扫描不再迁移 (状态已变).
	n2, err := s.ReevaluateExpiredOrders(context.Background(), time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second sweep migrated = %d, want 0", n2)
	}
}

func TestReevaluateExpired_InflightDepositBlocks(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "exp-3", "100", time.Now().Add(-time.Minute))
	// 在途 DETECTED 同币种充值.
	var daAddr string
	if err := pool.QueryRow(context.Background(),
		`SELECT address FROM deposit_addresses WHERE id=$1`, o.DepositAddressID).Scan(&daAddr); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO onchain_deposits
		  (chain_id, block_hash, tx_hash, log_index, block_number, token_contract,
		   from_address, to_address, amount, deposit_category, status)
		VALUES (31337, '0xaaa1', '0xbbb1', 0, 10,
		        '0x4444444444444444444444444444444444444444',
		        '0x9999999999999999999999999999999999999999', $1, 50,
		        'ORDER_MATCH', 'DETECTED')`, daAddr); err != nil {
		t.Fatalf("seed inflight deposit: %v", err)
	}
	n, err := s.ReevaluateExpiredOrders(context.Background(), time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("in-flight deposit must block expiry, migrated = %d", n)
	}
	if st := orderStatus(t, pool, o.ID); st != types.OrderAwaitingPayment {
		t.Fatalf("status = %s, want AWAITING_PAYMENT", st)
	}
}

// ---------------------------------------------------------------------------
// 人工审核闭环
// ---------------------------------------------------------------------------

func TestReviewAcceptUnderpaid(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "rev-1", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "60")
	if _, err := s.ReevaluateExpiredOrders(context.Background(), time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	out, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewAcceptUnderpaid,
	})
	if err != nil {
		t.Fatalf("ReviewOrder: %v", err)
	}
	if out.Status != types.OrderPaid {
		t.Fatalf("status = %s, want PAID", out.Status)
	}
	// 结算: received 60, fee = 60*100/10000 = 0 (floor) -> MA 60, FEE 0.
	// bps=100 -> fee = floor(6000/10000)=0; 全额入 MERCHANT_AVAILABLE.
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountMerchantAvailable); got != "60" {
		t.Fatalf("MERCHANT_AVAILABLE = %s, want 60", got)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountSuspenseRefundableLiab); got != "0" {
		t.Fatalf("refundable = %s, want 0", got)
	}
	if out.SettledAt == nil {
		t.Error("settled_at must be set")
	}
	// 终态拒绝再迁移.
	if _, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewAcceptUnderpaid,
	}); !errors.Is(err, ports.ErrInvalidTransition) {
		t.Fatalf("re-review must fail, got %v", err)
	}
}

func TestReviewAcceptLatePaid_FeeSplit(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "rev-2", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "100")
	// 直接置 LATE_PAID_REVIEW 并将 escrow 转挂 refundable (模拟迟付挂起态).
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_orders SET status='LATE_PAID_REVIEW' WHERE id=$1`, o.ID); err != nil {
		t.Fatal(err)
	}
	l := s.ledger
	if _, err := l.PostEntry(context.Background(), ports.PostEntryRequest{
		Template:    types.TemplateOrderSuspense,
		ReferenceID: "order_suspense:" + o.ID.String(),
		Legs: []ports.PostEntryLeg{
			{OwnerMerchantID: o.MerchantID, ChainID: testChain, AssetSymbol: "USDC",
				Account: types.AccountOrderEscrowLiab, Direction: types.DirectionDebit,
				Amount: types.MustParseU256("100")},
			{OwnerMerchantID: o.MerchantID, ChainID: testChain, AssetSymbol: "USDC",
				Account: types.AccountSuspenseRefundableLiab, Direction: types.DirectionCredit,
				Amount: types.MustParseU256("100")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// 商户费率 100bps: fee = floor(10000*100/10000)=100? received=100 -> fee=1.
	out, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewAcceptLatePaid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != types.OrderPaid {
		t.Fatalf("status = %s, want PAID", out.Status)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountMerchantAvailable); got != "99" {
		t.Fatalf("MERCHANT_AVAILABLE = %s, want 99", got)
	}
	if got := balanceOf(t, pool, types.SystemMerchantID, "USDC", types.AccountFeeRevenue); got != "1" {
		t.Fatalf("FEE_REVENUE = %s, want 1", got)
	}
}

func TestReviewCreateAndCancelRefund(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "rev-3", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "60")
	if _, err := s.ReevaluateExpiredOrders(context.Background(), time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	to := types.HexToAddress("0x5555555555555555555555555555555555555555")
	// FIX-D1: 审核退款必带票据 — 先验证缺失票据被拒.
	if _, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewCreateRefund, RefundToAddress: &to,
	}); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("CREATE_REFUND without ticket must fail ErrInvalidRefundTicket, got %v", err)
	}
	expiry := uint64(time.Now().Add(6 * time.Hour).Unix())
	ticketUUID := uuid.New()
	out, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewCreateRefund, RefundToAddress: &to,
		RefundTicket: buildTestTicket(uint64(testChain), testToken, to,
			types.MustParseU256("60"), ticketUUID, 0, expiry),
	})
	if err != nil {
		t.Fatalf("CREATE_REFUND: %v", err)
	}
	if out.Status != types.OrderRefundInProgress {
		t.Fatalf("status = %s, want REFUND_IN_PROGRESS", out.Status)
	}
	// 退款单: SUSPENSE_REFUNDABLE -> PAYOUT_LOCKED; 主键 == 票据内嵌 uuid.
	var payoutID uuid.UUID
	var kind, src string
	var storedTicket []byte
	var retrySeq int32
	err = pool.QueryRow(context.Background(), `
		SELECT id, payout_kind, source_account_type, admin_refund_ticket, retry_seq
		FROM merchant_payouts
		WHERE source_order_id = $1`, o.ID).Scan(&payoutID, &kind, &src, &storedTicket, &retrySeq)
	if err != nil {
		t.Fatalf("find refund payout: %v", err)
	}
	if payoutID != ticketUUID {
		t.Fatalf("payout id = %s, want ticket-bound uuid %s", payoutID, ticketUUID)
	}
	if kind != types.PayoutKindCustomerRefund.String() || src != types.AccountSuspenseRefundableLiab.String() {
		t.Fatalf("payout kind=%s source=%s", kind, src)
	}
	if len(storedTicket) != 191 {
		t.Fatalf("admin_refund_ticket = %dB, want 191B (127B payload || 64B sig)", len(storedTicket))
	}
	if retrySeq != 0 {
		t.Fatalf("retry_seq = %d, want 0", retrySeq)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountPayoutLockedLiab); got != "60" {
		t.Fatalf("locked = %s, want 60", got)
	}
	// 取消 -> 解冻 + 订单回 LATE_PAID_REVIEW.
	out2, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewCancelRefund,
	})
	if err != nil {
		t.Fatalf("CANCEL_REFUND: %v", err)
	}
	if out2.Status != types.OrderLatePaidReview {
		t.Fatalf("status = %s, want LATE_PAID_REVIEW", out2.Status)
	}
	if st := payoutStatus(t, pool, payoutID); st != types.PayoutCancelledUnlocked {
		t.Fatalf("payout = %s, want CANCELLED_UNLOCKED", st)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountPayoutLockedLiab); got != "0" {
		t.Fatalf("locked = %s after cancel, want 0", got)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountSuspenseRefundableLiab); got != "60" {
		t.Fatalf("refundable = %s after cancel, want 60", got)
	}
	// FIX-D1 回归: 已取消的退款单不得阻塞再次审核建单
	// (idem 复查排除 CANCELLED_UNLOCKED).
	out3, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewCreateRefund, RefundToAddress: &to,
		RefundTicket: buildTestTicket(uint64(testChain), testToken, to,
			types.MustParseU256("60"), uuid.New(), 0, expiry),
	})
	if err != nil {
		t.Fatalf("re-create refund after cancel: %v", err)
	}
	if out3.Status != types.OrderRefundInProgress {
		t.Fatalf("re-created refund status = %s", out3.Status)
	}
}

// ---------------------------------------------------------------------------
// RequestPayout / CancelPayout / OnPayoutConfirmed 全闭环
// ---------------------------------------------------------------------------

func TestPayoutLifecycle_Withdrawal(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	// 备付: 结算款入 MERCHANT_AVAILABLE.
	o := createTestOrder(t, s, "po-0", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "100")
	if _, err := s.ReevaluateExpiredOrders(ctx, time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewOrder(ctx, ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewAcceptUnderpaid,
	}); err != nil {
		t.Fatal(err)
	}
	// 商户提现 40 (FIX-D3: 先播种 ACTIVE 白名单记录).
	wdAddr := types.HexToAddress("0x6666666666666666666666666666666666666666")
	seedWhitelist(t, pool, wdAddr)
	p, err := s.RequestPayout(ctx, ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "wd-1", RequestHash: "h-wd-1",
		Kind:          types.PayoutKindWithdrawal,
		SourceAccount: types.AccountMerchantAvailable,
		ChainID:       testChain, AssetSymbol: "USDC",
		ToAddress: wdAddr,
		Amount:    types.MustParseU256("40"),
	})
	if err != nil {
		t.Fatalf("RequestPayout: %v", err)
	}
	if p.Status != types.PayoutLockedQueued {
		t.Fatalf("payout status = %s", p.Status)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountMerchantAvailable); got != "59" {
		t.Fatalf("available = %s, want 59", got)
	}
	// 幂等重放.
	p2, err := s.RequestPayout(ctx, ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "wd-1", RequestHash: "h-wd-1",
		Kind: types.PayoutKindWithdrawal, SourceAccount: types.AccountMerchantAvailable,
		ChainID: testChain, AssetSymbol: "USDC",
		ToAddress: p.ToAddress, Amount: types.MustParseU256("40"),
	})
	if err != nil || p2.ID != p.ID {
		t.Fatalf("idempotent payout replay: %v / %v", p2.ID, err)
	}
	// 广播 -> 确认: 扣 CUSTODY_HOT_PAYOUT.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
		VALUES ('00000000-0000-0000-0000-000000000000', 31337, 'USDC', 'CUSTODY_HOT_PAYOUT','DEBIT', 40)`); err != nil {
		t.Fatal(err)
	}
	if err := s.OnPayoutInFlight(ctx, p.ID); err != nil {
		t.Fatalf("OnPayoutInFlight: %v", err)
	}
	if err := s.OnPayoutConfirmed(ctx, p.ID); err != nil {
		t.Fatalf("OnPayoutConfirmed: %v", err)
	}
	if st := payoutStatus(t, pool, p.ID); st != types.PayoutCompleted {
		t.Fatalf("payout = %s, want COMPLETED", st)
	}
	if got := balanceOf(t, pool, types.SystemMerchantID, "USDC", types.AccountCustodyHotPayout); got != "0" {
		t.Fatalf("custody hot = %s, want 0", got)
	}
}

func TestCancelPayout_Unfreezes(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	o := createTestOrder(t, s, "po-1", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "100")
	if _, err := s.ReevaluateExpiredOrders(ctx, time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewOrder(ctx, ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewAcceptUnderpaid,
	}); err != nil {
		t.Fatal(err)
	}
	wdAddr := types.HexToAddress("0x6666666666666666666666666666666666666666")
	seedWhitelist(t, pool, wdAddr)
	p, err := s.RequestPayout(ctx, ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "wd-c", RequestHash: "h-wd-c",
		Kind: types.PayoutKindWithdrawal, SourceAccount: types.AccountMerchantAvailable,
		ChainID: testChain, AssetSymbol: "USDC",
		ToAddress: wdAddr,
		Amount:    types.MustParseU256("30"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.CancelPayout(ctx, p.ID)
	if err != nil {
		t.Fatalf("CancelPayout: %v", err)
	}
	if out.Status != types.PayoutCancelledUnlocked {
		t.Fatalf("status = %s", out.Status)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountMerchantAvailable); got != "99" {
		t.Fatalf("available = %s after unfreeze, want 99", got)
	}
	// 终态拒绝再取消.
	if _, err := s.CancelPayout(ctx, p.ID); !errors.Is(err, ports.ErrTerminalState) {
		t.Fatalf("re-cancel must fail ErrTerminalState, got %v", err)
	}
}

func TestRefundCompletion_RefundsOrder(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	o := createTestOrder(t, s, "po-2", "100", time.Now().Add(-time.Minute))
	seedEscrow(t, pool, s, o, "60")
	if _, err := s.ReevaluateExpiredOrders(ctx, time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	to := types.HexToAddress("0x7777777777777777777777777777777777777777")
	if _, err := s.ReviewOrder(ctx, ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewCreateRefund, RefundToAddress: &to,
		RefundTicket: buildTestTicket(uint64(testChain), testToken, to,
			types.MustParseU256("60"), uuid.New(), 0, uint64(time.Now().Add(time.Hour).Unix())),
	}); err != nil {
		t.Fatal(err)
	}
	var payoutID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM merchant_payouts WHERE source_order_id=$1`, o.ID).Scan(&payoutID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
		VALUES ('00000000-0000-0000-0000-000000000000', 31337, 'USDC', 'CUSTODY_HOT_PAYOUT','DEBIT', 60)`); err != nil {
		t.Fatal(err)
	}
	if err := s.OnPayoutInFlight(ctx, payoutID); err != nil {
		t.Fatal(err)
	}
	if err := s.OnPayoutConfirmed(ctx, payoutID); err != nil {
		t.Fatalf("OnPayoutConfirmed: %v", err)
	}
	if st := orderStatus(t, pool, o.ID); st != types.OrderRefunded {
		t.Fatalf("order = %s, want REFUNDED", st)
	}
	if st := payoutStatus(t, pool, payoutID); st != types.PayoutCompleted {
		t.Fatalf("payout = %s, want COMPLETED", st)
	}
}

// ---------------------------------------------------------------------------
// 深回滚追偿与解冻闭环
// ---------------------------------------------------------------------------

func TestUnfreezeMerchant_AfterReorg(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	// 构造追偿应收: 直接记账 (模板 9 允许 MRR -> CU? 用直接 SQL 更简).
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
		VALUES ($1, 31337, 'USDC', 'MERCHANT_REORG_RECEIVABLE','DEBIT', 40)`, testMerchant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE merchants SET payout_frozen = true WHERE id = $1`, testMerchant); err != nil {
		t.Fatal(err)
	}
	// 未清零拒绝解冻.
	if err := s.UnfreezeMerchant(ctx, testMerchant); err == nil {
		t.Fatal("unfreeze with outstanding receivable must fail")
	}
	// 核销清零 (运营补款后) -> 解冻成功.
	if _, err := pool.Exec(ctx, `
		UPDATE ledger_accounts SET balance = 0
		WHERE owner_merchant_id = $1 AND account_type = 'MERCHANT_REORG_RECEIVABLE'`, testMerchant); err != nil {
		t.Fatal(err)
	}
	if err := s.UnfreezeMerchant(ctx, testMerchant); err != nil {
		t.Fatalf("UnfreezeMerchant: %v", err)
	}
	var frozen bool
	if err := pool.QueryRow(ctx, `SELECT payout_frozen FROM merchants WHERE id=$1`, testMerchant).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen {
		t.Fatal("payout_frozen must be cleared")
	}
}

func TestResolveReversal(t *testing.T) {
	s, pool := newService(t)
	o := createTestOrder(t, s, "rev-9", "100", time.Now().Add(time.Hour))
	// 人工置 SETTLED_REVERSED + 追偿应收未清.
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_orders SET status='SETTLED_REVERSED' WHERE id=$1`, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
		VALUES ($1, 31337, 'USDC', 'MERCHANT_REORG_RECEIVABLE','DEBIT', 25)`, testMerchant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewResolveReversal,
	}); !errors.Is(err, ports.ErrInvalidTransition) {
		t.Fatalf("resolve with outstanding receivable must fail, got %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE ledger_accounts SET balance = 0 WHERE account_type='MERCHANT_REORG_RECEIVABLE'`); err != nil {
		t.Fatal(err)
	}
	out, err := s.ReviewOrder(context.Background(), ports.ReviewOrderRequest{
		OrderID: o.ID, Action: ports.ReviewResolveReversal,
	})
	if err != nil {
		t.Fatalf("RESOLVE_REVERSAL: %v", err)
	}
	if out.Status != types.OrderReversalResolved {
		t.Fatalf("status = %s, want REVERSAL_RESOLVED", out.Status)
	}
}

func TestRequestPayout_FrozenMerchantRejected(t *testing.T) {
	s, pool := newService(t)
	if _, err := pool.Exec(context.Background(),
		`UPDATE merchants SET payout_frozen = true WHERE id = $1`, testMerchant); err != nil {
		t.Fatal(err)
	}
	_, err := s.RequestPayout(context.Background(), ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "wd-f", RequestHash: "h-wd-f",
		Kind: types.PayoutKindWithdrawal, SourceAccount: types.AccountMerchantAvailable,
		ChainID: testChain, AssetSymbol: "USDC",
		ToAddress: types.HexToAddress("0x6666666666666666666666666666666666666666"),
		Amount:    types.MustParseU256("10"),
	})
	if !errors.Is(err, ports.ErrMerchantFrozen) {
		t.Fatalf("frozen merchant must fail ErrMerchantFrozen, got %v", err)
	}
}

// ===========================================================================
// FIX-D1 回归: CUSTOMER_REFUND payout_uuid == 票据内嵌 uuid == merchant_payouts.id
// (signer SignTransactionRequest.payout_uuid 与票据逐字节比对).
// ===========================================================================

func TestRequestPayout_RefundTicketUUIDBinding(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	seedSuspenseBalance(t, pool, types.AccountSuspenseRefundableLiab, "100")

	to := types.HexToAddress("0x7777777777777777777777777777777777777777")
	puuid := uuid.New()
	expiry := uint64(time.Now().Add(time.Hour).Unix())
	req := ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "rf-1", RequestHash: "h-rf-1",
		Kind:          types.PayoutKindCustomerRefund,
		SourceAccount: types.AccountSuspenseRefundableLiab,
		ChainID:       testChain, AssetSymbol: "USDC",
		ToAddress: to, Amount: types.MustParseU256("60"),
		PayoutID: puuid,
		RefundTicket: buildTestTicket(uint64(testChain), testToken, to,
			types.MustParseU256("60"), puuid, 0, expiry),
	}
	p, err := s.RequestPayout(ctx, req)
	if err != nil {
		t.Fatalf("RequestPayout: %v", err)
	}
	// 主键 == 客户端 payout_uuid (票据内嵌 uuid) — signer 绑定的根基.
	if p.ID != puuid {
		t.Fatalf("payout id = %s, want client payout_uuid %s", p.ID, puuid)
	}
	if p.RetrySeq != 0 {
		t.Fatalf("retry_seq = %d, want 0", p.RetrySeq)
	}
	if len(p.AdminRefundTicket) != refundTicketPackedLen {
		t.Fatalf("admin_refund_ticket = %dB, want %dB", len(p.AdminRefundTicket), refundTicketPackedLen)
	}
	// 存库字节: 127B 载荷 || 64B 签名, 载荷 [99:115] == payout_uuid raw16.
	if got := p.AdminRefundTicket[99:115]; !bytes.Equal(got, puuid[:]) {
		t.Fatalf("stored ticket embedded uuid = %x, want %x", got, puuid)
	}

	// 幂等重放: 同 (merchant, idem_key, hash) 返回既有行, 不重复冻结.
	p2, err := s.RequestPayout(ctx, req)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if p2.ID != p.ID {
		t.Fatalf("replay id = %s, want %s", p2.ID, p.ID)
	}
	if got := balanceOf(t, pool, testMerchant, "USDC", types.AccountPayoutLockedLiab); got != "60" {
		t.Fatalf("locked = %s after replay, want 60 (no double freeze)", got)
	}

	// PK 锚: 不同 idem_key 但同 payout_uuid + 同 request_hash -> 既有行
	// (崩溃重试: 客户端复用 payout_uuid 重签, 服务端 PK 冲突回退预读).
	req2 := req
	req2.IdempotencyKey = "rf-1-retry"
	p3, err := s.RequestPayout(ctx, req2)
	if err != nil {
		t.Fatalf("PK-anchor replay: %v", err)
	}
	if p3.ID != p.ID {
		t.Fatalf("PK-anchor id = %s, want %s", p3.ID, p.ID)
	}

	// 冲突: 同 payout_uuid 但 request_hash 不同 -> ErrIdempotencyConflict.
	req3 := req
	req3.IdempotencyKey = "rf-1-other"
	req3.RequestHash = "h-different"
	if _, err := s.RequestPayout(ctx, req3); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("hash mismatch must fail ErrIdempotencyConflict, got %v", err)
	}
}

func TestRequestPayout_RefundTicketRejected(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	seedSuspenseBalance(t, pool, types.AccountSuspenseRefundableLiab, "100")
	to := types.HexToAddress("0x7777777777777777777777777777777777777777")
	expiry := uint64(time.Now().Add(time.Hour).Unix())
	puuid := uuid.New()
	base := ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "rf-x", RequestHash: "h-rf-x",
		Kind:          types.PayoutKindCustomerRefund,
		SourceAccount: types.AccountSuspenseRefundableLiab,
		ChainID:       testChain, AssetSymbol: "USDC",
		ToAddress: to, Amount: types.MustParseU256("60"), PayoutID: puuid,
	}

	// 1) 票据缺失.
	if _, err := s.RequestPayout(ctx, base); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("missing ticket must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 2) 票据内嵌 uuid != PayoutID.
	r2 := base
	r2.IdempotencyKey = "rf-x2"
	r2.RefundTicket = buildTestTicket(uint64(testChain), testToken, to,
		types.MustParseU256("60"), uuid.New(), 0, expiry)
	if _, err := s.RequestPayout(ctx, r2); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("uuid mismatch must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 3) 票据内嵌 retry_seq != 0 (新退款必须是 0 — 重试留给换票重签).
	r3 := base
	r3.IdempotencyKey = "rf-x3"
	r3.RefundTicket = buildTestTicket(uint64(testChain), testToken, to,
		types.MustParseU256("60"), puuid, 1, expiry)
	if _, err := s.RequestPayout(ctx, r3); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("retry_seq=1 must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 4) 载荷长度不对.
	r4 := base
	r4.IdempotencyKey = "rf-x4"
	r4.RefundTicket = &ports.RefundTicket{
		SignedPayload:  make([]byte, 126),
		AdminSignature: make([]byte, 64),
	}
	if _, err := s.RequestPayout(ctx, r4); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("126B payload must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 5) 签名长度不对.
	r5 := base
	r5.IdempotencyKey = "rf-x5"
	r5.RefundTicket = buildTestTicket(uint64(testChain), testToken, to,
		types.MustParseU256("60"), puuid, 0, expiry)
	r5.RefundTicket.AdminSignature = make([]byte, 32)
	if _, err := s.RequestPayout(ctx, r5); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("32B sig must fail ErrInvalidRefundTicket, got %v", err)
	}

	// 6) 字段交叉校验: 载荷 vs 请求 — chain_id 不符.
	r6 := base
	r6.IdempotencyKey = "rf-x6"
	r6.RefundTicket = buildTestTicket(1 /* wrong chain */, testToken, to,
		types.MustParseU256("60"), puuid, 0, expiry)
	if _, err := s.RequestPayout(ctx, r6); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("chain mismatch must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 7) token 不符 (签了别的币).
	r7 := base
	r7.IdempotencyKey = "rf-x7"
	r7.RefundTicket = buildTestTicket(uint64(testChain),
		types.HexToAddress("0x1111111111111111111111111111111111111111"), to,
		types.MustParseU256("60"), puuid, 0, expiry)
	if _, err := s.RequestPayout(ctx, r7); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("token mismatch must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 8) to_address 不符 (签了别的收款方).
	r8 := base
	r8.IdempotencyKey = "rf-x8"
	r8.RefundTicket = buildTestTicket(uint64(testChain), testToken,
		types.HexToAddress("0x8888888888888888888888888888888888888888"),
		types.MustParseU256("60"), puuid, 0, expiry)
	if _, err := s.RequestPayout(ctx, r8); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("to mismatch must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 9) exact_amount 不符 (签的金额与请求不等).
	r9 := base
	r9.IdempotencyKey = "rf-x9"
	r9.RefundTicket = buildTestTicket(uint64(testChain), testToken, to,
		types.MustParseU256("61"), puuid, 0, expiry)
	if _, err := s.RequestPayout(ctx, r9); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("amount mismatch must fail ErrInvalidRefundTicket, got %v", err)
	}
	// 10) expiry 已过.
	r10 := base
	r10.IdempotencyKey = "rf-x10"
	r10.RefundTicket = buildTestTicket(uint64(testChain), testToken, to,
		types.MustParseU256("60"), puuid, 0, 1)
	if _, err := s.RequestPayout(ctx, r10); !errors.Is(err, ErrInvalidRefundTicket) {
		t.Fatalf("expired ticket must fail ErrInvalidRefundTicket, got %v", err)
	}
}

// ===========================================================================
// FIX-D3 回归: WITHDRAWAL 收款地址白名单前置校验.
// ===========================================================================

func TestRequestPayout_WhitelistEnforced(t *testing.T) {
	s, pool := newService(t)
	ctx := context.Background()
	seedSuspenseBalance(t, pool, types.AccountMerchantAvailable, "100")
	to := types.HexToAddress("0x6666666666666666666666666666666666666666")
	req := ports.PayoutRequest{
		MerchantID: testMerchant, IdempotencyKey: "wd-w", RequestHash: "h-wd-w",
		Kind:          types.PayoutKindWithdrawal,
		SourceAccount: types.AccountMerchantAvailable,
		ChainID:       testChain, AssetSymbol: "USDC",
		ToAddress: to, Amount: types.MustParseU256("30"),
	}

	// 1) 完全无记录 -> ErrAddressNotWhitelisted.
	if _, err := s.RequestPayout(ctx, req); !errors.Is(err, ErrAddressNotWhitelisted) {
		t.Fatalf("unlisted address must fail ErrAddressNotWhitelisted, got %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM merchant_withdrawal_addresses WHERE merchant_id = $1 AND chain_id = 31337`,
			testMerchant)
	})
	setMWA := func(status string, validUntil time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO merchant_withdrawal_addresses
			  (merchant_id, chain_id, address, admin_approval_sig, valid_until, status)
			VALUES ($1, 31337, $2, '\x00'::bytea, $3, $4)
			ON CONFLICT (merchant_id, chain_id, address)
			DO UPDATE SET status = $4, valid_until = $3`,
			testMerchant, addrHex(to), validUntil, status); err != nil {
			t.Fatalf("set mwa: %v", err)
		}
	}
	future := time.Now().Add(24 * time.Hour)

	// 2) PENDING_COOLDOWN -> 拒.
	setMWA("PENDING_COOLDOWN", future)
	if _, err := s.RequestPayout(ctx, req); !errors.Is(err, ErrAddressNotWhitelisted) {
		t.Fatalf("PENDING_COOLDOWN must fail ErrAddressNotWhitelisted, got %v", err)
	}
	// 3) REVOKED -> 拒.
	setMWA("REVOKED", future)
	if _, err := s.RequestPayout(ctx, req); !errors.Is(err, ErrAddressNotWhitelisted) {
		t.Fatalf("REVOKED must fail ErrAddressNotWhitelisted, got %v", err)
	}
	// 4) ACTIVE 但 valid_until 已过 -> 拒 (租期耗尽按失效论).
	setMWA("ACTIVE", time.Now().Add(-time.Hour))
	if _, err := s.RequestPayout(ctx, req); !errors.Is(err, ErrAddressNotWhitelisted) {
		t.Fatalf("expired lease must fail ErrAddressNotWhitelisted, got %v", err)
	}
	// 5) ACTIVE + 有效租期 -> 放行.
	setMWA("ACTIVE", future)
	p, err := s.RequestPayout(ctx, req)
	if err != nil {
		t.Fatalf("whitelisted withdrawal: %v", err)
	}
	if p.Status != types.PayoutLockedQueued {
		t.Fatalf("status = %s, want LOCKED_QUEUED", p.Status)
	}
}

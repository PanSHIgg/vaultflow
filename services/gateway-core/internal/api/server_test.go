package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/sse"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// fakes — ReadStore / OrderService 桩 (Wave-2 I1 以 PG/真实服务替换)
// ---------------------------------------------------------------------------

type fakeStore struct {
	resolveAPIKey         func(ctx context.Context, hash string) (types.Merchant, error)
	getOrderDeposits      func(ctx context.Context, orderID uuid.UUID, limit int) ([]types.OnchainDeposit, error)
	getSupportedAsset     func(ctx context.Context, c types.ChainID, sym string) (types.SupportedAsset, error)
	getDepositAddress     func(ctx context.Context, id uuid.UUID) (types.DepositAddress, error)
	findOrderRefundPayout func(ctx context.Context, orderID uuid.UUID) (types.MerchantPayout, error)
}

func (f *fakeStore) ResolveAPIKey(ctx context.Context, h string) (types.Merchant, error) {
	if f.resolveAPIKey != nil {
		return f.resolveAPIKey(ctx, h)
	}
	return types.Merchant{}, ports.ErrNotFound
}
func (f *fakeStore) GetMerchant(context.Context, uuid.UUID) (types.Merchant, error) {
	return types.Merchant{}, ports.ErrNotFound
}
func (f *fakeStore) MerchantReceivableBalance(context.Context, uuid.UUID) (types.U256, error) {
	return types.U256{}, nil
}
func (f *fakeStore) GetDepositAddress(ctx context.Context, id uuid.UUID) (types.DepositAddress, error) {
	if f.getDepositAddress != nil {
		return f.getDepositAddress(ctx, id)
	}
	return types.DepositAddress{}, ports.ErrNotFound
}
func (f *fakeStore) GetSupportedAsset(ctx context.Context, c types.ChainID, sym string) (types.SupportedAsset, error) {
	if f.getSupportedAsset != nil {
		return f.getSupportedAsset(ctx, c, sym)
	}
	return types.SupportedAsset{}, ports.ErrNotFound
}
func (f *fakeStore) ListSupportedAssets(context.Context) ([]types.SupportedAsset, error) {
	return nil, nil
}
func (f *fakeStore) ListOrders(context.Context, OrderListQuery) (OrderPage, error) {
	return OrderPage{Data: []types.PaymentOrder{}}, nil
}
func (f *fakeStore) ListOrderDeposits(ctx context.Context, orderID uuid.UUID, limit int) ([]types.OnchainDeposit, error) {
	if f.getOrderDeposits != nil {
		return f.getOrderDeposits(ctx, orderID, limit)
	}
	return nil, nil
}
func (f *fakeStore) ListPayouts(context.Context, PayoutListQuery) (PayoutPage, error) {
	return PayoutPage{Data: []types.MerchantPayout{}}, nil
}
func (f *fakeStore) FindOrderRefundPayout(ctx context.Context, orderID uuid.UUID) (types.MerchantPayout, error) {
	if f.findOrderRefundPayout != nil {
		return f.findOrderRefundPayout(ctx, orderID)
	}
	return types.MerchantPayout{}, ports.ErrNotFound
}
func (f *fakeStore) FindIntentByReference(context.Context, uuid.UUID) (types.OutboundTxIntent, error) {
	return types.OutboundTxIntent{}, ports.ErrNotFound
}
func (f *fakeStore) LatestFinalizedAttempt(context.Context, uuid.UUID) (types.OutboundTxAttempt, error) {
	return types.OutboundTxAttempt{}, ports.ErrNotFound
}
func (f *fakeStore) CreateWithdrawalAddress(context.Context, NewWithdrawalAddress) (types.MerchantWithdrawalAddress, error) {
	return types.MerchantWithdrawalAddress{}, ports.ErrUnavailable
}
func (f *fakeStore) GetWithdrawalAddress(context.Context, uuid.UUID) (types.MerchantWithdrawalAddress, error) {
	return types.MerchantWithdrawalAddress{}, ports.ErrNotFound
}
func (f *fakeStore) ListWithdrawalAddresses(context.Context, WhitelistQuery) (WhitelistPage, error) {
	return WhitelistPage{Data: []types.MerchantWithdrawalAddress{}}, nil
}
func (f *fakeStore) MarkWithdrawalAddressRevoked(context.Context, uuid.UUID) (types.MerchantWithdrawalAddress, error) {
	return types.MerchantWithdrawalAddress{}, ports.ErrNotFound
}
func (f *fakeStore) ListLedgerAccounts(context.Context, LedgerAccountQuery) ([]types.LedgerAccount, error) {
	return nil, nil
}
func (f *fakeStore) ListLedgerTransactions(context.Context, LedgerTxQuery) (LedgerTxPage, error) {
	return LedgerTxPage{Data: []types.LedgerTransaction{}}, nil
}
func (f *fakeStore) ListLedgerEntries(context.Context, LedgerEntryQuery) (LedgerEntryPage, error) {
	return LedgerEntryPage{Data: []LedgerEntryView{}}, nil
}
func (f *fakeStore) GetChainSyncCursor(context.Context, types.ChainID) (types.ChainSyncCursor, error) {
	return types.ChainSyncCursor{}, ports.ErrNotFound
}
func (f *fakeStore) ListNonceCursors(context.Context, *types.ChainID) ([]NonceCursorView, error) {
	return nil, nil
}
func (f *fakeStore) ListOutboundIntents(context.Context, OutboundListQuery) (OutboundPage, error) {
	return OutboundPage{Data: []types.OutboundTxIntent{}}, nil
}
func (f *fakeStore) ListQuarantinedVaults(context.Context, QuarantineQuery) (QuarantinePage, error) {
	return QuarantinePage{Data: []QuarantinedVaultView{}}, nil
}

// fakeOrders ports.OrderService 桩.
type fakeOrders struct {
	createFn        func(ctx context.Context, req ports.CreateOrderRequest) (types.PaymentOrder, error)
	getFn           func(ctx context.Context, id uuid.UUID) (types.PaymentOrder, error)
	requestPayoutFn func(ctx context.Context, req ports.PayoutRequest) (types.MerchantPayout, error)
	reviewFn        func(ctx context.Context, req ports.ReviewOrderRequest) (types.PaymentOrder, error)
	calls           int
}

func (f *fakeOrders) CreateOrder(ctx context.Context, req ports.CreateOrderRequest) (types.PaymentOrder, error) {
	f.calls++
	if f.createFn != nil {
		return f.createFn(ctx, req)
	}
	return types.PaymentOrder{}, ports.ErrUnavailable
}
func (f *fakeOrders) GetOrder(ctx context.Context, id uuid.UUID) (types.PaymentOrder, error) {
	if f.getFn != nil {
		return f.getFn(ctx, id)
	}
	return types.PaymentOrder{}, ports.ErrNotFound
}
func (f *fakeOrders) ReviewOrder(ctx context.Context, req ports.ReviewOrderRequest) (types.PaymentOrder, error) {
	if f.reviewFn != nil {
		return f.reviewFn(ctx, req)
	}
	return types.PaymentOrder{}, ports.ErrInvalidTransition
}
func (f *fakeOrders) RequestPayout(ctx context.Context, req ports.PayoutRequest) (types.MerchantPayout, error) {
	if f.requestPayoutFn != nil {
		return f.requestPayoutFn(ctx, req)
	}
	return types.MerchantPayout{}, ports.ErrUnavailable
}
func (f *fakeOrders) CancelPayout(context.Context, uuid.UUID) (types.MerchantPayout, error) {
	return types.MerchantPayout{}, ports.ErrNotFound
}
func (f *fakeOrders) GetPayout(context.Context, uuid.UUID) (types.MerchantPayout, error) {
	return types.MerchantPayout{}, ports.ErrNotFound
}
func (f *fakeOrders) UnfreezeMerchant(context.Context, uuid.UUID) error { return nil }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var testMerchant = types.Merchant{
	ID:   uuid.MustParse("11111111-2222-3333-4444-555555555555"),
	Name: "test-merchant",
}

func apiKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newTestServer(t *testing.T, mutate func(*Deps)) *Server {
	t.Helper()
	deps := Deps{
		Config:      DefaultConfig(),
		Store:       &fakeStore{},
		AdminJWTKey: []byte("test-admin-secret"),
		nowFn:       func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&deps)
	}
	return NewServer(deps)
}

func doJSON(t *testing.T, srv http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func adminJWT(t *testing.T, key []byte, sub string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": sub, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return tok
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	return b.Error.Code
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

func TestMerchantAuth(t *testing.T) {
	store := &fakeStore{resolveAPIKey: func(_ context.Context, hash string) (types.Merchant, error) {
		if hash == apiKeyHash("good-key") {
			return testMerchant, nil
		}
		return types.Merchant{}, ports.ErrNotFound
	}}
	srv := newTestServer(t, func(d *Deps) { d.Store = store })

	rec := doJSON(t, srv, "GET", "/v1/orders", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status=%d want 401", rec.Code)
	}
	if errCode(t, rec) != string(CodeUnauthorized) {
		t.Fatalf("code=%s want UNAUTHORIZED", errCode(t, rec))
	}

	rec = doJSON(t, srv, "GET", "/v1/orders", nil, map[string]string{"X-Api-Key": "bad"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: status=%d want 401", rec.Code)
	}

	rec = doJSON(t, srv, "GET", "/v1/orders", nil, map[string]string{"X-Api-Key": "good-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("good key: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminAuth(t *testing.T) {
	srv := newTestServer(t, nil)

	rec := doJSON(t, srv, "GET", "/v1/admin/signer/health", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: status=%d", rec.Code)
	}

	bad := adminJWT(t, []byte("wrong-secret"), "admin-1")
	rec = doJSON(t, srv, "GET", "/v1/admin/signer/health", nil,
		map[string]string{"Authorization": "Bearer " + bad})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: status=%d", rec.Code)
	}

	good := adminJWT(t, []byte("test-admin-secret"), "admin-1")
	rec = doJSON(t, srv, "GET", "/v1/admin/signer/health", nil,
		map[string]string{"Authorization": "Bearer " + good})
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid JWT rejected")
	}
}

func TestAdminAuth_Expired(t *testing.T) {
	srv := newTestServer(t, nil)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "admin-1", "exp": time.Now().Add(-time.Hour).Unix(),
	}).SignedString([]byte("test-admin-secret"))
	if err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, srv, "GET", "/v1/admin/signer/health", nil,
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired JWT: status=%d want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

func TestIdempotency(t *testing.T) {
	orderID := uuid.New()
	orders := &fakeOrders{createFn: func(_ context.Context, req ports.CreateOrderRequest) (types.PaymentOrder, error) {
		return types.PaymentOrder{
			ID: orderID, MerchantID: req.MerchantID, ChainID: req.ChainID,
			AssetSymbol: req.AssetSymbol, ExpectedAmount: req.ExpectedAmount,
			Status: types.OrderAwaitingPayment, ExpiresAt: req.ExpiresAt,
			DepositAddressID: uuid.New(),
		}, nil
	}}
	store := &fakeStore{resolveAPIKey: func(_ context.Context, h string) (types.Merchant, error) {
		if h == apiKeyHash("k1") {
			return testMerchant, nil
		}
		return types.Merchant{}, ports.ErrNotFound
	}}
	srv := newTestServer(t, func(d *Deps) { d.Store = store; d.Orders = orders })

	body := map[string]any{"chain_id": 31337, "asset_symbol": "USDC", "amount": "1500000"}
	hdr := map[string]string{"X-Api-Key": "k1", "Idempotency-Key": "idem-1"}

	rec1 := doJSON(t, srv, "POST", "/v1/orders", body, hdr)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), orderID.String()) {
		t.Fatalf("missing order_id: %s", rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), `"expected_amount":"1500000"`) {
		t.Fatalf("amount not serialized as decimal string: %s", rec1.Body.String())
	}
	callsBefore := orders.calls

	rec2 := doJSON(t, srv, "POST", "/v1/orders", body, hdr)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("replay: status=%d", rec2.Code)
	}
	if rec2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatal("missing X-Idempotent-Replay header")
	}
	if rec1.Body.String() != rec2.Body.String() {
		t.Fatal("replayed body differs from first response")
	}
	if orders.calls != callsBefore {
		t.Fatalf("idempotent replay re-entered CreateOrder (%d -> %d)", callsBefore, orders.calls)
	}

	rec3 := doJSON(t, srv, "POST", "/v1/orders",
		map[string]any{"chain_id": 31337, "asset_symbol": "USDC", "amount": "999"}, hdr)
	if rec3.Code != http.StatusConflict {
		t.Fatalf("conflict: status=%d body=%s", rec3.Code, rec3.Body.String())
	}
	if errCode(t, rec3) != string(CodeIdempotencyConflict) {
		t.Fatalf("code=%s want IDEMPOTENCY_CONFLICT", errCode(t, rec3))
	}

	rec4 := doJSON(t, srv, "POST", "/v1/orders", body, map[string]string{"X-Api-Key": "k1"})
	if rec4.Code != http.StatusBadRequest {
		t.Fatalf("missing key: status=%d", rec4.Code)
	}
	if errCode(t, rec4) != string(CodeIdempotencyKeyRequired) {
		t.Fatalf("code=%s want IDEMPOTENCY_KEY_REQUIRED", errCode(t, rec4))
	}
}

// ---------------------------------------------------------------------------
// 频控
// ---------------------------------------------------------------------------

func TestRateLimit_LocalWindow(t *testing.T) {
	store := &fakeStore{resolveAPIKey: func(context.Context, string) (types.Merchant, error) {
		return testMerchant, nil
	}}
	srv := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Limiter = NewRateLimiter(nil, nil)
		d.Config.RateMerchant = RateLimitRule{Limit: 2, Window: time.Minute}
	})
	hdr := map[string]string{"X-Api-Key": "any"}
	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		last = doJSON(t, srv, "GET", "/v1/orders", nil, hdr)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: status=%d want 429", last.Code)
	}
	if errCode(t, last) != string(CodeRateLimited) {
		t.Fatalf("code=%s want RATE_LIMITED", errCode(t, last))
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestRateLimiter_FailOpen(t *testing.T) {
	// redis 客户端指向死连接 -> Allow fail-open 放行 (不阻断支付主流程).
	l := NewRateLimiter(nil, nil)
	if ok, _ := l.Allow(context.Background(), "k", RateLimitRule{Limit: 1, Window: time.Minute}); !ok {
		t.Fatal("local limiter first call should pass")
	}
	if ok, _ := l.Allow(context.Background(), "k", RateLimitRule{Limit: 1, Window: time.Minute}); ok {
		t.Fatal("local limiter second call in window should reject")
	}
	if ok, _ := l.Allow(context.Background(), "k2", RateLimitRule{Limit: 1, Window: time.Minute}); !ok {
		t.Fatal("different key should pass")
	}
}

// ---------------------------------------------------------------------------
// 订单端到端
// ---------------------------------------------------------------------------

func TestCreateOrderValidation(t *testing.T) {
	store := &fakeStore{resolveAPIKey: func(context.Context, string) (types.Merchant, error) {
		return testMerchant, nil
	}}
	srv := newTestServer(t, func(d *Deps) { d.Store = store; d.Orders = &fakeOrders{} })
	hdr := map[string]string{"X-Api-Key": "k", "Idempotency-Key": "i1"}

	// amount 为 JSON number -> 400 (U256.UnmarshalText 拒绝)
	rec := doJSON(t, srv, "POST", "/v1/orders", map[string]any{
		"chain_id": 31337, "asset_symbol": "USDC", "amount": 1500000}, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("numeric amount: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, srv, "POST", "/v1/orders", map[string]any{
		"chain_id": 31337, "asset_symbol": "USDC", "amount": "100", "expires_in_seconds": 10},
		map[string]string{"X-Api-Key": "k", "Idempotency-Key": "i2"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ttl: status=%d", rec.Code)
	}
}

func TestPublicPayment_Unauthenticated(t *testing.T) {
	orderID := uuid.New()
	orders := &fakeOrders{getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
		return types.PaymentOrder{
			ID: orderID, MerchantID: testMerchant.ID, ChainID: 31337,
			AssetSymbol: "USDC", ExpectedAmount: types.U256FromUint64(1500000),
			Status:    types.OrderAwaitingPayment,
			ExpiresAt: time.Now().Add(time.Hour), DepositAddressID: uuid.New(),
		}, nil
	}}
	srv := newTestServer(t, func(d *Deps) { d.Orders = orders })

	rec := doJSON(t, srv, "GET", "/v1/public/payments/"+orderID.String(), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), orderID.String()) {
		t.Fatalf("missing order_id: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// dev-simulator gate
// ---------------------------------------------------------------------------

func TestSimGate_Disabled(t *testing.T) {
	srv := newTestServer(t, nil) // SimEnabled=false
	tok := adminJWT(t, []byte("test-admin-secret"), "ops")
	rec := doJSON(t, srv, "POST", "/v1/sim/anvil/snapshot", map[string]any{},
		map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": "s1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != string(CodeFeatureDisabled) {
		t.Fatalf("code=%s want FEATURE_DISABLED", errCode(t, rec))
	}
}

func TestSimGate_Enabled_AnvilPassthrough(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) {
		d.Config.SimEnabled = true
		d.Anvil = &fakeAnvilPassthrough{result: "0x7"}
	})
	tok := adminJWT(t, []byte("test-admin-secret"), "ops")
	rec := doJSON(t, srv, "POST", "/v1/sim/anvil/snapshot", map[string]any{},
		map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": "s1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"0x7"`) {
		t.Fatalf("result not passed through: %s", rec.Body.String())
	}
}

func TestSimAnvil_WithBody(t *testing.T) {
	var gotParams map[string]any
	srv := newTestServer(t, func(d *Deps) {
		d.Config.SimEnabled = true
		d.Anvil = &fakeAnvilPassthrough2{fn: func(_ context.Context, chain uint64, action string, params map[string]any) (any, error) {
			gotParams = params
			return true, nil
		}}
	})
	tok := adminJWT(t, []byte("test-admin-secret"), "ops")
	rec := doJSON(t, srv, "POST", "/v1/sim/anvil/revert",
		map[string]any{"chain_id": 31337, "params": map[string]any{"snapshot_id": "0x9"}},
		map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": "s2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotParams["snapshot_id"] != "0x9" {
		t.Fatalf("params not passed: %v", gotParams)
	}
}

type fakeAnvilPassthrough2 struct {
	fn func(context.Context, uint64, string, map[string]any) (any, error)
}

func (f *fakeAnvilPassthrough2) Invoke(ctx context.Context, c uint64, a string, p map[string]any) (any, error) {
	return f.fn(ctx, c, a, p)
}

type fakeAnvilPassthrough struct{ result any }

func (f *fakeAnvilPassthrough) Invoke(context.Context, uint64, string, map[string]any) (any, error) {
	return f.result, nil
}

// ---------------------------------------------------------------------------
// SSE handler (in-process streamer)
// ---------------------------------------------------------------------------

func TestStreamPaymentEvents(t *testing.T) {
	orderID := uuid.New()
	orders := &fakeOrders{getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
		return types.PaymentOrder{
			ID: orderID, MerchantID: testMerchant.ID, ChainID: 31337,
			AssetSymbol: "USDC", ExpectedAmount: types.U256FromUint64(1500000),
			Status: types.OrderAwaitingPayment, DepositAddressID: uuid.New(),
		}, nil
	}}
	streamer := &stubStreamer{events: []stubEvent{
		{seq: 1, typ: ports.OrderEventDepositDetected, status: types.OrderAwaitingPayment},
	}}
	srv := newTestServer(t, func(d *Deps) {
		d.Orders = orders
		d.Streamer = streamer
	})

	req := httptest.NewRequest("GET", "/v1/public/payments/"+orderID.String()+"/events", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req.WithContext(ctx))
	body := rec.Body.String()
	if !strings.Contains(body, "retry: 3000") {
		t.Fatalf("missing retry field: %q", body)
	}
	if !strings.Contains(body, "event: block_detected") {
		t.Fatalf("missing event frame: %q", body)
	}
	if !strings.Contains(body, "id: 1") {
		t.Fatalf("missing event id: %q", body)
	}
}

// stubStreamer 简单 EventStreamer: 回放预置事件后挂起至 ctx 结束.
type stubStreamer struct {
	events []stubEvent
}
type stubEvent struct {
	seq    uint64
	typ    ports.OrderEventType
	status types.OrderStatus
}

func (s *stubStreamer) Subscribe(ctx context.Context, orderID uuid.UUID, afterSeq uint64) (<-chan sse.StreamEvent, func(), error) {
	out := make(chan sse.StreamEvent, len(s.events)+1)
	for _, e := range s.events {
		if e.seq > afterSeq {
			out <- sse.StreamEvent{Seq: e.seq, Event: ports.OrderEvent{
				OrderID: orderID, Type: e.typ, Status: e.status,
				OccurredAt: time.Now().UTC(),
			}}
		}
	}
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, func() {}, nil
}

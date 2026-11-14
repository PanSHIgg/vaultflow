package api

// FIX-D1/D3/D4/D5 API 面回归: 退款票据 payout_uuid 绑定 / retry_seq /
// 白名单错误码 / 审核票据透传 / SSE 非终态与 lastEventId / EIP-3009 v 归一化.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/order"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

var (
	apiTestToken    = types.HexToAddress("0x4444444444444444444444444444444444444444")
	apiTestRefundTo = types.HexToAddress("0x7777777777777777777777777777777777777777")
	// 测试签名: 64B 全 0xab (parseEd25519Signature 只校验编码长度).
	apiTestSigHex = "0x" + strings.Repeat("ab", 64)
	// nowFn 固定 2026-01-01T00:00:00Z -> 票据 expiry 必须大于该时刻.
	apiTestExpiry = int64(1893456000) // 2030-01-01
)

func usdcAsset() types.SupportedAsset {
	return types.SupportedAsset{
		ChainID: 31337, AssetSymbol: "USDC", TokenContract: apiTestToken,
		Decimals: 6, SupportsEIP3009: true,
	}
}

func merchantAuthHeaders(idem string) map[string]string {
	return map[string]string{
		"X-Api-Key":       "good-key",
		"Idempotency-Key": idem,
	}
}

func keyedStore(extra *fakeStore) *fakeStore {
	base := &fakeStore{resolveAPIKey: func(_ context.Context, hash string) (types.Merchant, error) {
		if hash == apiKeyHash("good-key") {
			return testMerchant, nil
		}
		return types.Merchant{}, ports.ErrNotFound
	}}
	if extra != nil {
		extra.resolveAPIKey = base.resolveAPIKey
		return extra
	}
	return base
}

func payoutOf(req ports.PayoutRequest, status types.PayoutStatus) types.MerchantPayout {
	return types.MerchantPayout{
		ID: req.PayoutID, MerchantID: req.MerchantID, PayoutKind: req.Kind,
		SourceAccount: req.SourceAccount, ChainID: req.ChainID,
		AssetSymbol: req.AssetSymbol, ToAddress: req.ToAddress,
		Amount: req.Amount, RetrySeq: 0, Status: status,
		CreatedAt: time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// FIX-D1: POST /v1/refunds — payout_uuid == 票据内嵌 uuid == 下行 PayoutRequest.PayoutID
// ---------------------------------------------------------------------------

func TestCreateRefund_TicketBoundToPayoutUUID(t *testing.T) {
	var captured ports.PayoutRequest
	orders := &fakeOrders{requestPayoutFn: func(_ context.Context, req ports.PayoutRequest) (types.MerchantPayout, error) {
		captured = req
		return payoutOf(req, types.PayoutLockedQueued), nil
	}}
	store := keyedStore(&fakeStore{getSupportedAsset: func(_ context.Context, c types.ChainID, sym string) (types.SupportedAsset, error) {
		return usdcAsset(), nil
	}})
	srv := newTestServer(t, func(d *Deps) { d.Store = store; d.Orders = orders })

	puuid := uuid.New()
	to := apiTestRefundTo.Hex()
	rec := doJSON(t, srv, "POST", "/v1/refunds", map[string]any{
		"payout_uuid":    puuid.String(),
		"source_account": "SUSPENSE_REFUNDABLE_LIAB",
		"chain_id":       31337,
		"asset_symbol":   "USDC",
		"amount":         "60",
		"refund_ticket": map[string]any{
			"to_address":      to,
			"payout_uuid":     puuid.String(),
			"retry_seq":       0,
			"expiry_unix":     apiTestExpiry,
			"admin_signature": apiTestSigHex,
		},
	}, merchantAuthHeaders("rf-uuid-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 响应主键回显客户端 payout_uuid.
	var resp struct {
		ID       string `json:"id"`
		RetrySeq int32  `json:"retry_seq"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != puuid.String() {
		t.Fatalf("resp.id = %s, want %s", resp.ID, puuid)
	}
	// 下行请求: PayoutID 与 Kind 绑定.
	if captured.PayoutID != puuid {
		t.Fatalf("PayoutRequest.PayoutID = %s, want %s", captured.PayoutID, puuid)
	}
	if captured.Kind != types.PayoutKindCustomerRefund {
		t.Fatalf("kind = %s, want CUSTOMER_REFUND", captured.Kind)
	}
	// 127B 票据逐字节断言 (AGENTS §2.3-B): signer 将逐字节比对.
	pl := captured.RefundTicket.SignedPayload
	if len(pl) != 127 {
		t.Fatalf("payload = %dB, want 127B", len(pl))
	}
	if string(pl[0:19]) != "VAULTFLOW_REFUND_V1" {
		t.Fatal("bad magic")
	}
	if binary.BigEndian.Uint64(pl[19:27]) != 31337 {
		t.Fatal("bad chain_id")
	}
	if !bytes.Equal(pl[27:47], apiTestToken.Bytes()) {
		t.Fatal("bad token")
	}
	if !bytes.Equal(pl[47:67], apiTestRefundTo.Bytes()) {
		t.Fatal("bad to_address")
	}
	if got := binary.BigEndian.Uint64(pl[67+24 : 99]); got != 60 {
		t.Fatalf("bad amount: %d", got)
	}
	if !bytes.Equal(pl[99:115], puuid[:]) {
		t.Fatal("payload-embedded payout_uuid != client payout_uuid — signer binding broken")
	}
	if binary.BigEndian.Uint32(pl[115:119]) != 0 {
		t.Fatal("embedded retry_seq != 0")
	}
	if binary.BigEndian.Uint64(pl[119:127]) != uint64(apiTestExpiry) {
		t.Fatal("bad expiry")
	}
	if len(captured.RefundTicket.AdminSignature) != 64 {
		t.Fatalf("sig = %dB, want 64B", len(captured.RefundTicket.AdminSignature))
	}
}

func TestCreateRefund_RejectsNonZeroRetrySeq(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) {
		d.Store = keyedStore(nil)
		d.Orders = &fakeOrders{}
	})
	rec := doJSON(t, srv, "POST", "/v1/refunds", map[string]any{
		"payout_uuid":    uuid.NewString(),
		"source_account": "SUSPENSE_REFUNDABLE_LIAB",
		"chain_id":       31337,
		"asset_symbol":   "USDC",
		"amount":         "60",
		"refund_ticket": map[string]any{
			"to_address":      apiTestRefundTo.Hex(),
			"retry_seq":       1, // 新退款必须 0 — 换号重试属未来流程
			"expiry_unix":     apiTestExpiry,
			"admin_signature": apiTestSigHex,
		},
	}, merchantAuthHeaders("rf-rs-1"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != string(CodeRefundTicketInvalid) {
		t.Fatalf("code = %s, want REFUND_TICKET_INVALID", got)
	}
}

func TestCreateRefund_RejectsEmbeddedUUIDMismatch(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) {
		d.Store = keyedStore(nil)
		d.Orders = &fakeOrders{}
	})
	rec := doJSON(t, srv, "POST", "/v1/refunds", map[string]any{
		"payout_uuid":    uuid.NewString(),
		"source_account": "SUSPENSE_REFUNDABLE_LIAB",
		"chain_id":       31337,
		"asset_symbol":   "USDC",
		"amount":         "60",
		"refund_ticket": map[string]any{
			"to_address":      apiTestRefundTo.Hex(),
			"payout_uuid":     uuid.NewString(), // != 顶层 payout_uuid
			"retry_seq":       0,
			"expiry_unix":     apiTestExpiry,
			"admin_signature": apiTestSigHex,
		},
	}, merchantAuthHeaders("rf-mis-1"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != string(CodeRefundTicketInvalid) {
		t.Fatalf("code = %s, want REFUND_TICKET_INVALID", got)
	}
}

// ---------------------------------------------------------------------------
// FIX-D1: POST /v1/orders/{id}/review APPROVE_REFUND — 完整票据透传
// ---------------------------------------------------------------------------

func TestReviewApproveRefund_TicketForwarded(t *testing.T) {
	oid := uuid.New()
	received := types.MustParseU256("60")
	orders := &fakeOrders{
		getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
			return types.PaymentOrder{
				ID: oid, MerchantID: testMerchant.ID, ChainID: 31337,
				AssetSymbol: "USDC", ExpectedAmount: types.MustParseU256("60"),
				ReceivedAmount:   received,
				Status:           types.OrderLatePaidReview,
				DepositAddressID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
			}, nil
		},
	}
	var captured ports.ReviewOrderRequest
	orders.reviewFn = func(_ context.Context, req ports.ReviewOrderRequest) (types.PaymentOrder, error) {
		captured = req
		return types.PaymentOrder{ID: oid, MerchantID: testMerchant.ID,
			ChainID: 31337, AssetSymbol: "USDC",
			ExpectedAmount: types.MustParseU256("60"), ReceivedAmount: received,
			Status: types.OrderRefundInProgress, DepositAddressID: uuid.New(),
			ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	puuid := uuid.New()
	store := keyedStore(&fakeStore{
		getSupportedAsset: func(_ context.Context, c types.ChainID, sym string) (types.SupportedAsset, error) {
			return usdcAsset(), nil
		},
		findOrderRefundPayout: func(_ context.Context, orderID uuid.UUID) (types.MerchantPayout, error) {
			return types.MerchantPayout{ID: puuid, MerchantID: testMerchant.ID,
				PayoutKind: types.PayoutKindCustomerRefund, Status: types.PayoutLockedQueued,
				ChainID: 31337, AssetSymbol: "USDC", ToAddress: apiTestRefundTo,
				Amount: received, CreatedAt: time.Now().UTC()}, nil
		},
	})
	srv := newTestServer(t, func(d *Deps) { d.Store = store; d.Orders = orders })

	rec := doJSON(t, srv, "POST", "/v1/orders/"+oid.String()+"/review", map[string]any{
		"action": "APPROVE_REFUND",
		"refund": map[string]any{
			"to_address":      apiTestRefundTo.Hex(),
			"payout_uuid":     puuid.String(),
			"retry_seq":       0,
			"expiry_unix":     apiTestExpiry,
			"admin_signature": apiTestSigHex,
		},
	}, merchantAuthHeaders("rev-rf-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// ReviewOrderRequest 必须携带完整票据 (payload+signature), 非仅地址.
	if captured.RefundTicket == nil {
		t.Fatal("ReviewOrderRequest.RefundTicket must carry the complete ticket")
	}
	pl := captured.RefundTicket.SignedPayload
	if len(pl) != 127 || !bytes.Equal(pl[99:115], puuid[:]) {
		t.Fatalf("ticket payload malformed or uuid not bound (%dB)", len(pl))
	}
	if !bytes.Equal(pl[47:67], apiTestRefundTo.Bytes()) {
		t.Fatal("ticket to != refund.to_address")
	}
	var wantAmt [32]byte
	received.Big().FillBytes(wantAmt[:])
	if !bytes.Equal(pl[67:99], wantAmt[:]) {
		t.Fatal("ticket amount != order.received_amount")
	}
	if captured.RefundToAddress == nil || *captured.RefundToAddress != apiTestRefundTo {
		t.Fatal("RefundToAddress mismatch")
	}
	// 响应 payout.id == 票据内嵌 uuid.
	var resp struct {
		Order struct {
			Status string `json:"status"`
		} `json:"order"`
		Payout *struct {
			ID string `json:"id"`
		} `json:"payout"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Payout == nil || resp.Payout.ID != puuid.String() {
		t.Fatalf("resp.payout.id = %+v, want %s", resp.Payout, puuid)
	}
}

func TestReviewApproveRefund_RequiresPayoutUUID(t *testing.T) {
	oid := uuid.New()
	orders := &fakeOrders{getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
		return types.PaymentOrder{ID: oid, MerchantID: testMerchant.ID,
			ChainID: 31337, AssetSymbol: "USDC", Status: types.OrderLatePaidReview,
			ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	srv := newTestServer(t, func(d *Deps) { d.Store = keyedStore(nil); d.Orders = orders })
	rec := doJSON(t, srv, "POST", "/v1/orders/"+oid.String()+"/review", map[string]any{
		"action": "APPROVE_REFUND",
		"refund": map[string]any{
			"to_address":      apiTestRefundTo.Hex(),
			"retry_seq":       0,
			"expiry_unix":     apiTestExpiry,
			"admin_signature": apiTestSigHex,
		}, // 缺 payout_uuid — 票据签发必备, 否则主键无法与票据绑定
	}, merchantAuthHeaders("rev-miss-1"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != string(CodeRefundTicketInvalid) {
		t.Fatalf("code = %s, want REFUND_TICKET_INVALID", got)
	}
}

// ---------------------------------------------------------------------------
// FIX-D3: POST /v1/payouts — 白名单错误码映射
// ---------------------------------------------------------------------------

func TestCreatePayout_WhitelistErrorMapping(t *testing.T) {
	orders := &fakeOrders{requestPayoutFn: func(_ context.Context, req ports.PayoutRequest) (types.MerchantPayout, error) {
		return types.MerchantPayout{}, order.ErrAddressNotWhitelisted
	}}
	srv := newTestServer(t, func(d *Deps) { d.Store = keyedStore(nil); d.Orders = orders })
	rec := doJSON(t, srv, "POST", "/v1/payouts", map[string]any{
		"payout_uuid":    uuid.NewString(),
		"source_account": "MERCHANT_AVAILABLE",
		"chain_id":       31337,
		"asset_symbol":   "USDC",
		"amount":         "40",
		"to_address":     "0x6666666666666666666666666666666666666666",
	}, merchantAuthHeaders("wd-wl-1"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != string(CodeAddressNotWhitelisted) {
		t.Fatalf("code = %s, want ADDRESS_NOT_WHITELISTED", got)
	}
}

func TestCreatePayout_PayoutUUIDPropagates(t *testing.T) {
	var captured ports.PayoutRequest
	orders := &fakeOrders{requestPayoutFn: func(_ context.Context, req ports.PayoutRequest) (types.MerchantPayout, error) {
		captured = req
		return payoutOf(req, types.PayoutLockedQueued), nil
	}}
	srv := newTestServer(t, func(d *Deps) { d.Store = keyedStore(nil); d.Orders = orders })
	puuid := uuid.New()
	rec := doJSON(t, srv, "POST", "/v1/payouts", map[string]any{
		"payout_uuid":    puuid.String(),
		"source_account": "MERCHANT_AVAILABLE",
		"chain_id":       31337,
		"asset_symbol":   "USDC",
		"amount":         "40",
		"to_address":     "0x6666666666666666666666666666666666666666",
	}, merchantAuthHeaders("wd-uuid-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if captured.PayoutID != puuid {
		t.Fatalf("PayoutRequest.PayoutID = %s, want %s", captured.PayoutID, puuid)
	}
	if captured.RefundTicket != nil {
		t.Fatal("WITHDRAWAL must not carry a refund ticket")
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != puuid.String() {
		t.Fatalf("resp.id = %s, want %s", resp.ID, puuid)
	}
}

// ---------------------------------------------------------------------------
// FIX-D4: SSE — EXPIRED 非终态 + lastEventId query 兜底
// ---------------------------------------------------------------------------

func TestStreamPaymentEvents_ExpiredIsNotTerminal(t *testing.T) {
	orderID := uuid.New()
	orders := &fakeOrders{getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
		return types.PaymentOrder{
			ID: orderID, MerchantID: testMerchant.ID, ChainID: 31337,
			AssetSymbol: "USDC", ExpectedAmount: types.U256FromUint64(1500000),
			Status: types.OrderAwaitingPayment, DepositAddressID: uuid.New(),
		}, nil
	}}
	streamer := &stubStreamer{events: []stubEvent{
		{seq: 1, typ: ports.OrderEventExpired, status: types.OrderUnderpaidExpiredReview},
		{seq: 2, typ: ports.OrderEventStatusChanged, status: types.OrderLatePaidReview},
	}}
	srv := newTestServer(t, func(d *Deps) { d.Orders = orders; d.Streamer = streamer })

	req := httptest.NewRequest("GET", "/v1/public/payments/"+orderID.String()+"/events", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req.WithContext(ctx))
	body := rec.Body.String()
	// FIX-D4: expired 事件后不得断开 — 后续状态跃迁必须继续下发.
	if !strings.Contains(body, "event: expired") {
		t.Fatalf("missing expired frame: %q", body)
	}
	if !strings.Contains(body, "id: 2\n") {
		t.Fatalf("stream must continue after expired (missing seq 2): %q", body)
	}
	if !strings.Contains(body, "event: late_paid_review") {
		t.Fatalf("missing post-expired status frame: %q", body)
	}
}

func TestStreamPaymentEvents_TerminalCloses(t *testing.T) {
	orderID := uuid.New()
	orders := &fakeOrders{getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
		return types.PaymentOrder{
			ID: orderID, MerchantID: testMerchant.ID, ChainID: 31337,
			AssetSymbol: "USDC", ExpectedAmount: types.U256FromUint64(1500000),
			Status: types.OrderRefunded, DepositAddressID: uuid.New(),
		}, nil
	}}
	streamer := &stubStreamer{events: []stubEvent{
		{seq: 1, typ: ports.OrderEventStatusChanged, status: types.OrderRefunded},
	}}
	srv := newTestServer(t, func(d *Deps) { d.Orders = orders; d.Streamer = streamer })

	// 不设超时 — 终态事件即应正向关闭, handler 须自行返回.
	req := httptest.NewRequest("GET", "/v1/public/payments/"+orderID.String()+"/events", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { srv.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close after REFUNDED (terminal)")
	}
	if !strings.Contains(rec.Body.String(), "event: refunded") {
		t.Fatalf("missing refunded frame: %q", rec.Body.String())
	}
}

func TestStreamPaymentEvents_LastEventIDQueryFallback(t *testing.T) {
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
		{seq: 2, typ: ports.OrderEventConfirmationUpdate, status: types.OrderAwaitingPayment},
	}}
	srv := newTestServer(t, func(d *Deps) { d.Orders = orders; d.Streamer = streamer })

	// 浏览器原生 EventSource 无法设 header — ?lastEventId= 兜底 (FIX-D4).
	req := httptest.NewRequest("GET",
		"/v1/public/payments/"+orderID.String()+"/events?lastEventId=1", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req.WithContext(ctx))
	body := rec.Body.String()
	if strings.Contains(body, "id: 1\n") {
		t.Fatalf("seq 1 must not replay after lastEventId=1: %q", body)
	}
	if !strings.Contains(body, "id: 2\n") {
		t.Fatalf("seq 2 must replay: %q", body)
	}
}

func TestStreamPaymentEvents_LastEventIDHeaderPrecedence(t *testing.T) {
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
		{seq: 2, typ: ports.OrderEventConfirmationUpdate, status: types.OrderAwaitingPayment},
	}}
	srv := newTestServer(t, func(d *Deps) { d.Orders = orders; d.Streamer = streamer })

	// header 优先于 query (header=2 -> 什么都不回放; query=0 被忽略).
	req := httptest.NewRequest("GET",
		"/v1/public/payments/"+orderID.String()+"/events?lastEventId=0", nil)
	req.Header.Set("Last-Event-ID", "2")
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req.WithContext(ctx))
	body := rec.Body.String()
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") {
		t.Fatalf("header Last-Event-ID=2 must suppress both events: %q", body)
	}
}

// ---------------------------------------------------------------------------
// FIX-D5: EIP-3009 — v 归一化 (yParity 0/1 -> 27/28) 与提示面 valid_before
// ---------------------------------------------------------------------------

type apiFakeTxManager struct {
	queued []ports.QueueIntentRequest
}

func (f *apiFakeTxManager) QueueIntent(_ context.Context, req ports.QueueIntentRequest) (types.OutboundTxIntent, error) {
	f.queued = append(f.queued, req)
	return types.OutboundTxIntent{ID: uuid.New(), IntentType: req.IntentType,
		ReferenceID: req.ReferenceID, ChainID: req.ChainID,
		Status: types.TxIntentStatus("QUEUED")}, nil
}
func (f *apiFakeTxManager) GetIntent(context.Context, uuid.UUID) (types.OutboundTxIntent, error) {
	return types.OutboundTxIntent{}, ports.ErrNotFound
}
func (f *apiFakeTxManager) ListAttempts(context.Context, uuid.UUID) ([]types.OutboundTxAttempt, error) {
	return nil, nil
}
func (f *apiFakeTxManager) RequestCancel(context.Context, uuid.UUID) error { return nil }
func (f *apiFakeTxManager) NonceCursorStatus(_ context.Context, chainID types.ChainID, wallet types.Address) (ports.NonceCursorStatus, error) {
	return ports.NonceCursorStatus{ChainID: chainID, WalletAddress: wallet}, nil
}

var (
	eip3009Router = types.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	relayerWallet = types.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	eip3009Expiry = time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC) // nowFn+1h
	eip3009Salt   = types.HexToHash("0x" + strings.Repeat("11", 32))
	eip3009Payer  = types.HexToAddress("0x8888888888888888888888888888888888888888")
	eip3009RSig   = types.HexToHash("0x" + strings.Repeat("22", 32))
	eip3009SSig   = types.HexToHash("0x" + strings.Repeat("33", 32))
)

func eip3009Fixture(t *testing.T) (*fakeOrders, *apiFakeTxManager, uuid.UUID) {
	oid := uuid.New()
	orders := &fakeOrders{getFn: func(_ context.Context, id uuid.UUID) (types.PaymentOrder, error) {
		return types.PaymentOrder{
			ID: oid, MerchantID: testMerchant.ID, ChainID: 31337,
			AssetSymbol: "USDC", ExpectedAmount: types.MustParseU256("60"),
			Status: types.OrderAwaitingPayment, DepositAddressID: uuid.New(),
			ExpiresAt: eip3009Expiry,
		}, nil
	}}
	return orders, &apiFakeTxManager{}, oid
}

func eip3009Server(t *testing.T, orders *fakeOrders, txm *apiFakeTxManager) *Server {
	salt := eip3009Salt
	store := keyedStore(&fakeStore{
		getSupportedAsset: func(_ context.Context, c types.ChainID, sym string) (types.SupportedAsset, error) {
			return usdcAsset(), nil
		},
		getDepositAddress: func(_ context.Context, id uuid.UUID) (types.DepositAddress, error) {
			return types.DepositAddress{ID: id, Create2Salt: salt}, nil
		},
	})
	return newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Orders = orders
		d.TxManager = txm
		d.Config.Chains = map[types.ChainID]ChainConfig{
			31337: {
				ChainID:              31337,
				EIP3009Router:        eip3009Router,
				RelayerHotWallet:     relayerWallet,
				RelayGasLimit:        220_000,
				MaxFeePerGas:         types.MustParseU256("2000000000"),
				MaxPriorityFeePerGas: types.MustParseU256("1000000000"),
			},
		}
	})
}

func eip3009Body(v int) map[string]any {
	nonce := AuthNonce(31337, eip3009Salt)
	return map[string]any{
		"from":         eip3009Payer.Hex(),
		"to":           eip3009Router.Hex(),
		"value":        "60",
		"valid_after":  0,
		"valid_before": eip3009Expiry.Unix(),
		"nonce":        "0x" + hex.EncodeToString(nonce.Bytes()),
		"v":            v,
		"r":            "0x" + hex.EncodeToString(eip3009RSig.Bytes()),
		"s":            "0x" + hex.EncodeToString(eip3009SSig.Bytes()),
	}
}

func TestSubmitEip3009_YParityNormalizedTo27(t *testing.T) {
	orders, txm, oid := eip3009Fixture(t)
	srv := eip3009Server(t, orders, txm)

	// v=0 (yParity) -> calldata v 槽位必须抬升为 27.
	rec := doJSON(t, srv, "POST", "/v1/public/payments/"+oid.String()+"/eip3009",
		eip3009Body(0), map[string]string{"Idempotency-Key": "e3009-v0"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(txm.queued) != 1 {
		t.Fatalf("queued = %d", len(txm.queued))
	}
	cd := txm.queued[0].Calldata
	if len(cd) != 292 {
		t.Fatalf("calldata = %dB, want 292B", len(cd))
	}
	if cd[227] != 27 {
		t.Fatalf("calldata v slot = %d, want 27 (yParity+27)", cd[227])
	}
	if txm.queued[0].IntentType != types.IntentTypeEIP3009Relay {
		t.Fatalf("intent = %s", txm.queued[0].IntentType)
	}
	if txm.queued[0].WalletAddress != relayerWallet {
		t.Fatal("intent wallet != relayer hot wallet")
	}
}

func TestSubmitEip3009_V28KeptAndRejected(t *testing.T) {
	orders, txm, oid := eip3009Fixture(t)
	srv := eip3009Server(t, orders, txm)

	rec := doJSON(t, srv, "POST", "/v1/public/payments/"+oid.String()+"/eip3009",
		eip3009Body(28), map[string]string{"Idempotency-Key": "e3009-v28"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if cd := txm.queued[0].Calldata; cd[227] != 28 {
		t.Fatalf("v=28 must pass through as 28, got %d", cd[227])
	}

	// 非法 v=5 -> 400.
	rec2 := doJSON(t, srv, "POST", "/v1/public/payments/"+oid.String()+"/eip3009",
		eip3009Body(5), map[string]string{"Idempotency-Key": "e3009-v5"})
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("v=5 status = %d, want 400 (%s)", rec2.Code, rec2.Body.String())
	}
}

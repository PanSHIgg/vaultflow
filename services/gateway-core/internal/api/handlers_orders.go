package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// POST /v1/orders — createOrder (merchant, idempotent)
// ---------------------------------------------------------------------------

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	var req createOrderRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	if req.ChainID <= 0 {
		writeError(w, r, badRequest("chain_id is required"))
		return
	}
	if isBlank(req.AssetSymbol) {
		writeError(w, r, badRequest("asset_symbol is required"))
		return
	}
	if len(req.AssetSymbol) > 32 {
		writeError(w, r, badRequest("asset_symbol too long"))
		return
	}
	if req.MerchantOrderRef != nil && len(*req.MerchantOrderRef) > 128 {
		writeError(w, r, badRequest("merchant_order_ref exceeds 128 chars"))
		return
	}
	if _, ae := parseAmount(req.Amount, "amount"); ae != nil {
		writeError(w, r, ae)
		return
	}
	ttl := int64(1800)
	if req.ExpiresInSeconds != nil {
		ttl = *req.ExpiresInSeconds
	}
	if ttl < 60 || ttl > 86400 {
		writeError(w, r, badRequest("expires_in_seconds must be in [60,86400]"))
		return
	}
	if s.deps.Orders == nil {
		writeError(w, r, upstream("order service not wired"))
		return
	}
	body, _ := readBody(r, s.deps.Config.MaxBodyBytes)
	order, err := s.deps.Orders.CreateOrder(r.Context(), ports.CreateOrderRequest{
		MerchantID:     m.ID,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		RequestHash:    bodySHA256Hex(body),
		ChainID:        types.ChainID(req.ChainID),
		AssetSymbol:    req.AssetSymbol,
		ExpectedAmount: req.Amount,
		ExpiresAt:      s.deps.now().Add(time.Duration(ttl) * time.Second),
	})
	if err != nil {
		writeError(w, r, mapError(err, CodeOrderStateInvalid))
		return
	}
	dto, ae := s.orderProjection(r, order, req.MerchantOrderRef)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

// orderProjection 组装 Order 响应 (补 deposit_address / asset / eip3009 hint).
func (s *Server) orderProjection(r *http.Request, o types.PaymentOrder, merchantRef *string) (orderDTO, *APIError) {
	var dep *types.DepositAddress
	var asset *types.SupportedAsset
	if s.deps.Store != nil {
		if d, err := s.deps.Store.GetDepositAddress(r.Context(), o.DepositAddressID); err == nil {
			dd := d
			dep = &dd
		}
		if a, err := s.deps.Store.GetSupportedAsset(r.Context(), o.ChainID, o.AssetSymbol); err == nil {
			aa := a
			asset = &aa
		}
	}
	return s.toOrderDTO(o, dep, asset, merchantRef), nil
}

// ---------------------------------------------------------------------------
// GET /v1/orders — listOrders (merchant view)
// ---------------------------------------------------------------------------

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	limit, after, ae := parsePageParams(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	statuses, ae := parseEnumCSV[types.OrderStatus](r, "status", orderStatusValid)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	chainID, ae := parseChainIDQuery(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Store == nil {
		writeError(w, r, upstream("read store not wired"))
		return
	}
	page, err := s.deps.Store.ListOrders(r.Context(), OrderListQuery{
		MerchantID:  m.ID,
		Statuses:    statuses,
		ChainID:     chainID,
		AssetSymbol: r.URL.Query().Get("asset_symbol"),
		MerchantRef: r.URL.Query().Get("merchant_order_ref"),
		After:       after,
		Limit:       limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, CodeOrderStateInvalid))
		return
	}
	out := orderPageDTO{Data: make([]orderDTO, 0, len(page.Data))}
	for _, o := range page.Data {
		dto, perr := s.orderProjection(r, o, nil)
		if perr != nil {
			continue
		}
		out.Data = append(out.Data, dto)
	}
	if page.HasMore && len(page.Data) > 0 {
		last := page.Data[len(page.Data)-1]
		// 支付单无 created_at 列投影; Keyset 游标语义由 Store 实现持有,
		// 这里以零值排序键 + id 决胜 (见 ReadStore 契约注释 — Wave-2 统一).
		out.Page = pageOf(true, encodeCursor(time.Time{}, last.ID))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /v1/orders/{orderId} — getOrder (merchant view)
// ---------------------------------------------------------------------------

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	orderID, ae := parsePathUUID(r, "orderId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Orders == nil {
		writeError(w, r, upstream("order service not wired"))
		return
	}
	order, err := s.deps.Orders.GetOrder(r.Context(), orderID)
	if err != nil {
		writeError(w, r, mapError(err, CodeOrderStateInvalid))
		return
	}
	if order.MerchantID != m.ID {
		writeError(w, r, notFound("order not found"))
		return
	}
	dto, ae := s.orderProjection(r, order, nil)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// ---------------------------------------------------------------------------
// POST /v1/orders/{orderId}/review — reviewOrder
// ---------------------------------------------------------------------------

var reviewActionMap = map[string]ports.ReviewAction{
	"ACCEPT_UNDERPAID_DISCOUNT": ports.ReviewAcceptUnderpaid,
	"ACCEPT_LATE_PAYMENT":       ports.ReviewAcceptLatePaid,
	"APPROVE_REFUND":            ports.ReviewCreateRefund,
}

func (s *Server) reviewOrder(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	orderID, ae := parsePathUUID(r, "orderId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	var req reviewOrderRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	action, ok := reviewActionMap[req.Action]
	if !ok {
		writeError(w, r, badRequest("invalid action; expected ACCEPT_UNDERPAID_DISCOUNT | ACCEPT_LATE_PAYMENT | APPROVE_REFUND"))
		return
	}
	if len(req.Note) > 512 {
		writeError(w, r, badRequest("note exceeds 512 chars"))
		return
	}
	var refundTo *types.Address
	var refundUUID uuid.UUID
	var refundSig []byte
	if req.Action == "APPROVE_REFUND" {
		if req.Refund == nil {
			writeError(w, r, badRequest("refund is required for APPROVE_REFUND"))
			return
		}
		addr, err := parseEVMAddress(req.Refund.ToAddress)
		if err != nil {
			writeError(w, r, badRequest("invalid refund.to_address"))
			return
		}
		// FIX-D1: 票据内嵌 payout_uuid 即退款单主键 — 必填且须与商户在
		// 离线管理员签署时所用 uuid 一致.
		if req.Refund.PayoutUUID == nil || *req.Refund.PayoutUUID == "" {
			writeError(w, r, unprocessable(CodeRefundTicketInvalid,
				"refund.payout_uuid is required (it is embedded in the signed ticket and becomes the payout id)"))
			return
		}
		if refundUUID, err = uuid.Parse(*req.Refund.PayoutUUID); err != nil {
			writeError(w, r, badRequest("invalid refund.payout_uuid"))
			return
		}
		if req.Refund.RetrySeq != 0 {
			writeError(w, r, unprocessable(CodeRefundTicketInvalid,
				"refund.retry_seq must be 0 for a new refund payout"))
			return
		}
		if req.Refund.ExpiryUnix <= 0 {
			writeError(w, r, unprocessable(CodeRefundTicketInvalid, "refund.expiry_unix must be positive"))
			return
		}
		if req.Refund.ExpiryUnix <= s.deps.now().Unix() {
			writeError(w, r, unprocessable(CodeRefundTicketInvalid, "refund ticket already expired"))
			return
		}
		if refundSig, err = parseEd25519Signature(req.Refund.AdminSignature); err != nil {
			writeError(w, r, badRequest("invalid refund.admin_signature"))
			return
		}
		refundTo = &addr
	}
	if s.deps.Orders == nil {
		writeError(w, r, upstream("order service not wired"))
		return
	}
	// 商户作用域: 订单必须属于调用方商户.
	order, err := s.deps.Orders.GetOrder(r.Context(), orderID)
	if err != nil {
		writeError(w, r, mapError(err, CodeOrderStateInvalid))
		return
	}
	if order.MerchantID != m.ID {
		writeError(w, r, notFound("order not found"))
		return
	}
	var refundTicket *ports.RefundTicket
	if refundTo != nil {
		// FIX-D1: 拼 127B REFUND_V1 载荷 — chain/token/amount 由订单派生
		// (amount=received_amount), payout_uuid/retry_seq/expiry 取请求字段;
		// signer 验签并对 calldata 逐字节比对.
		if s.deps.Store == nil {
			writeError(w, r, upstream("read store not wired"))
			return
		}
		asset, err := s.deps.Store.GetSupportedAsset(r.Context(), order.ChainID, order.AssetSymbol)
		if err != nil {
			writeError(w, r, notFound("unsupported asset"))
			return
		}
		payload := BuildRefundTicketPayload(
			uint64(order.ChainID), asset.TokenContract, *refundTo, order.ReceivedAmount,
			refundUUID, uint32(req.Refund.RetrySeq), uint64(req.Refund.ExpiryUnix),
		)
		refundTicket = &ports.RefundTicket{SignedPayload: payload, AdminSignature: refundSig}
	}
	reviewed, err := s.deps.Orders.ReviewOrder(r.Context(), ports.ReviewOrderRequest{
		OrderID:         orderID,
		Action:          action,
		RefundToAddress: refundTo,
		RefundTicket:    refundTicket,
		OperatorNote:    req.Note,
	})
	if err != nil {
		writeError(w, r, mapError(err, CodeOrderStateInvalid))
		return
	}
	resp := reviewOrderResponse{}
	resp.Order, ae = s.orderProjection(r, reviewed, nil)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if req.Action == "APPROVE_REFUND" && s.deps.Store != nil {
		if p, err := s.deps.Store.FindOrderRefundPayout(r.Context(), orderID); err == nil {
			dto := s.payoutProjection(r, p)
			resp.Payout = &dto
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

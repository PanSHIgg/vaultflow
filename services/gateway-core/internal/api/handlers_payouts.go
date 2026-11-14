package api

import (
	"github.com/google/uuid"
	"net/http"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// payoutProjection 组装 Payout 响应 (尽力补 outbound_intent_id / tx_hash).
func (s *Server) payoutProjection(r *http.Request, p types.MerchantPayout) payoutDTO {
	var intentID *uuid.UUID
	var txHash *types.Hash
	if s.deps.Store != nil {
		if it, err := s.deps.Store.FindIntentByReference(r.Context(), p.ID); err == nil {
			iid := it.ID
			intentID = &iid
			if at, err := s.deps.Store.LatestFinalizedAttempt(r.Context(), it.ID); err == nil {
				h := at.TxHash
				txHash = &h
			}
		}
	}
	return toPayoutDTO(p, intentID, txHash)
}

// validateSourceAccount PayoutSourceAccount 四类来源校验.
func validateSourceAccount(raw string) (types.LedgerAccountCode, *APIError) {
	acct := types.LedgerAccountCode(raw)
	if !acct.IsPayoutSource() {
		return "", badRequest("source_account must be one of MERCHANT_AVAILABLE | SUSPENSE_OVERPAY_LIAB | SUSPENSE_REFUNDABLE_LIAB | SUSPENSE_UNMATCHED_LIAB")
	}
	return acct, nil
}

func suspenseOnly(acct types.LedgerAccountCode) *APIError {
	for _, s := range types.SuspenseLiabilityAccounts {
		if acct == s {
			return nil
		}
	}
	return badRequest("source_account for CUSTOMER_REFUND must be a SUSPENSE_*_LIAB account")
}

// ---------------------------------------------------------------------------
// POST /v1/payouts — createPayout (WITHDRAWAL)
// ---------------------------------------------------------------------------

func (s *Server) createPayout(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	var req createPayoutRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	payoutUUID, ae := parseUUIDParam(req.PayoutUUID, "payout_uuid")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	acct, ae := validateSourceAccount(req.SourceAccount)
	if ae != nil {
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
	if _, ae := parseAmount(req.Amount, "amount"); ae != nil {
		writeError(w, r, ae)
		return
	}
	to, err := parseEVMAddress(req.ToAddress)
	if err != nil {
		writeError(w, r, badRequest("invalid to_address"))
		return
	}
	var srcOrder *uuid.UUID
	if req.SourceOrderID != nil && *req.SourceOrderID != "" {
		oid, ae := parseUUIDParam(*req.SourceOrderID, "source_order_id")
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		srcOrder = &oid
	}
	if s.deps.Orders == nil {
		writeError(w, r, upstream("order service not wired"))
		return
	}
	body, _ := readBody(r, s.deps.Config.MaxBodyBytes)
	// FIX-D1: 客户端 payout_uuid 即 merchant_payouts.id — 幂等锚透传至
	// signer (payout_uuid, retry_seq) 槽位绑定与 intent.reference_id.
	p, err := s.deps.Orders.RequestPayout(r.Context(), ports.PayoutRequest{
		MerchantID:     m.ID,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		RequestHash:    bodySHA256Hex(body),
		Kind:           types.PayoutKindWithdrawal,
		SourceAccount:  acct,
		SourceOrderID:  srcOrder,
		ChainID:        types.ChainID(req.ChainID),
		AssetSymbol:    req.AssetSymbol,
		ToAddress:      to,
		Amount:         req.Amount,
		PayoutID:       payoutUUID,
		RefundTicket:   nil,
	})
	if err != nil {
		writeError(w, r, mapError(err, CodePayoutStateInvalid))
		return
	}
	writeJSON(w, http.StatusCreated, s.payoutProjection(r, p))
}

// ---------------------------------------------------------------------------
// GET /v1/payouts — listPayouts
// ---------------------------------------------------------------------------

func (s *Server) listPayouts(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	limit, after, ae := parsePageParams(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	statuses, ae := parseEnumCSV[types.PayoutStatus](r, "status", payoutStatusValid)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	chainID, ae := parseChainIDQuery(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	var kind *types.PayoutKind
	if raw := r.URL.Query().Get("payout_kind"); raw != "" {
		k := types.PayoutKind(raw)
		if !payoutKindValid(k) {
			writeError(w, r, badRequest("invalid payout_kind"))
			return
		}
		kind = &k
	}
	var src *types.LedgerAccountCode
	if raw := r.URL.Query().Get("source_account"); raw != "" {
		a, ae := validateSourceAccount(raw)
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		src = &a
	}
	if s.deps.Store == nil {
		writeError(w, r, upstream("read store not wired"))
		return
	}
	page, err := s.deps.Store.ListPayouts(r.Context(), PayoutListQuery{
		MerchantID: m.ID, Statuses: statuses, Kind: kind, SourceAccount: src,
		ChainID: chainID, After: after, Limit: limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, CodePayoutStateInvalid))
		return
	}
	out := payoutPageDTO{Data: make([]payoutDTO, 0, len(page.Data))}
	for _, p := range page.Data {
		out.Data = append(out.Data, s.payoutProjection(r, p))
	}
	if page.HasMore && len(page.Data) > 0 {
		last := page.Data[len(page.Data)-1]
		out.Page = pageOf(true, encodeCursor(last.CreatedAt, last.ID))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /v1/payouts/{payoutId} — getPayout
// ---------------------------------------------------------------------------

func (s *Server) getPayout(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	pid, ae := parsePathUUID(r, "payoutId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Orders == nil {
		writeError(w, r, upstream("order service not wired"))
		return
	}
	p, err := s.deps.Orders.GetPayout(r.Context(), pid)
	if err != nil {
		writeError(w, r, mapError(err, CodePayoutStateInvalid))
		return
	}
	if p.MerchantID != m.ID {
		writeError(w, r, notFound("payout not found"))
		return
	}
	writeJSON(w, http.StatusOK, s.payoutProjection(r, p))
}

// ---------------------------------------------------------------------------
// POST /v1/payouts/{payoutId}/cancel — cancelPayout
// ---------------------------------------------------------------------------

func (s *Server) cancelPayout(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	pid, ae := parsePathUUID(r, "payoutId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Orders == nil {
		writeError(w, r, upstream("order service not wired"))
		return
	}
	p, err := s.deps.Orders.GetPayout(r.Context(), pid)
	if err != nil {
		writeError(w, r, mapError(err, CodePayoutStateInvalid))
		return
	}
	if p.MerchantID != m.ID {
		writeError(w, r, notFound("payout not found"))
		return
	}
	cancelled, err := s.deps.Orders.CancelPayout(r.Context(), pid)
	if err != nil {
		writeError(w, r, mapError(err, CodePayoutStateInvalid))
		return
	}
	writeJSON(w, http.StatusOK, s.payoutProjection(r, cancelled))
}

// ---------------------------------------------------------------------------
// POST /v1/refunds — createRefund (CUSTOMER_REFUND + 127B RefundTicket)
// ---------------------------------------------------------------------------

func (s *Server) createRefund(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	var req createRefundRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	payoutUUID, ae := parseUUIDParam(req.PayoutUUID, "payout_uuid")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	acct, ae := validateSourceAccount(req.SourceAccount)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if ae := suspenseOnly(acct); ae != nil {
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
	if _, ae := parseAmount(req.Amount, "amount"); ae != nil {
		writeError(w, r, ae)
		return
	}
	if req.RefundTicket == nil {
		writeError(w, r, badRequest("refund_ticket is required"))
		return
	}
	to, err := parseEVMAddress(req.RefundTicket.ToAddress)
	if err != nil {
		writeError(w, r, badRequest("invalid refund_ticket.to_address"))
		return
	}
	// FIX-D1/D3: 新建退款单 retry_seq 列恒 0 — 票据内嵌 seq 必须同为 0
	// 才能与 signer 槽位绑定逐字节对齐; 重试换票属另一流程 (见交付报告).
	if req.RefundTicket.RetrySeq != 0 {
		writeError(w, r, unprocessable(CodeRefundTicketInvalid,
			"refund_ticket.retry_seq must be 0 for a new refund payout"))
		return
	}
	if req.RefundTicket.PayoutUUID != nil && *req.RefundTicket.PayoutUUID != "" {
		embedded, ae := parseUUIDParam(*req.RefundTicket.PayoutUUID, "refund_ticket.payout_uuid")
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		if embedded != payoutUUID {
			writeError(w, r, unprocessable(CodeRefundTicketInvalid,
				"refund_ticket.payout_uuid must equal top-level payout_uuid"))
			return
		}
	}
	sig, err := parseEd25519Signature(req.RefundTicket.AdminSignature)
	if err != nil {
		writeError(w, r, badRequest("invalid refund_ticket.admin_signature"))
		return
	}
	if req.RefundTicket.ExpiryUnix <= 0 {
		writeError(w, r, unprocessable(CodeRefundTicketInvalid, "refund_ticket.expiry_unix must be positive"))
		return
	}
	if req.RefundTicket.ExpiryUnix <= s.deps.now().Unix() {
		writeError(w, r, unprocessable(CodeRefundTicketInvalid, "refund ticket already expired"))
		return
	}
	var srcOrder *uuid.UUID
	if req.SourceOrderID != nil && *req.SourceOrderID != "" {
		oid, ae := parseUUIDParam(*req.SourceOrderID, "source_order_id")
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		srcOrder = &oid
	}
	if s.deps.Orders == nil || s.deps.Store == nil {
		writeError(w, r, upstream("order service or read store not wired"))
		return
	}
	asset, err := s.deps.Store.GetSupportedAsset(r.Context(), types.ChainID(req.ChainID), req.AssetSymbol)
	if err != nil {
		writeError(w, r, notFound("unsupported asset"))
		return
	}
	// 拼装 127B RefundTicket 载荷 (AGENTS.md §2.3-B 冻结字节序).
	payload := BuildRefundTicketPayload(
		uint64(req.ChainID), asset.TokenContract, to, req.Amount,
		payoutUUID, uint32(req.RefundTicket.RetrySeq), uint64(req.RefundTicket.ExpiryUnix),
	)
	body, _ := readBody(r, s.deps.Config.MaxBodyBytes)
	p, err := s.deps.Orders.RequestPayout(r.Context(), ports.PayoutRequest{
		MerchantID:     m.ID,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		RequestHash:    bodySHA256Hex(body),
		Kind:           types.PayoutKindCustomerRefund,
		SourceAccount:  acct,
		SourceOrderID:  srcOrder,
		ChainID:        types.ChainID(req.ChainID),
		AssetSymbol:    req.AssetSymbol,
		ToAddress:      to,
		Amount:         req.Amount,
		PayoutID:       payoutUUID, // FIX-D1: PK == 票据内嵌 uuid, signer 逐字节比对
		RefundTicket: &ports.RefundTicket{
			SignedPayload:  payload,
			AdminSignature: sig,
		},
	})
	if err != nil {
		writeError(w, r, mapError(err, CodePayoutStateInvalid))
		return
	}
	writeJSON(w, http.StatusCreated, s.payoutProjection(r, p))
}

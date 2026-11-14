package api

import (
	"net/http"
	"time"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// POST /v1/withdrawal-addresses — registerWithdrawalAddress
// ---------------------------------------------------------------------------

func (s *Server) registerWithdrawalAddress(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	var req registerWithdrawalAddressRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	if req.ChainID <= 0 {
		writeError(w, r, badRequest("chain_id is required"))
		return
	}
	addr, err := parseEVMAddress(req.Address)
	if err != nil {
		writeError(w, r, badRequest("invalid address"))
		return
	}
	if req.ValidUntilUnix <= 0 {
		writeError(w, r, badRequest("valid_until_unix must be a positive unix timestamp"))
		return
	}
	sig, err := parseEd25519Signature(req.AdminSignature)
	if err != nil {
		writeError(w, r, badRequest("invalid admin_signature"))
		return
	}
	if s.deps.Signer == nil || s.deps.Store == nil {
		writeError(w, r, upstream("signer client or read store not wired"))
		return
	}
	// 69B ADDR_V1 载荷 (merchant_uuid 由服务端绑定认证商户 — openapi 契约).
	payload := BuildWhitelistPayload(uint64(req.ChainID), m.ID, addr, uint64(req.ValidUntilUnix))
	resp, err := s.deps.Signer.RegisterWhitelist(r.Context(), ports.RegisterWhitelistRequest{
		SignedPayload:  payload,
		AdminSignature: sig,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	if !resp.Accepted {
		writeError(w, r, signerRejectError(resp))
		return
	}
	rec, err := s.deps.Store.CreateWithdrawalAddress(r.Context(), NewWithdrawalAddress{
		MerchantID: m.ID,
		ChainID:    types.ChainID(req.ChainID),
		Address:    addr,
		AdminSig:   sig,
		ValidUntil: time.Unix(req.ValidUntilUnix, 0).UTC(),
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	writeJSON(w, http.StatusCreated, withdrawalDTO(rec, false))
}

// withdrawalDTO types.MerchantWithdrawalAddress -> WithdrawalAddress 投影.
func withdrawalDTO(v types.MerchantWithdrawalAddress, signerRevoked bool) withdrawalAddressDTO {
	// ⚠ DDL 无 created_at / first_seen_at 列 — first_seen_at 恒 null (signer 持有),
	// created_at 以 valid_until 之外零值输出 null (契约缺陷已上报).
	return withdrawalAddressDTO{
		ID:            v.ID.String(),
		MerchantID:    v.MerchantID.String(),
		ChainID:       int64(v.ChainID),
		Address:       lowerHexAddr(v.Address),
		Status:        v.Status.String(),
		ValidUntil:    v.ValidUntil.UTC(),
		FirstSeenAt:   nil,
		SignerRevoked: signerRevoked,
	}
}

// signerRejectError 映射 vault-signer 拒签码 -> openapi 错误.
func signerRejectError(resp ports.WhitelistOpResponse) *APIError {
	msg := resp.Reason
	if msg == "" {
		msg = "signer rejected whitelist operation"
	}
	switch resp.RejectCode {
	case ports.RejectCodeInvalidAdminSignature:
		return unprocessable(CodeSignerRejected, "invalid admin signature: "+msg)
	case ports.RejectCodeWhitelistNotFound:
		return unprocessable(CodeAddressNotWhitelisted, msg)
	case ports.RejectCodeWhitelistCooldown, ports.RejectCodeWhitelistExpired, ports.RejectCodeWhitelistRevoked:
		return unprocessable(CodeAddressNotWhitelisted, msg)
	default:
		return unprocessable(CodeSignerRejected, msg)
	}
}

// ---------------------------------------------------------------------------
// GET /v1/withdrawal-addresses — listWithdrawalAddresses
// ---------------------------------------------------------------------------

func (s *Server) listWithdrawalAddresses(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	limit, ae := func() (int, *APIError) {
		limit, _, ae := parsePageParams(r)
		return limit, ae
	}()
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	afterID, ae := parseIDCursor(r.URL.Query().Get("cursor"))
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	statuses, ae := parseEnumCSV[types.WithdrawalAddressStatus](r, "status", withdrawalStatusValid)
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
	page, err := s.deps.Store.ListWithdrawalAddresses(r.Context(), WhitelistQuery{
		MerchantID: m.ID, Statuses: statuses, ChainID: chainID, After: afterID, Limit: limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	out := withdrawalAddressPageDTO{Data: make([]withdrawalAddressDTO, 0, len(page.Data))}
	for _, v := range page.Data {
		out.Data = append(out.Data, withdrawalDTO(v, v.Status == types.WithdrawalRevoked))
	}
	if page.HasMore && len(page.Data) > 0 {
		out.Page = pageOf(true, encodeIDCursor(page.Data[len(page.Data)-1].ID))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// DELETE /v1/withdrawal-addresses/{withdrawalAddressId} — revokeWithdrawalAddress
// ---------------------------------------------------------------------------

func (s *Server) revokeWithdrawalAddress(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	wid, ae := parsePathUUID(r, "withdrawalAddressId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Signer == nil || s.deps.Store == nil {
		writeError(w, r, upstream("signer client or read store not wired"))
		return
	}
	rec, err := s.deps.Store.GetWithdrawalAddress(r.Context(), wid)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	if rec.MerchantID != m.ID {
		writeError(w, r, notFound("withdrawal address not found"))
		return
	}
	resp, err := s.deps.Signer.RevokeWhitelist(r.Context(), ports.RevokeWhitelistRequest{
		ChainID:          uint64(rec.ChainID),
		MerchantUUID:     m.ID,
		RecipientAddress: rec.Address,
		AdminNote:        "gateway revocation via merchant API",
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	if !resp.Accepted {
		writeError(w, r, signerRejectError(resp))
		return
	}
	revoked, err := s.deps.Store.MarkWithdrawalAddressRevoked(r.Context(), wid)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	writeJSON(w, http.StatusOK, withdrawalDTO(revoked, true))
}

package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// GET /v1/admin/reconciliation/status — getReconciliationStatus
// ---------------------------------------------------------------------------

func (s *Server) getReconciliationStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.recon.status())
}

// ---------------------------------------------------------------------------
// POST /v1/admin/reconciliation/run — runReconciliation
// ---------------------------------------------------------------------------

type runReconRequest struct {
	ChainID *int64 `json:"chain_id"`
}

func (s *Server) runReconciliation(w http.ResponseWriter, r *http.Request) {
	var req runReconRequest
	// body 可选: 空 body = 全链巡检.
	if body, _ := readBody(r, s.deps.Config.MaxBodyBytes); len(body) > 0 {
		if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
			writeError(w, r, ae)
			return
		}
	}
	var cid *types.ChainID
	if req.ChainID != nil {
		if *req.ChainID <= 0 {
			writeError(w, r, badRequest("invalid chain_id"))
			return
		}
		c := types.ChainID(*req.ChainID)
		cid = &c
	}
	runID, ok := s.recon.trigger(cid)
	if !ok {
		writeError(w, r, conflict(CodeReconciliationRunning, "a reconciliation run is already in progress"))
		return
	}
	writeJSON(w, http.StatusAccepted, reconciliationRunAcceptedDTO{
		RunID: runID.String(), Status: "RUNNING",
	})
}

// ---------------------------------------------------------------------------
// GET /v1/admin/outbound-queue — getOutboundQueue
// ---------------------------------------------------------------------------

func (s *Server) getOutboundQueue(w http.ResponseWriter, r *http.Request) {
	limit, after, ae := parsePageParams(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	statuses, ae := parseEnumCSV[types.TxIntentStatus](r, "status", intentStatusValid)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	chainID, ae := parseChainIDQuery(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	var itype *types.IntentType
	if raw := r.URL.Query().Get("intent_type"); raw != "" {
		t := types.IntentType(raw)
		if !intentTypeValid(t) {
			writeError(w, r, badRequest("invalid intent_type"))
			return
		}
		itype = &t
	}
	var wallet *types.Address
	if raw := r.URL.Query().Get("wallet_address"); raw != "" {
		a, err := parseEVMAddress(raw)
		if err != nil {
			writeError(w, r, badRequest("invalid wallet_address"))
			return
		}
		wallet = &a
	}
	if s.deps.Store == nil {
		writeError(w, r, upstream("read store not wired"))
		return
	}
	out := outboundQueueViewDTO{
		NonceCursors:  []nonceCursorDTO{},
		HaltedWallets: []haltedWalletDTO{},
		Data:          []outboundIntentDTO{},
	}
	// nonce 游标 + 熔断视图.
	cursors, err := s.deps.Store.ListNonceCursors(r.Context(), chainID)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	for _, cv := range cursors {
		dto := nonceCursorDTO{
			ChainID:       int64(cv.Cursor.ChainID),
			WalletAddress: lowerHexAddr(cv.Cursor.WalletAddress),
			WalletRole:    s.walletRole(cv.Cursor.ChainID, cv.Cursor.WalletAddress),
			NextNonce:     cv.Cursor.NextNonce,
			Halted:        cv.Halted,
		}
		if !cv.Cursor.UpdatedAt.IsZero() {
			t := cv.Cursor.UpdatedAt.UTC()
			dto.UpdatedAt = &t
		}
		out.NonceCursors = append(out.NonceCursors, dto)
	}
	page, err := s.deps.Store.ListOutboundIntents(r.Context(), OutboundListQuery{
		Statuses: statuses, IntentType: itype, ChainID: chainID, Wallet: wallet,
		After: after, Limit: limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	for _, it := range page.Data {
		if it.Status == types.IntentNonceStolenHalt {
			out.HaltedWallets = append(out.HaltedWallets, haltedWalletDTO{
				ChainID:       int64(it.ChainID),
				WalletAddress: lowerHexAddr(it.WalletAddress),
				IntentID:      it.ID.String(),
			})
		}
		dto := outboundIntentDTO{
			ID:            it.ID.String(),
			IntentType:    it.IntentType.String(),
			ChainID:       int64(it.ChainID),
			WalletAddress: lowerHexAddr(it.WalletAddress),
			Nonce:         it.Nonce,
			Status:        it.Status.String(),
			CreatedAt:     it.CreatedAt.UTC(),
			Attempts:      []outboundAttemptDTO{},
		}
		if it.ReferenceID != uuid.Nil {
			rs := it.ReferenceID.String()
			dto.ReferenceID = &rs
		}
		if s.deps.TxManager != nil {
			if attempts, err := s.deps.TxManager.ListAttempts(r.Context(), it.ID); err == nil {
				for _, at := range attempts {
					dto.Attempts = append(dto.Attempts, attemptDTO(at))
				}
			}
		}
		out.Data = append(out.Data, dto)
	}
	if page.HasMore && len(page.Data) > 0 {
		last := page.Data[len(page.Data)-1]
		out.Page = pageOf(true, encodeCursor(last.CreatedAt, last.ID))
	}
	writeJSON(w, http.StatusOK, out)
}

func attemptDTO(a types.OutboundTxAttempt) outboundAttemptDTO {
	dto := outboundAttemptDTO{
		ID:                   a.ID.String(),
		AttemptNo:            a.AttemptNo,
		AttemptKind:          a.Kind.String(),
		Status:               a.Status.String(),
		BlockNumber:          a.BlockNumber,
		ReceiptStatus:        a.ReceiptStatus,
		MaxFeePerGas:         a.MaxFeePerGas,
		MaxPriorityFeePerGas: a.MaxPriorityFeePerGas,
		GasUsed:              a.GasUsed,
		EffectiveGasPrice:    a.EffectiveGasPrice,
		BroadcastedAt:        a.BroadcastedAt.UTC(),
	}
	if a.TxHash != (types.Hash{}) {
		h := lowerHexHash(a.TxHash)
		dto.TxHash = &h
	}
	if a.BlockHash != nil {
		h := lowerHexHash(*a.BlockHash)
		dto.BlockHash = &h
	}
	return dto
}

// walletRole 按链配置反推热钱包角色.
func (s *Server) walletRole(chainID types.ChainID, wallet types.Address) string {
	if cc, ok := s.deps.Config.Chains[chainID]; ok {
		switch wallet {
		case cc.RelayerHotWallet:
			return "RELAYER_HOT"
		case cc.PayoutHotWallet:
			return "PAYOUT_HOT"
		}
	}
	return "PAYOUT_HOT" // 未知默认提现角色 (Wave-2 配置完备后可辨)
}

// ---------------------------------------------------------------------------
// GET /v1/admin/signer/health — getSignerHealth
// ---------------------------------------------------------------------------

func (s *Server) getSignerHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Signer == nil {
		writeError(w, r, upstream("signer client not wired"))
		return
	}
	h, err := s.deps.Signer.Health(r.Context())
	if err != nil {
		writeError(w, r, upstream("vault-signer unreachable"))
		return
	}
	out := signerHealthDTO{
		Reachable:     true,
		MTLSOK:        true,
		SignerVersion: h.Version,
		UptimeSeconds: h.UptimeSeconds,
		Wallets:       []signerWalletDTO{},
	}
	for _, addr := range h.LoadedWallets {
		role := ""
		chainIDs := []int64{}
		for cid, cc := range s.deps.Config.Chains {
			if addr == cc.RelayerHotWallet {
				role = "RELAYER_HOT"
			} else if addr == cc.PayoutHotWallet {
				role = "PAYOUT_HOT"
			}
			chainIDs = append(chainIDs, int64(cid))
		}
		out.Wallets = append(out.Wallets, signerWalletDTO{
			Role: role, Address: lowerHexAddr(addr), ChainIDs: chainIDs, Loaded: true,
		})
	}
	// 网关侧 5 分钟巡检协程结果叠加: 此处按需即时校验 (Wave-2 起协程后可改读缓存).
	ac := &struct {
		Valid                       bool      `json:"valid"`
		LastSeq                     uint64    `json:"last_seq"`
		LastEntryHash               string    `json:"last_entry_hash"`
		VerifiedAt                  time.Time `json:"verified_at"`
		VerificationIntervalSeconds int       `json:"verification_interval_seconds"`
	}{VerificationIntervalSeconds: s.deps.Config.ReconIntervalSec}
	if vr, err := s.deps.Signer.VerifyAuditChain(r.Context(), ports.VerifyAuditChainRequest{FromSeq: 0, ToSeq: 0}); err == nil {
		ac.Valid = vr.Valid
		ac.LastSeq = vr.EntriesChecked
		ac.LastEntryHash = lowerHexHash(vr.TipHash)
		ac.VerifiedAt = s.deps.now()
	}
	out.AuditChain = ac
	// ⚠ 契约缺陷: ports.SignerHealth 无熔断器/白名单分项/幂等槽字段 —
	// whitelist 仅聚合数可映射 (active_count), 其余留空待 Wave-2 合拢.
	out.Whitelist = &struct {
		ActiveCount          int `json:"active_count"`
		PendingCooldownCount int `json:"pending_cooldown_count"`
		RevokedCount         int `json:"revoked_count"`
	}{ActiveCount: int(h.WhitelistEntries)}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /v1/admin/quarantined-vaults — listQuarantinedVaults
// ---------------------------------------------------------------------------

func (s *Server) listQuarantinedVaults(w http.ResponseWriter, r *http.Request) {
	limit, after, ae := parsePageParams(r)
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
	page, err := s.deps.Store.ListQuarantinedVaults(r.Context(), QuarantineQuery{
		ChainID: chainID, After: after, Limit: limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	out := quarantinedVaultPageDTO{Data: make([]quarantinedVaultDTO, 0, len(page.Data))}
	for _, v := range page.Data {
		dto := quarantinedVaultDTO{
			DepositAddressID: v.DepositAddressID.String(),
			ChainID:          int64(v.ChainID),
			Address:          lowerHexAddr(v.Address),
			IsQuarantined:    v.IsQuarantined,
			AssetSymbol:      v.AssetSymbol,
			Reason:           v.Reason,
			ItemStatus:       v.ItemStatus,
			QuarantinedAt:    v.QuarantinedAt,
		}
		if v.SweepBatchID != nil {
			b := v.SweepBatchID.String()
			dto.SweepBatchID = &b
		}
		if v.SweepBatchItemID != nil {
			b := v.SweepBatchItemID.String()
			dto.SweepBatchItemID = &b
		}
		if v.RelatedOrderID != nil {
			b := v.RelatedOrderID.String()
			dto.RelatedOrderID = &b
		}
		out.Data = append(out.Data, dto)
	}
	if page.HasMore && len(page.Data) > 0 {
		last := page.Data[len(page.Data)-1]
		out.Page = pageOf(true, encodeIDCursor(last.DepositAddressID))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /v1/admin/merchants/{merchantId}/payout-unfreeze — unfreezeMerchantPayouts
// ---------------------------------------------------------------------------

func (s *Server) unfreezeMerchantPayouts(w http.ResponseWriter, r *http.Request) {
	mid, ae := parsePathUUID(r, "merchantId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Orders == nil || s.deps.Store == nil {
		writeError(w, r, upstream("order service or read store not wired"))
		return
	}
	if _, err := s.deps.Store.GetMerchant(r.Context(), mid); err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	bal, err := s.deps.Store.MerchantReceivableBalance(r.Context(), mid)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	if bal.Sign() > 0 {
		writeError(w, r, apiErrDetails(http.StatusConflict, CodeReorgReceivableOutstanding,
			"MERCHANT_REORG_RECEIVABLE still outstanding",
			map[string]any{"reorg_receivable_balance": bal.String()}))
		return
	}
	if err := s.deps.Orders.UnfreezeMerchant(r.Context(), mid); err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	now := s.deps.now()
	writeJSON(w, http.StatusOK, merchantFreezeStateDTO{
		MerchantID:             mid.String(),
		PayoutFrozen:           false,
		ReorgReceivableBalance: bal,
		UnfrozenAt:             &now,
	})
}

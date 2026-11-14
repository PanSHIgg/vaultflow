package api

import (
	"math/big"
	"net/http"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// GET /v1/public/payments/{orderId} — getPublicPayment (无鉴权)
// ---------------------------------------------------------------------------

// publicProjection 收银台公开投影 (剥除商户内部字段; 404 隐藏存在性).
func (s *Server) publicProjection(r *http.Request, order types.PaymentOrder) (publicPaymentDTO, *APIError) {
	var dep *types.DepositAddress
	var asset *types.SupportedAsset
	var merchantName string
	var safeHead uint64
	if s.deps.Store != nil {
		if d, err := s.deps.Store.GetDepositAddress(r.Context(), order.DepositAddressID); err == nil {
			dd := d
			dep = &dd
		}
		if a, err := s.deps.Store.GetSupportedAsset(r.Context(), order.ChainID, order.AssetSymbol); err == nil {
			aa := a
			asset = &aa
		}
		if mm, err := s.deps.Store.GetMerchant(r.Context(), order.MerchantID); err == nil {
			merchantName = mm.Name
		}
		if cur, err := s.deps.Store.GetChainSyncCursor(r.Context(), order.ChainID); err == nil {
			safeHead = cur.SafeHeadHeight
		}
	}
	now := s.deps.now()
	secRemain := order.ExpiresAt.UTC().Unix() - now.Unix()
	dto := publicPaymentDTO{
		OrderID:          order.ID.String(),
		Status:           order.Status.String(),
		MerchantName:     merchantName,
		ChainID:          int64(order.ChainID),
		AssetSymbol:      order.AssetSymbol,
		ExpectedAmount:   order.ExpectedAmount,
		ReceivedAmount:   order.ReceivedAmount,
		ExpiresAt:        order.ExpiresAt.UTC(),
		SecondsRemaining: secRemain,
		SafeHeadHeight:   safeHead,
	}
	if cc, ok := s.deps.Config.Chains[order.ChainID]; ok {
		dto.RequiredConfirmations = cc.RequiredConfirmations
	}
	if order.SettledAt != nil {
		t := order.SettledAt.UTC()
		dto.SettledAt = &t
	}
	if dep != nil {
		dto.DepositAddress = lowerHexAddr(dep.Address)
		dto.DepositAddressDeployed = dep.IsDeployed
	}
	if asset != nil {
		dto.TokenContract = lowerHexAddr(asset.TokenContract)
		dto.Decimals = asset.Decimals
		if asset.SupportsEIP3009 && dep != nil {
			cc := s.deps.Config.Chains[order.ChainID]
			vb := order.ExpiresAt.UTC()
			dto.EIP3009 = &eip3009HintDTO{
				Supported:     true,
				RouterAddress: lowerHexAddr(cc.EIP3009Router),
				AuthNonce:     lowerHexHash(AuthNonce(uint64(order.ChainID), dep.Create2Salt)),
				TokenContract: lowerHexAddr(asset.TokenContract),
				ValidBefore:   &vb,
			}
		}
	}
	return dto, nil
}

func (s *Server) getPublicPayment(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, r, mapError(err, ""))
		return
	}
	dto, ae := s.publicProjection(r, order)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// ---------------------------------------------------------------------------
// POST /v1/public/payments/{orderId}/eip3009 — submitEip3009Authorization
// ---------------------------------------------------------------------------

func (s *Server) submitEip3009(w http.ResponseWriter, r *http.Request) {
	orderID, ae := parsePathUUID(r, "orderId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	var req eip3009Submission
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Orders == nil || s.deps.TxManager == nil || s.deps.Store == nil {
		writeError(w, r, upstream("relay pipeline not wired"))
		return
	}
	order, err := s.deps.Orders.GetOrder(r.Context(), orderID)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	// 仅未付且未过期订单受理 (openapi: unpaid & unexpired).
	if order.Status != types.OrderAwaitingPayment && order.Status != types.OrderPartiallyPaid {
		writeError(w, r, conflict(CodeOrderStateInvalid, "order is not awaiting payment"))
		return
	}
	if s.deps.now().After(order.ExpiresAt) {
		writeError(w, r, conflict(CodeOrderStateInvalid, "order already expired"))
		return
	}
	asset, err := s.deps.Store.GetSupportedAsset(r.Context(), order.ChainID, order.AssetSymbol)
	if err != nil {
		writeError(w, r, notFound("unsupported asset"))
		return
	}
	if !asset.SupportsEIP3009 {
		writeError(w, r, apiErr(http.StatusBadRequest, CodeSignerRejected, "asset does not support EIP-3009 gasless relay"))
		return
	}
	cc, ok := s.deps.Config.Chains[order.ChainID]
	if !ok {
		writeError(w, r, upstream("chain not configured"))
		return
	}
	dep, err := s.deps.Store.GetDepositAddress(r.Context(), order.DepositAddressID)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	// ---- 字段级绑定校验 (偏差一律 400 SIGNER_REJECTED-class) ----
	from, err := parseEVMAddress(req.From)
	if err != nil {
		writeError(w, r, badRequest("invalid from address"))
		return
	}
	to, err := parseEVMAddress(req.To)
	if err != nil {
		writeError(w, r, badRequest("invalid to address"))
		return
	}
	nonce, err := parseBytes32(req.Nonce)
	if err != nil {
		writeError(w, r, badRequest("invalid nonce"))
		return
	}
	rSig, err := parseBytes32(req.R)
	if err != nil {
		writeError(w, r, badRequest("invalid r"))
		return
	}
	sSig, err := parseBytes32(req.S)
	if err != nil {
		writeError(w, r, badRequest("invalid s"))
		return
	}
	// FIX-D5: v 接受 27/28 或归一化 yParity 0/1 (openapi Eip3009Submission
	// 口径); yParity 一律抬升为 27/28 后再进 calldata.
	switch req.V {
	case 0, 1:
		req.V += 27
	case 27, 28:
	default:
		writeError(w, r, badRequest("v must be 27/28 or normalized yParity 0/1"))
		return
	}
	if to != cc.EIP3009Router {
		writeError(w, r, apiErr(http.StatusBadRequest, CodeSignerRejected, "to != chain EIP3009PayRouter"))
		return
	}
	if req.Value.Cmp(order.ExpectedAmount) != 0 {
		writeError(w, r, apiErr(http.StatusBadRequest, CodeSignerRejected, "value != order.expected_amount"))
		return
	}
	if req.ValidBefore != order.ExpiresAt.Unix() {
		writeError(w, r, apiErr(http.StatusBadRequest, CodeSignerRejected, "valid_before != order.expires_at"))
		return
	}
	wantNonce := AuthNonce(uint64(order.ChainID), dep.Create2Salt)
	if nonce != wantNonce {
		writeError(w, r, apiErr(http.StatusBadRequest, CodeSignerRejected, "nonce != authNonce(chain_id, salt)"))
		return
	}
	// ABI 编码 payOrderWithAuthorization 并入队 EIP3009_RELAY 意图.
	calldata := BuildEIP3009RelayCalldata(
		dep.Create2Salt, from, req.Value.Big(),
		big.NewInt(req.ValidAfter), big.NewInt(req.ValidBefore),
		nonce, uint8(req.V), rSig, sSig,
	)
	gasLimit := cc.RelayGasLimit
	if gasLimit == 0 {
		gasLimit = 200_000
	}
	intent, err := s.deps.TxManager.QueueIntent(r.Context(), ports.QueueIntentRequest{
		IntentType:           types.IntentTypeEIP3009Relay,
		ReferenceID:          order.ID,
		ChainID:              order.ChainID,
		WalletAddress:        cc.RelayerHotWallet,
		To:                   cc.EIP3009Router,
		Value:                types.U256{},
		Calldata:             calldata,
		GasLimit:             gasLimit,
		MaxFeePerGas:         cc.MaxFeePerGas,
		MaxPriorityFeePerGas: cc.MaxPriorityFeePerGas,
	})
	if err != nil {
		writeError(w, r, mapError(err, CodeOrderStateInvalid))
		return
	}
	writeJSON(w, http.StatusAccepted, eip3009AcceptedDTO{
		OrderID:      order.ID.String(),
		IntentID:     intent.ID.String(),
		IntentStatus: intent.Status.String(),
	})
}

// GET /v1/public/payments/{orderId}/events — SSE 流 (见 handlers_sse.go)

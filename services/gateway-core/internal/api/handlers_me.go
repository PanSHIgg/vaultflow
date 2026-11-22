package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vaultflow/gateway-core/internal/exchange"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// GET /v1/me/deposit-address/{chain_id} — 每用户每链常驻充值地址 (惰性分配)
// ---------------------------------------------------------------------------

// depositAddressDTO web DepositAddress 契约.
type depositAddressDTO struct {
	Address       string        `json:"address"`
	ChainID       types.ChainID `json:"chain_id"`
	TokenContract string        `json:"token_contract"`
	QRPayload     string        `json:"qr_payload"`
}

func (s *Server) getDepositAddress(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromCtx(r)
	n, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("chain_id")), 10, 64)
	if err != nil || n == 0 {
		writeError(w, r, badRequest("invalid chain_id"))
		return
	}
	chainID := types.ChainID(n)
	if _, ok := s.deps.Config.Chains[chainID]; !ok {
		writeError(w, r, badRequest("unsupported chain_id"))
		return
	}
	uda, err := s.deps.Exchange.GetOrCreateDepositAddress(r.Context(), userID, chainID)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	// token_contract = 该链首个 supported_assets 行 (USDC 优先).
	contract, ae := s.primaryTokenContract(r, chainID)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	addr := lowerHexAddr(uda.Address)
	writeJSON(w, http.StatusOK, depositAddressDTO{
		Address:       addr,
		ChainID:       chainID,
		TokenContract: contract,
		QRPayload:     addr, // 契约: qr_payload = 裸地址串
	})
}

// primaryTokenContract 取该链展示用主币合约: 白名单首行, USDC 优先.
func (s *Server) primaryTokenContract(r *http.Request, chainID types.ChainID) (string, *APIError) {
	assets, err := s.deps.Store.ListAssets(r.Context())
	if err != nil {
		return "", mapError(err)
	}
	fallback := ""
	for _, a := range assets {
		if a.ChainID != chainID {
			continue
		}
		if a.AssetSymbol == "USDC" {
			return lowerHexAddr(a.TokenContract), nil
		}
		if fallback == "" {
			fallback = lowerHexAddr(a.TokenContract)
		}
	}
	if fallback == "" {
		return "", badRequest("no supported assets on this chain")
	}
	return fallback, nil
}

// ---------------------------------------------------------------------------
// GET /v1/me/deposits?chain_id=&cursor=&limit= — 充值记录 (keyset 分页)
// ---------------------------------------------------------------------------

// depositDTO web DepositRecord 契约 (chain_id 附加透出, 前端可忽略).
type depositDTO struct {
	TxHash                string        `json:"tx_hash"`
	TokenSymbol           string        `json:"token_symbol"`
	ChainID               types.ChainID `json:"chain_id"`
	Amount                types.U256    `json:"amount"`
	Decimals              int           `json:"decimals"`
	Confirmations         int64         `json:"confirmations"`
	RequiredConfirmations int           `json:"required_confirmations"`
	Status                string        `json:"status"`
	CreatedAt             string        `json:"created_at"`
}

// depositPageDTO {items, next_cursor} — next_cursor 无下一页时为 null.
type depositPageDTO struct {
	Items      []depositDTO `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}

const (
	depositsDefaultLimit = 20
	depositsMaxLimit     = 50
)

func (s *Server) listDeposits(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromCtx(r)
	chainID, ae := parseChainIDQuery(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	limit := depositsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > depositsMaxLimit {
			writeError(w, r, badRequest("limit must be an integer in [1,50]"))
			return
		}
		limit = n
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	page, err := s.deps.Store.ListUserDeposits(r.Context(), userID, chainID, cursor, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(w, r, invalidCursor("invalid cursor"))
			return
		}
		writeError(w, r, mapError(err))
		return
	}

	// safe_head: 页内出现的各链一次一查询 (无游标行 -> 0, 即全未确认).
	safeHeads := map[types.ChainID]int64{}
	safeHead := func(cid types.ChainID) int64 {
		if h, ok := safeHeads[cid]; ok {
			return h
		}
		h, err := s.deps.Store.GetSafeHeadHeight(r.Context(), cid)
		if err != nil {
			h = 0 // ErrNotFound 或抖动均按 0 兜底 — 只影响进度展示
		}
		safeHeads[cid] = h
		return h
	}

	// required_confirmations: (chain,asset) -> 白名单阈值; 资产缺失回退链配置.
	reqConfs, ae := s.requiredConfMap(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	reqConf := func(d store.DepositRecord) int {
		if rc, ok := reqConfs[assetKey{chain: d.ChainID, symbol: d.AssetSymbol}]; ok {
			return rc
		}
		if cc, ok := s.deps.Config.Chains[d.ChainID]; ok {
			return cc.RequiredConfirmations
		}
		return 0
	}

	out := depositPageDTO{Items: make([]depositDTO, 0, len(page.Data))}
	for _, d := range page.Data {
		conf := safeHead(d.ChainID) - d.BlockNumber + 1
		if conf < 0 {
			conf = 0
		}
		out.Items = append(out.Items, depositDTO{
			TxHash:                lowerHexHash(d.TxHash),
			TokenSymbol:           d.AssetSymbol,
			ChainID:               d.ChainID,
			Amount:                d.Amount,
			Decimals:              d.Decimals,
			Confirmations:         conf,
			RequiredConfirmations: reqConf(d),
			Status:                exchange.MapDepositStatus(d.Status), // ORPHANED_* -> PENDING
			CreatedAt:             d.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	if page.HasMore {
		out.NextCursor = &page.NextCursor
	}
	writeJSON(w, http.StatusOK, out)
}

// assetKey (chain_id, asset_symbol) 复合键.
type assetKey struct {
	chain  types.ChainID
	symbol string
}

// requiredConfMap supported_assets 的 (chain,symbol) -> required_confirmations.
func (s *Server) requiredConfMap(r *http.Request) (map[assetKey]int, *APIError) {
	assets, err := s.deps.Store.ListAssets(r.Context())
	if err != nil {
		return nil, mapError(err)
	}
	out := make(map[assetKey]int, len(assets))
	for _, a := range assets {
		out[assetKey{chain: a.ChainID, symbol: a.AssetSymbol}] = a.RequiredConfirmations
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// GET /v1/me/balances — 用户资产余额 (available + pending 并集)
// ---------------------------------------------------------------------------

// balanceDTO web Balance 契约: available/pending 为十进制字符串.
type balanceDTO struct {
	TokenSymbol string     `json:"token_symbol"`
	Available   types.U256 `json:"available"`
	Pending     types.U256 `json:"pending"`
	Decimals    int        `json:"decimals"`
}

func (s *Server) listBalances(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromCtx(r)
	balances, err := s.deps.Store.ListUserBalances(r.Context(), userID)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	pendings, err := s.deps.Store.ListPendingDepositSums(r.Context(), userID)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}

	// 并集: 余额行 (available) ∪ pending 聚合 (DETECTED/FINALIZED 充值).
	rows := map[string]*balanceDTO{}
	for _, b := range balances {
		rows[b.AssetSymbol] = &balanceDTO{
			TokenSymbol: b.AssetSymbol,
			Available:   b.Available,
			Decimals:    b.Decimals,
		}
	}
	for _, p := range pendings {
		row, ok := rows[p.AssetSymbol]
		if !ok {
			row = &balanceDTO{TokenSymbol: p.AssetSymbol, Decimals: p.Decimals}
			rows[p.AssetSymbol] = row
		}
		row.Pending = p.Total
	}
	out := make([]balanceDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TokenSymbol < out[j].TokenSymbol
	})
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /v1/me/withdrawals — 提现申请 (余额冻结 available->pending, QUEUED;
//   白名单注册/冷却/出账由 backend withdrawer 异步推进)
// GET  /v1/me/withdrawals — 提现历史
// ---------------------------------------------------------------------------

// createWithdrawalReq 提现申请体 (amount 十进制原子单位字符串).
type createWithdrawalReq struct {
	ChainID        uint64 `json:"chain_id"`
	TokenSymbol    string `json:"token_symbol"`
	ToAddress      string `json:"to_address"`
	Amount         string `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

// withdrawalDTO web Withdrawal 契约.
type withdrawalDTO struct {
	ID            string        `json:"id"`
	ChainID       types.ChainID `json:"chain_id"`
	TokenSymbol   string        `json:"token_symbol"`
	TokenContract string        `json:"token_contract"`
	ToAddress     string        `json:"to_address"`
	Amount        types.U256    `json:"amount"`
	Status        string        `json:"status"`
	TxHash        string        `json:"tx_hash,omitempty"`
	FailReason    string        `json:"fail_reason,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

func toWithdrawalDTO(w store.Withdrawal) withdrawalDTO {
	return withdrawalDTO{
		ID:            w.ID.String(),
		ChainID:       w.ChainID,
		TokenSymbol:   w.AssetSymbol,
		TokenContract: lowerHexAddr(w.TokenContract),
		ToAddress:     lowerHexAddr(w.ToAddress),
		Amount:        w.Amount,
		Status:        w.Status,
		TxHash:        w.TxHash,
		FailReason:    w.FailReason,
		CreatedAt:     w.CreatedAt,
		UpdatedAt:     w.UpdatedAt,
	}
}

func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromCtx(r)
	var req createWithdrawalReq
	if ae := decodeJSONBody(r, &req, 1<<16); ae != nil {
		writeError(w, r, ae)
		return
	}
	chainID := types.ChainID(req.ChainID)
	if _, ok := s.deps.Config.Chains[chainID]; !ok {
		writeError(w, r, badRequest("unsupported chain_id"))
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(req.TokenSymbol))
	if symbol == "" || !evmAddressRe.MatchString(req.ToAddress) {
		writeError(w, r, badRequest("invalid token_symbol or to_address"))
		return
	}
	amount, perr := types.ParseU256(req.Amount)
	if perr != nil || amount.IsZero() {
		writeError(w, r, badRequest("amount must be a positive decimal string"))
		return
	}
	// (chain_id, symbol) 必须在 supported_assets 白名单内.
	var contract types.Address
	found := false
	if assets, err := s.deps.Store.ListAssets(r.Context()); err == nil {
		for _, a := range assets {
			if a.ChainID == chainID && strings.EqualFold(a.AssetSymbol, symbol) {
				contract = a.TokenContract
				found = true
				break
			}
		}
	}
	if !found {
		writeError(w, r, badRequest("asset not supported on this chain"))
		return
	}
	toAddr := types.HexToAddress(req.ToAddress)
	if toAddr == (types.Address{}) {
		writeError(w, r, badRequest("zero to_address"))
		return
	}
	// 幂等槽: Idempotency-Key 头优先, body idempotency_key 兜底 (≤128 字符).
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem == "" {
		idem = strings.TrimSpace(req.IdempotencyKey)
	}
	if len(idem) > 128 {
		idem = idem[:128]
	}
	wd, err := s.deps.Store.RequestWithdrawal(r.Context(), userID, chainID,
		symbol, contract, toAddr, amount, idem)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toWithdrawalDTO(wd))
}

func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromCtx(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	wds, err := s.deps.Store.ListUserWithdrawals(r.Context(), userID, limit)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	out := make([]withdrawalDTO, 0, len(wds))
	for _, wd := range wds {
		out = append(out, toWithdrawalDTO(wd))
	}
	writeJSON(w, http.StatusOK, out)
}

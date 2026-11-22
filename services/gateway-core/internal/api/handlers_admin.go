package api

import (
	"net/http"
	"strings"
	"time"

	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// /v1/admin/wallets — platform_wallets CRUD (RELAYER/PAYOUT/COLD)
//
// 签名机联动顺序约定 (先 signer 后 DB, 失败即 502 且不落库):
//   - COLD 新增/启用/禁用/删除 -> UpdateColdWallets(add/remove) 先行;
//   - RELAYER/PAYOUT 新增      -> GenerateWallet 由 signer KMS 出密钥对,
//     客户端自报 address 一律 400 (防网关侧地址替换).
// ---------------------------------------------------------------------------

// walletDTO admin 平台钱包行视图.
type walletDTO struct {
	ID        string        `json:"id"`
	ChainID   types.ChainID `json:"chain_id"`
	Role      string        `json:"role"`
	Address   string        `json:"address"`
	Status    string        `json:"status"`
	CreatedAt string        `json:"created_at"`
}

func walletToDTO(w store.PlatformWallet) walletDTO {
	return walletDTO{
		ID:        w.ID.String(),
		ChainID:   w.ChainID,
		Role:      w.Role,
		Address:   lowerHexAddr(w.Address),
		Status:    w.Status,
		CreatedAt: w.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func walletRoleValid(role string) bool {
	return role == store.WalletRoleRelayer || role == store.WalletRolePayout || role == store.WalletRoleCold
}

// listWallets GET /v1/admin/wallets?chain_id= -> 200 [walletDTO...]
func (s *Server) listWallets(w http.ResponseWriter, r *http.Request) {
	chainID, ae := parseChainIDQuery(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	wallets, err := s.deps.Store.ListPlatformWallets(r.Context(), chainID)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	out := make([]walletDTO, 0, len(wallets))
	for _, wlt := range wallets {
		out = append(out, walletToDTO(wlt))
	}
	writeJSON(w, http.StatusOK, out)
}

// createWalletRequest POST 体. role=COLD 时 address 必填; RELAYER/PAYOUT
// 由 signer 生成, address 必须为空 (非空即 400 — 已冻结策略, 防地址替换).
type createWalletRequest struct {
	ChainID uint64 `json:"chain_id"`
	Role    string `json:"role"`
	Address string `json:"address"`
}

// createWallet POST /v1/admin/wallets -> 201 walletDTO.
func (s *Server) createWallet(w http.ResponseWriter, r *http.Request) {
	var req createWalletRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	if req.ChainID == 0 {
		writeError(w, r, badRequest("chain_id is required"))
		return
	}
	chainID := types.ChainID(req.ChainID)
	role := strings.ToUpper(strings.TrimSpace(req.Role))
	if !walletRoleValid(role) {
		writeError(w, r, badRequest("role must be one of RELAYER|PAYOUT|COLD"))
		return
	}
	if s.deps.Signer == nil {
		writeError(w, r, signerUnavailable("signer client not wired"))
		return
	}

	var addr types.Address
	switch role {
	case store.WalletRoleCold:
		if isBlank(req.Address) {
			writeError(w, r, badRequest("address is required for role COLD"))
			return
		}
		parsed, err := parseEVMAddress(strings.TrimSpace(req.Address))
		if err != nil {
			writeError(w, r, badRequest("invalid address: must be 0x-prefixed 40-hex"))
			return
		}
		addr = parsed
		// 先同步 signer allowed_cold_wallets, 再落库.
		if _, err := s.deps.Signer.UpdateColdWallets(r.Context(), chainID,
			[]types.Address{addr}, nil); err != nil {
			writeError(w, r, signerUnavailable("vault-signer UpdateColdWallets failed"))
			return
		}
	default: // RELAYER / PAYOUT — signer KMS 生成, 拒绝客户端自报地址.
		if !isBlank(req.Address) {
			writeError(w, r, badRequest("address must be empty for RELAYER|PAYOUT (signer generates)"))
			return
		}
		gen, err := s.deps.Signer.GenerateWallet(r.Context(), chainID, role)
		if err != nil {
			writeError(w, r, signerUnavailable("vault-signer GenerateWallet failed"))
			return
		}
		addr = gen
	}

	wallet, err := s.deps.Store.InsertPlatformWallet(r.Context(), chainID, role, addr)
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, r, conflict(CodeConflict, "wallet already exists for this chain/role/address"))
			return
		}
		writeError(w, r, mapError(err))
		return
	}
	writeJSON(w, http.StatusCreated, walletToDTO(wallet))
}

// updateWalletRequest PUT 体: 仅允许状态切换.
type updateWalletRequest struct {
	Status string `json:"status"`
}

// updateWallet PUT /v1/admin/wallets/{id} {status:ACTIVE|DISABLED} -> 200 walletDTO.
// COLD 行先 UpdateColdWallets (DISABLED->remove / ACTIVE->add), signer 失败
// 返回 502 且不动 DB.
func (s *Server) updateWallet(w http.ResponseWriter, r *http.Request) {
	id, ae := parsePathUUID(r, "id")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	var req updateWalletRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(req.Status))
	if status != store.WalletStatusActive && status != store.WalletStatusDisabled {
		writeError(w, r, badRequest("status must be ACTIVE or DISABLED"))
		return
	}
	wallet, err := s.deps.Store.GetPlatformWallet(r.Context(), id)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	if wallet.Role == store.WalletRoleCold && wallet.Status != status {
		if s.deps.Signer == nil {
			writeError(w, r, signerUnavailable("signer client not wired"))
			return
		}
		var add, remove []types.Address
		if status == store.WalletStatusActive {
			add = []types.Address{wallet.Address}
		} else {
			remove = []types.Address{wallet.Address}
		}
		if _, err := s.deps.Signer.UpdateColdWallets(r.Context(), wallet.ChainID, add, remove); err != nil {
			writeError(w, r, signerUnavailable("vault-signer UpdateColdWallets failed"))
			return
		}
	}
	wallet, err = s.deps.Store.UpdatePlatformWalletStatus(r.Context(), id, status)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	writeJSON(w, http.StatusOK, walletToDTO(wallet))
}

// deleteWallet DELETE /v1/admin/wallets/{id} -> 204.
// COLD: UpdateColdWallets(remove) 先行后删行; RELAYER/PAYOUT: 仅删行
// (signer 侧密钥材料保留 — 已知限制, 重启轮换覆盖).
func (s *Server) deleteWallet(w http.ResponseWriter, r *http.Request) {
	id, ae := parsePathUUID(r, "id")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	wallet, err := s.deps.Store.GetPlatformWallet(r.Context(), id)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	if wallet.Role == store.WalletRoleCold {
		if s.deps.Signer == nil {
			writeError(w, r, signerUnavailable("signer client not wired"))
			return
		}
		if _, err := s.deps.Signer.UpdateColdWallets(r.Context(), wallet.ChainID,
			nil, []types.Address{wallet.Address}); err != nil {
			writeError(w, r, signerUnavailable("vault-signer UpdateColdWallets failed"))
			return
		}
	}
	if err := s.deps.Store.DeletePlatformWallet(r.Context(), id); err != nil {
		writeError(w, r, mapError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// GET /v1/admin/signer/health — vault-signer 运行态透传
// ---------------------------------------------------------------------------

// signerHealthDTO signer 健康投影 (ports.SignerHealth 字段直出,
// hash/address 小写 0x-hex).
type signerHealthDTO struct {
	Reachable        bool     `json:"reachable"`
	Version          string   `json:"version"`
	UptimeSeconds    uint64   `json:"uptime_seconds"`
	SignerClock      string   `json:"signer_clock"`
	WhitelistEntries uint32   `json:"whitelist_entries"`
	AuditEntries     uint64   `json:"audit_entries"`
	AuditTipHash     string   `json:"audit_tip_hash"`
	LoadedWallets    []string `json:"loaded_wallets"`
}

func (s *Server) getSignerHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Signer == nil {
		writeError(w, r, signerUnavailable("signer client not wired"))
		return
	}
	h, err := s.deps.Signer.Health(r.Context())
	if err != nil {
		writeError(w, r, signerUnavailable("vault-signer unreachable"))
		return
	}
	out := signerHealthDTO{
		Reachable:        true,
		Version:          h.Version,
		UptimeSeconds:    h.UptimeSeconds,
		SignerClock:      h.SignerClock.UTC().Format(time.RFC3339),
		WhitelistEntries: h.WhitelistEntries,
		AuditEntries:     h.AuditEntries,
		AuditTipHash:     lowerHexHash(h.AuditTipHash),
		LoadedWallets:    make([]string, 0, len(h.LoadedWallets)),
	}
	for _, addr := range h.LoadedWallets {
		out.LoadedWallets = append(out.LoadedWallets, lowerHexAddr(addr))
	}
	writeJSON(w, http.StatusOK, out)
}

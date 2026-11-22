package api

import (
	"net/http"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// GET /v1/assets — 支持的币种/网络列表 (public)
// ---------------------------------------------------------------------------

// assetDTO web Asset 契约: 金额 NUMERIC(78,0) 十进制字符串; 可选字段 omitempty.
type assetDTO struct {
	TokenSymbol           string        `json:"token_symbol"`
	ChainID               types.ChainID `json:"chain_id"`
	ChainName             string        `json:"chain_name,omitempty"`
	TokenContract         string        `json:"token_contract"`
	TokenExplorerURL      string        `json:"token_explorer_url,omitempty"`
	BlockTimeSeconds      int           `json:"block_time_seconds,omitempty"`
	RequiredConfirmations int           `json:"required_confirmations"`
	MinDeposit            types.U256    `json:"min_deposit"`
	Decimals              int           `json:"decimals"`
}

// listAssets 200 [{assetDTO...}] — supported_assets 白名单全量 (EVM 链).
func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	assets, err := s.deps.Store.ListAssets(r.Context())
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	out := make([]assetDTO, 0, len(assets))
	for _, a := range assets {
		out = append(out, assetDTO{
			TokenSymbol:           a.AssetSymbol,
			ChainID:               a.ChainID,
			ChainName:             a.ChainName,
			TokenContract:         lowerHexAddr(a.TokenContract),
			TokenExplorerURL:      a.TokenExplorerURL,
			BlockTimeSeconds:      a.BlockTimeSeconds,
			RequiredConfirmations: a.RequiredConfirmations,
			MinDeposit:            a.MinDeposit,
			Decimals:              a.Decimals,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/types"
)

const assetCols = `chain_id, asset_symbol, token_contract, decimals, supports_eip3009,
	min_deposit::text, chain_name, block_time_seconds, required_confirmations,
	token_explorer_url`

func scanAssetInfo(row pgx.Row) (AssetInfo, error) {
	var a AssetInfo
	var tc, minDep string
	var chain int64
	err := row.Scan(&chain, &a.AssetSymbol, &tc, &a.Decimals, &a.SupportsEIP3009,
		&minDep, &a.ChainName, &a.BlockTimeSeconds, &a.RequiredConfirmations,
		&a.TokenExplorerURL)
	if err != nil {
		return a, err
	}
	a.ChainID = types.ChainID(chain)
	a.TokenContract = types.HexToAddress(tc)
	a.MinDeposit = parseU256Text(minDep)
	return a, nil
}

// ListAssets 全部 supported_assets 白名单行 (含 007/008 增补列),
// 供 GET /v1/assets 与充值分类/确认数判定.
func (s *Store) ListAssets(ctx context.Context) ([]AssetInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+assetCols+` FROM supported_assets ORDER BY chain_id, asset_symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetInfo{}
	for rows.Next() {
		a, err := scanAssetInfo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

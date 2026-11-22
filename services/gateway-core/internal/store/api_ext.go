package store

// api_ext.go — gateway REST API 专用读侧查询扩展 (W-gateway 窗口新增文件).
// 仅承载 gateway 进程需要、而 store 既有文件未覆盖的只读/单查投影:
//   - GetSafeHeadHeight:        chain_sync_cursors.safe_head_height (确认数进度);
//   - ListPendingDepositSums:   /v1/me/balances 的 pending 聚合;
//   - GetPlatformWallet:        admin 钱包 PUT/DELETE 前置按 id 单查.

import (
	"context"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// GetSafeHeadHeight 读取 chain_sync_cursors.safe_head_height (indexer 单写者持久化).
// 该链尚无游标行 (indexer 未起跑/链未接入) 时返回 ErrNotFound — 调用方按 0 处理.
func (s *Store) GetSafeHeadHeight(ctx context.Context, chainID types.ChainID) (int64, error) {
	var h int64
	err := s.pool.QueryRow(ctx,
		`SELECT safe_head_height FROM chain_sync_cursors WHERE chain_id=$1`,
		int64(chainID)).Scan(&h)
	if err != nil {
		return 0, mapNoRows(err)
	}
	return h, nil
}

// PendingDepositSum /v1/me/balances 的 pending 聚合行:
// 该用户 status IN ('DETECTED','FINALIZED') 的 onchain_deposits
// 按 asset_symbol 跨链汇总 (经 token_contract join supported_assets 解析符号).
type PendingDepositSum struct {
	AssetSymbol string
	Total       types.U256
	Decimals    int
}

// ListPendingDepositSums 用户待确认入账的充值聚合 (DETECTED=捕获未达标,
// FINALIZED=达标待贷记). token_contract 不在 supported_assets 白名单内的充值
// 无法归并到资产余额维度, 直接跳过 (与 CreditDepositAtomically 的
// ErrUnknownAsset 拒绝口径一致).
func (s *Store) ListPendingDepositSums(ctx context.Context, userID uuid.UUID) ([]PendingDepositSum, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.asset_symbol, SUM(d.amount)::text, MIN(a.decimals)
		  FROM onchain_deposits d
		  JOIN supported_assets a
		    ON a.chain_id=d.chain_id AND a.token_contract=d.token_contract
		 WHERE d.user_id=$1 AND d.status IN ($2, $3)
		 GROUP BY a.asset_symbol
		 ORDER BY a.asset_symbol`,
		userID, DepositStatusDetected, DepositStatusFinalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingDepositSum{}
	for rows.Next() {
		var p PendingDepositSum
		var total string
		if err := rows.Scan(&p.AssetSymbol, &total, &p.Decimals); err != nil {
			return nil, err
		}
		p.Total = parseU256Text(total)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlatformWallet 按主键单查 platform_wallets 行
// (admin PUT/DELETE 需在变更前取 role/address — store 既有方法仅支持
// list/by-role 查询, 故在此补齐).
func (s *Store) GetPlatformWallet(ctx context.Context, id uuid.UUID) (PlatformWallet, error) {
	w, err := scanPlatformWallet(s.pool.QueryRow(ctx,
		`SELECT `+pwCols+` FROM platform_wallets WHERE id=$1`, id))
	return w, mapNoRows(err)
}

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/types"
)

const pwCols = `id, chain_id, role, address, status, created_at`

func scanPlatformWallet(row pgx.Row) (PlatformWallet, error) {
	var w PlatformWallet
	var addr string
	var chain int64
	err := row.Scan(&w.ID, &chain, &w.Role, &addr, &w.Status, &w.CreatedAt)
	if err != nil {
		return w, err
	}
	w.ChainID = types.ChainID(chain)
	w.Address = types.HexToAddress(addr)
	return w, nil
}

// ListPlatformWallets admin CRUD 列表; chainID 可选过滤, 按 chain_id, role, created_at.
func (s *Store) ListPlatformWallets(ctx context.Context, chainID *types.ChainID) ([]PlatformWallet, error) {
	q := `SELECT ` + pwCols + ` FROM platform_wallets`
	args := []any{}
	if chainID != nil {
		q += ` WHERE chain_id=$1`
		args = append(args, int64(*chainID))
	}
	q += ` ORDER BY chain_id, role, created_at`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlatformWallet{}
	for rows.Next() {
		w, err := scanPlatformWallet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// InsertPlatformWallet 新增平台钱包 (默认 status='ACTIVE'); role 须为
// WalletRole* 之一 (CHECK 兜底). 唯一冲突返回 23505.
func (s *Store) InsertPlatformWallet(ctx context.Context, chainID types.ChainID,
	role string, addr types.Address) (PlatformWallet, error) {
	return scanPlatformWallet(s.pool.QueryRow(ctx, `
		INSERT INTO platform_wallets (chain_id, role, address)
		VALUES ($1, $2, $3)
		RETURNING `+pwCols, int64(chainID), role, addrHex(addr)))
}

// UpdatePlatformWalletStatus ACTIVE<->DISABLED 状态切换; 未命中返回 ErrNotFound.
func (s *Store) UpdatePlatformWalletStatus(ctx context.Context, id uuid.UUID, status string) (PlatformWallet, error) {
	w, err := scanPlatformWallet(s.pool.QueryRow(ctx, `
		UPDATE platform_wallets SET status=$2 WHERE id=$1 RETURNING `+pwCols, id, status))
	return w, mapNoRows(err)
}

// DeletePlatformWallet 物理删除; 未命中返回 ErrNotFound.
func (s *Store) DeletePlatformWallet(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM platform_wallets WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: platform wallet %s", ErrNotFound, id)
	}
	return nil
}

// GetActiveColdWallets sweep/再平衡目标: 该链全部 ACTIVE 冷金库, 按创建顺序.
func (s *Store) GetActiveColdWallets(ctx context.Context, chainID types.ChainID) ([]PlatformWallet, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+pwCols+` FROM platform_wallets
		 WHERE chain_id=$1 AND role=$2 AND status=$3
		 ORDER BY created_at`, int64(chainID), WalletRoleCold, WalletStatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlatformWallet{}
	for rows.Next() {
		w, err := scanPlatformWallet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetPlatformWalletByRole 该链该角色的第一个 ACTIVE 钱包
// (RELAYER/PAYOUT 单例语义); 未配置返回 ErrNotFound.
func (s *Store) GetPlatformWalletByRole(ctx context.Context, chainID types.ChainID, role string) (PlatformWallet, error) {
	w, err := scanPlatformWallet(s.pool.QueryRow(ctx, `
		SELECT `+pwCols+` FROM platform_wallets
		 WHERE chain_id=$1 AND role=$2 AND status=$3
		 ORDER BY created_at LIMIT 1`, int64(chainID), role, WalletStatusActive))
	return w, mapNoRows(err)
}

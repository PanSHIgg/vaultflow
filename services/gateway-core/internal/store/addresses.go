package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/types"
)

const udaCols = `id, user_id, chain_id, address, create2_salt, is_deployed, is_quarantined, created_at`

func scanUserDepositAddress(row pgx.Row) (UserDepositAddress, error) {
	var d UserDepositAddress
	var addr string
	var salt []byte
	var chain int64
	err := row.Scan(&d.ID, &d.UserID, &chain, &addr, &salt,
		&d.IsDeployed, &d.IsQuarantined, &d.CreatedAt)
	if err != nil {
		return d, err
	}
	d.ChainID = types.ChainID(chain)
	d.Address = types.HexToAddress(addr)
	if len(salt) == 32 {
		copy(d.Create2Salt[:], salt)
	}
	return d, nil
}

// GetUserDepositAddress 查 (user_id, chain_id) 的常驻充值地址; 未分配返回 ErrNotFound.
func (s *Store) GetUserDepositAddress(ctx context.Context, userID uuid.UUID, chainID types.ChainID) (UserDepositAddress, error) {
	d, err := scanUserDepositAddress(s.pool.QueryRow(ctx,
		`SELECT `+udaCols+` FROM user_deposit_addresses
		  WHERE user_id=$1 AND chain_id=$2`, userID, int64(chainID)))
	return d, mapNoRows(err)
}

// InsertUserDepositAddress 写入新分配地址并 RETURNING 全行.
// UNIQUE(user_id,chain_id) 竞态冲突返回 23505, 由调用方重选
// (见 exchange.Service.GetOrCreateDepositAddress / IsUniqueViolation).
func (s *Store) InsertUserDepositAddress(ctx context.Context, userID uuid.UUID, chainID types.ChainID,
	addr types.Address, salt types.Hash) (UserDepositAddress, error) {
	return scanUserDepositAddress(s.pool.QueryRow(ctx, `
		INSERT INTO user_deposit_addresses (user_id, chain_id, address, create2_salt)
		VALUES ($1, $2, $3, $4)
		RETURNING `+udaCols, userID, int64(chainID), addrHex(addr), salt.Bytes()))
}

// MarkUserDepositAddressDeployed 归集器部署 EIP-1167 proxy 后置位.
func (s *Store) MarkUserDepositAddressDeployed(ctx context.Context, id uuid.UUID) (UserDepositAddress, error) {
	d, err := scanUserDepositAddress(s.pool.QueryRow(ctx, `
		UPDATE user_deposit_addresses SET is_deployed=true
		  WHERE id=$1 RETURNING `+udaCols, id))
	return d, mapNoRows(err)
}

// MarkUserDepositAddressQuarantined VaultQuarantined (黑名单/revert) 置位.
func (s *Store) MarkUserDepositAddressQuarantined(ctx context.Context, id uuid.UUID) (UserDepositAddress, error) {
	d, err := scanUserDepositAddress(s.pool.QueryRow(ctx, `
		UPDATE user_deposit_addresses SET is_quarantined=true
		  WHERE id=$1 RETURNING `+udaCols, id))
	return d, mapNoRows(err)
}

// LookupUserByAddress indexer 侧归属反查: (chain_id, to_address) -> 归属用户
// 充值地址行. 未白名单地址 (国库热钱包/未知地址) 返回 ErrNotFound, 调用方据此
// 落 UNMATCHED/TREASURY_* 分类且 user_id 留 NULL.
func (s *Store) LookupUserByAddress(ctx context.Context, chainID types.ChainID, addr types.Address) (UserDepositAddress, error) {
	d, err := scanUserDepositAddress(s.pool.QueryRow(ctx,
		`SELECT `+udaCols+` FROM user_deposit_addresses
		  WHERE chain_id=$1 AND address=$2`, int64(chainID), addrHex(addr)))
	return d, mapNoRows(err)
}

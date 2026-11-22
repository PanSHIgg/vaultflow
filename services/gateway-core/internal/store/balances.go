package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ubCols ListUserBalances / CreditUserBalance 返回列
// (decimals 取 supported_assets 中该资产符号的任一白名单行, 展示用).
const ubCols = `b.user_id, b.asset_symbol, b.available::text, b.pending::text,
	COALESCE((SELECT MIN(a.decimals) FROM supported_assets a WHERE a.asset_symbol=b.asset_symbol), -1),
	b.updated_at`

func scanUserBalance(row pgx.Row) (UserBalance, error) {
	var b UserBalance
	var avail, pend string
	err := row.Scan(&b.UserID, &b.AssetSymbol, &avail, &pend, &b.Decimals, &b.UpdatedAt)
	if err != nil {
		return b, err
	}
	b.Available = parseU256Text(avail)
	b.Pending = parseU256Text(pend)
	return b, nil
}

// ListUserBalances 用户全部资产余额 (含 0 余额行; 按 asset_symbol 排序).
func (s *Store) ListUserBalances(ctx context.Context, userID uuid.UUID) ([]UserBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+ubCols+`
		  FROM user_balances b
		 WHERE b.user_id=$1
		 ORDER BY b.asset_symbol`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserBalance{}
	for rows.Next() {
		b, err := scanUserBalance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreditUserBalance UPSERT 累加 available (原子单位 delta, 必须 > 0).
// 余额按 (user, asset_symbol) 聚合 — 链只是入金通道.
func (s *Store) CreditUserBalance(ctx context.Context, userID uuid.UUID,
	symbol string, delta types.U256) (UserBalance, error) {
	return scanUserBalance(s.pool.QueryRow(ctx, `
		WITH upsert AS (
			INSERT INTO user_balances (user_id, asset_symbol, available)
			VALUES ($1, $2, $3::numeric)
			ON CONFLICT (user_id, asset_symbol)
			DO UPDATE SET available = user_balances.available + EXCLUDED.available,
			              updated_at = now()
			RETURNING *
		)
		SELECT `+ubCols+` FROM upsert b`,
		userID, symbol, delta.String()))
}

// CreditDepositAtomically 充值入账 — 单事务内:
//  1. SELECT ... FOR UPDATE 锁定 onchain_deposits 行;
//  2. 幂等: status='CREDITED' 直接返回 nil; 非 'FINALIZED' 返回
//     ports.ErrInvalidTransition;
//  3. user_id NULL -> ErrDepositUnassigned; token_contract 未白名单 ->
//     ErrUnknownAsset;
//  4. 置 status='CREDITED' + UPSERT user_balances.available += amount.
//
// backend credit worker 循环: ListFinalizedUncredited -> CreditDepositAtomically.
func (s *Store) CreditDepositAtomically(ctx context.Context, depositID uuid.UUID) error {
	return s.WithinTx(ctx, func(tx pgx.Tx) error {
		var chain int64
		var tokenContract, amtText, status string
		var userID *uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT chain_id, token_contract, amount::text, status, user_id
			  FROM onchain_deposits WHERE id=$1 FOR UPDATE`, depositID).
			Scan(&chain, &tokenContract, &amtText, &status, &userID)
		if err != nil {
			return mapNoRows(err)
		}
		if status == DepositStatusCredited || status == DepositStatusSwept {
			return nil // 幂等: 已入账/已归集
		}
		if status != DepositStatusFinalized {
			return fmt.Errorf("%w: deposit %s status=%s", ports.ErrInvalidTransition, depositID, status)
		}
		if userID == nil {
			return fmt.Errorf("%w: deposit %s", ErrDepositUnassigned, depositID)
		}
		var symbol string
		err = tx.QueryRow(ctx, `
			SELECT asset_symbol FROM supported_assets
			  WHERE chain_id=$1 AND token_contract=$2`, chain, tokenContract).Scan(&symbol)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: chain=%d contract=%s", ErrUnknownAsset, chain, tokenContract)
			}
			return err
		}
		amount, err := types.ParseU256(amtText)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE onchain_deposits SET status=$2 WHERE id=$1`,
			depositID, DepositStatusCredited); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO user_balances (user_id, asset_symbol, available)
			VALUES ($1, $2, $3::numeric)
			ON CONFLICT (user_id, asset_symbol)
			DO UPDATE SET available = user_balances.available + EXCLUDED.available,
			              updated_at = now()`,
			*userID, symbol, amount.String())
		return err
	})
}

// DebitUserBalanceOnOrphan 深度重组冲正 — 单事务内把已 CREDITED 的充值回滚:
// user_balances.available -= amount (余额不足返回 ports.ErrNegativeBalance,
// 需人工介入), deposit 置 ORPHANED_DEEP. 充值尚未 CREDITED 时为 no-op.
func (s *Store) DebitUserBalanceOnOrphan(ctx context.Context, depositID uuid.UUID) error {
	return s.WithinTx(ctx, func(tx pgx.Tx) error {
		var chain int64
		var tokenContract, amtText, status string
		var userID *uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT chain_id, token_contract, amount::text, status, user_id
			  FROM onchain_deposits WHERE id=$1 FOR UPDATE`, depositID).
			Scan(&chain, &tokenContract, &amtText, &status, &userID)
		if err != nil {
			return mapNoRows(err)
		}
		if status != DepositStatusCredited || userID == nil {
			return nil // 未入账, 无余额可冲; 孤块状态迁移由 indexer 负责
		}
		var symbol string
		err = tx.QueryRow(ctx, `
			SELECT asset_symbol FROM supported_assets
			  WHERE chain_id=$1 AND token_contract=$2`, chain, tokenContract).Scan(&symbol)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: chain=%d contract=%s", ErrUnknownAsset, chain, tokenContract)
			}
			return err
		}
		amount, err := types.ParseU256(amtText)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE user_balances
			   SET available = available - $3::numeric, updated_at = now()
			 WHERE user_id=$1 AND asset_symbol=$2
			   AND available >= $3::numeric`,
			*userID, symbol, amount.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: user=%s asset=%s amount=%s",
				ports.ErrNegativeBalance, *userID, symbol, amount.String())
		}
		_, err = tx.Exec(ctx,
			`UPDATE onchain_deposits SET status=$2 WHERE id=$1`,
			depositID, DepositStatusOrphanedDeep)
		return err
	})
}

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// 提现状态机 (migrations/010):
//
//	QUEUED      — API 已收单, 余额已冻结 (available->pending)
//	WHITELISTED — 白名单租约已注册, 冷却中
//	IN_FLIGHT   — 出账交易已入 txmanager 队列/广播
//	CONFIRMED   — 链上确认, pending 已核销 (终态)
//	FAILED      — 意图失败, pending 已退回 available (终态)
//	REJECTED    — signer 策略硬拒, pending 已退回 available (终态)
const (
	WithdrawalStatusQueued      = "QUEUED"
	WithdrawalStatusWhitelisted = "WHITELISTED"
	WithdrawalStatusInFlight    = "IN_FLIGHT"
	WithdrawalStatusConfirmed   = "CONFIRMED"
	WithdrawalStatusFailed      = "FAILED"
	WithdrawalStatusRejected    = "REJECTED"
)

// Withdrawal withdrawals 表行.
type Withdrawal struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	ChainID       types.ChainID
	AssetSymbol   string
	TokenContract types.Address
	ToAddress     types.Address
	Amount        types.U256
	Status        string
	WhitelistedAt *time.Time
	IntentID      *uuid.UUID
	TxHash        string
	FailReason    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const wdCols = `id, user_id, chain_id, asset_symbol, token_contract, to_address,
	amount::text, status, whitelisted_at, intent_id, COALESCE(tx_hash,''),
	fail_reason, created_at, updated_at`

func scanWithdrawal(row pgx.Row) (Withdrawal, error) {
	var w Withdrawal
	var chain int64
	var amt, tokenContract, toAddr string
	err := row.Scan(&w.ID, &w.UserID, &chain, &w.AssetSymbol, &tokenContract,
		&toAddr, &amt, &w.Status, &w.WhitelistedAt, &w.IntentID,
		&w.TxHash, &w.FailReason, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return w, err
	}
	w.ChainID = types.ChainID(chain)
	w.TokenContract = types.HexToAddress(tokenContract)
	w.ToAddress = types.HexToAddress(toAddr)
	w.Amount = parseU256Text(amt)
	return w, nil
}

// RequestWithdrawal 收单 — 单事务内:
//  1. client_ref 命中既有行 → 幂等重放 (不重扣款);
//  2. 余额行锁定, available >= amount (不足 → ports.ErrNegativeBalance);
//  3. 冻结: available -= amount, pending += amount;
//  4. 落 withdrawals 行 (QUEUED).
// 并发同 client_ref: 一方 INSERT 23505 → 整事务回滚 → 重选获胜行.
func (s *Store) RequestWithdrawal(ctx context.Context, userID uuid.UUID, chainID types.ChainID,
	symbol string, tokenContract, toAddr types.Address, amount types.U256,
	clientRef string) (Withdrawal, error) {
	var w Withdrawal
	var replay bool
	err := s.WithinTx(ctx, func(tx pgx.Tx) error {
		if clientRef != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(
				SELECT 1 FROM withdrawals WHERE user_id=$1 AND client_ref=$2)`,
				userID, clientRef).Scan(&exists); err != nil {
				return err
			}
			if exists {
				replay = true
				return nil
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE user_balances
			   SET available = available - $3::numeric,
			       pending   = pending   + $3::numeric,
			       updated_at = now()
			 WHERE user_id=$1 AND asset_symbol=$2
			   AND available >= $3::numeric`,
			userID, symbol, amount.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: user=%s asset=%s amount=%s",
				ports.ErrNegativeBalance, userID, symbol, amount.String())
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO withdrawals (user_id, chain_id, asset_symbol, token_contract,
			                         to_address, amount, client_ref)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7)
			RETURNING `+wdCols,
			userID, int64(chainID), symbol, tokenContract.Hex(), toAddr.Hex(),
			amount.String(), nullableStr(clientRef))
		w, err = scanWithdrawal(row)
		return err
	})
	if err != nil {
		if clientRef != "" && IsUniqueViolation(err) {
			// 并发幂等槽竞态: 本事务已整体回滚 (未扣款), 返回获胜行.
			return s.withdrawalByClientRef(ctx, userID, clientRef)
		}
		return w, err
	}
	if replay {
		return s.withdrawalByClientRef(ctx, userID, clientRef)
	}
	return w, nil
}

// withdrawalByClientRef 幂等槽取行 (重放/竞态落败路径).
func (s *Store) withdrawalByClientRef(ctx context.Context, userID uuid.UUID, clientRef string) (Withdrawal, error) {
	return scanWithdrawal(s.pool.QueryRow(ctx, `SELECT `+wdCols+
		` FROM withdrawals WHERE user_id=$1 AND client_ref=$2`, userID, clientRef))
}

// nullableStr 空串 → NULL (client_ref 唯一索引仅约束非空值).
func nullableStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ListUserWithdrawals 用户提现历史 (新→旧).
func (s *Store) ListUserWithdrawals(ctx context.Context, userID uuid.UUID, limit int) ([]Withdrawal, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+wdCols+` FROM withdrawals WHERE user_id=$1
		 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Withdrawal{}
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListWithdrawalsByStatus worker 轮询: 取某链某状态的提现单 (FIFO).
func (s *Store) ListWithdrawalsByStatus(ctx context.Context, chainID types.ChainID,
	status string, limit int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+wdCols+` FROM withdrawals WHERE chain_id=$1 AND status=$2
		 ORDER BY created_at LIMIT $3`, int64(chainID), status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Withdrawal{}
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkWithdrawalWhitelisted 白名单租约注册成功, 记录冷却锚点.
func (s *Store) MarkWithdrawalWhitelisted(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE withdrawals SET status=$2, whitelisted_at=now(), updated_at=now()
		 WHERE id=$1 AND status=$3`,
		id, WithdrawalStatusWhitelisted, WithdrawalStatusQueued)
	return err
}

// AttachWithdrawalIntent 提现单已入 txmanager 队列 → IN_FLIGHT.
func (s *Store) AttachWithdrawalIntent(ctx context.Context, id, intentID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE withdrawals SET status=$2, intent_id=$3, updated_at=now()
		 WHERE id=$1 AND status=$4`,
		id, WithdrawalStatusInFlight, intentID, WithdrawalStatusWhitelisted)
	return err
}

// ListInFlightWithdrawals IN_FLIGHT 提现单 (worker 对账 intent 终态用).
func (s *Store) ListInFlightWithdrawals(ctx context.Context, chainID types.ChainID) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+wdCols+` FROM withdrawals WHERE chain_id=$1 AND status=$2
		 ORDER BY created_at`, int64(chainID), WithdrawalStatusInFlight)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Withdrawal{}
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// IntentTerminalStatus 查 intent 终态 (CONFIRMED 成功 / FAILED_* / CANCELLED_* 失败).
func (s *Store) IntentTerminalStatus(ctx context.Context, intentID uuid.UUID) (status, txHash string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT i.status, COALESCE((
			SELECT a.tx_hash FROM outbound_tx_attempts a
			 WHERE a.intent_id=i.id AND a.status='FINALIZED' LIMIT 1), '')
		  FROM outbound_tx_intents i WHERE i.id=$1`, intentID).
		Scan(&status, &txHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return status, txHash, err
}

// settleWithdrawal CONFIRMED/FAILED/REJECTED 终态结算 — 单事务内:
// CONFIRMED: pending -= amount (核销冻结); FAILED/REJECTED: pending -= amount,
// available += amount (退款). 幂等: 非 IN_FLIGHT 直接 no-op.
func (s *Store) settleWithdrawal(ctx context.Context, id uuid.UUID, final string,
	txHash, reason string) error {
	return s.WithinTx(ctx, func(tx pgx.Tx) error {
		var status string
		var amt string
		var userID uuid.UUID
		var symbol string
		if err := tx.QueryRow(ctx, `
			SELECT status, amount::text, user_id, asset_symbol
			  FROM withdrawals WHERE id=$1 FOR UPDATE`, id).
			Scan(&status, &amt, &userID, &symbol); err != nil {
			return mapNoRows(err)
		}
		if status != WithdrawalStatusInFlight {
			return nil // 幂等
		}
		refund := final != WithdrawalStatusConfirmed
		var err error
		if refund {
			_, err = tx.Exec(ctx, `
				UPDATE user_balances
				   SET pending = pending - $3::numeric,
				       available = available + $3::numeric,
				       updated_at = now()
				 WHERE user_id=$1 AND asset_symbol=$2 AND pending >= $3::numeric`,
				userID, symbol, amt)
		} else {
			_, err = tx.Exec(ctx, `
				UPDATE user_balances
				   SET pending = pending - $3::numeric, updated_at = now()
				 WHERE user_id=$1 AND asset_symbol=$2 AND pending >= $3::numeric`,
				userID, symbol, amt)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE withdrawals SET status=$2, tx_hash=$3, fail_reason=$4, updated_at=now()
			 WHERE id=$1`, id, final, txHash, reason)
		return err
	})
}

// SettleWithdrawalConfirmed 提现链上确认 → 冻结余额核销.
func (s *Store) SettleWithdrawalConfirmed(ctx context.Context, id uuid.UUID, txHash string) error {
	return s.settleWithdrawal(ctx, id, WithdrawalStatusConfirmed, txHash, "")
}

// SettleWithdrawalFailed 提现失败/拒签 → 冻结余额退回.
func (s *Store) SettleWithdrawalFailed(ctx context.Context, id uuid.UUID, reason string, rejected bool) error {
	final := WithdrawalStatusFailed
	if rejected {
		final = WithdrawalStatusRejected
	}
	return s.settleWithdrawal(ctx, id, final, "", reason)
}

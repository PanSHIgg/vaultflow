package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/types"
)

// depCols onchain_deposits 读投影列 (d.*) + LEFT JOIN supported_assets 展示列.
const depCols = `d.id, d.chain_id, d.block_hash, d.tx_hash, d.log_index, d.block_number,
	d.token_contract, d.from_address, d.to_address, d.amount::text,
	d.deposit_category, d.status, d.user_id, d.created_at,
	COALESCE(a.asset_symbol, ''), COALESCE(a.decimals, -1)`

const depJoin = ` FROM onchain_deposits d
	LEFT JOIN supported_assets a
	  ON a.chain_id=d.chain_id AND a.token_contract=d.token_contract`

func scanDepositRecord(row pgx.Row) (DepositRecord, error) {
	var d DepositRecord
	var bh, th, tc, fa, ta, amt string
	var chain int64
	var uid *uuid.UUID
	err := row.Scan(&d.ID, &chain, &bh, &th, &d.LogIndex, &d.BlockNumber,
		&tc, &fa, &ta, &amt, &d.Category, &d.Status, &uid, &d.CreatedAt,
		&d.AssetSymbol, &d.Decimals)
	if err != nil {
		return d, err
	}
	d.ChainID = types.ChainID(chain)
	d.BlockHash = types.HexToHash(bh)
	d.TxHash = types.HexToHash(th)
	d.TokenContract = types.HexToAddress(tc)
	d.FromAddress = types.HexToAddress(fa)
	d.ToAddress = types.HexToAddress(ta)
	d.Amount = parseU256Text(amt)
	if uid != nil {
		d.UserID = uuid.NullUUID{UUID: *uid, Valid: true}
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// 不透明游标 (base64url "created_at_RFC3339Nano|uuid")
// ---------------------------------------------------------------------------

func encodeDepositCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeDepositCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	ts, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, ErrInvalidCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return t, id, nil
}

// ListUserDeposits 用户充值历史, (created_at, id) DESC keyset 分页.
// chainID 可选过滤; cursor 为上页 NextCursor (空串=首页); limit<=0 时默认 50.
func (s *Store) ListUserDeposits(ctx context.Context, userID uuid.UUID, chainID *types.ChainID,
	cursor string, limit int) (DepositPage, error) {
	var sb strings.Builder
	args := []any{userID}
	sb.WriteString(`SELECT ` + depCols + depJoin + ` WHERE d.user_id=$1`)
	n := 1
	if chainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND d.chain_id=$%d`, n))
		args = append(args, int64(*chainID))
	}
	if cursor != "" {
		t, id, err := decodeDepositCursor(cursor)
		if err != nil {
			return DepositPage{}, err
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND (d.created_at, d.id) < ($%d, $%d)`, n, n+1))
		args = append(args, t, id)
		n++
	}
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY d.created_at DESC, d.id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return DepositPage{}, err
	}
	defer rows.Close()
	page := DepositPage{Data: []DepositRecord{}}
	for rows.Next() {
		d, err := scanDepositRecord(rows)
		if err != nil {
			return page, err
		}
		page.Data = append(page.Data, d)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
		last := page.Data[len(page.Data)-1]
		page.NextCursor = encodeDepositCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ListFinalizedUncredited backend credit worker 扫描源: 已 FINALIZED 且已解析
// 用户归属、尚未 CREDITED 的充值, 按发生顺序返回.
func (s *Store) ListFinalizedUncredited(ctx context.Context, chainID types.ChainID, limit int) ([]DepositRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+depCols+depJoin+`
		 WHERE d.chain_id=$1 AND d.status=$2 AND d.user_id IS NOT NULL
		 ORDER BY d.created_at, d.id LIMIT $3`,
		int64(chainID), DepositStatusFinalized, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DepositRecord{}
	for rows.Next() {
		d, err := scanDepositRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDepositsSweptByVault 归集批次确认后, 把该 vault 地址上全部 CREDITED
// 充值置为 SWEPT (链上资金已转冷金库). 返回受影响行数.
func (s *Store) MarkDepositsSweptByVault(ctx context.Context, chainID types.ChainID,
	depositAddressID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE onchain_deposits SET status=$3
		 WHERE status=$4 AND chain_id=$1
		   AND to_address = (SELECT address FROM user_deposit_addresses
		                      WHERE id=$2 AND chain_id=$1)`,
		int64(chainID), depositAddressID, DepositStatusSwept, DepositStatusCredited)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

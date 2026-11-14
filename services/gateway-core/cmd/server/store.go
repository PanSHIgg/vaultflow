// store.go — Wave-2 I1: api.ReadStore 的 pgx 生产实现.
//
// 读侧投影全部只读; 唯一写路径为 CreateWithdrawalAddress /
// MarkWithdrawalAddressRevoked (白名单投影, 由 handlers_whitelist 在 signer
// 注册成功后调用 — 按 I1 契约直接落 status='ACTIVE', 冷却/吊销权威留在
// signer 侧). NUMERIC(78,0) 一律 ::text 取回再走 types.MustParseU256.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/api"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// pgStore api.ReadStore 生产实现 (PostgreSQL 读侧投影).
type pgStore struct {
	pool *pgxpool.Pool
}

var _ api.ReadStore = (*pgStore)(nil)

func addrHex(a types.Address) string { return "0x" + hex.EncodeToString(a.Bytes()) }

func parseU256Text(s string) types.U256 { return types.MustParseU256(s) }

// ---------------------------------------------------------------------------
// 鉴权与商户
// ---------------------------------------------------------------------------

const merchantCols = `id, name, api_key_hash, COALESCE(webhook_url,''),
	COALESCE(webhook_secret_enc::text,''), fee_rate_bps, payout_frozen, created_at`

func scanMerchant(row pgx.Row) (types.Merchant, error) {
	var m types.Merchant
	var sec string
	err := row.Scan(&m.ID, &m.Name, &m.APIKeyHash, &m.WebhookURL, &sec,
		&m.FeeRateBps, &m.PayoutFrozen, &m.CreatedAt)
	m.WebhookSecretEnc = []byte(sec)
	return m, err
}

func (s *pgStore) ResolveAPIKey(ctx context.Context, apiKeyHash string) (types.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT `+merchantCols+` FROM merchants WHERE api_key_hash=$1`, apiKeyHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ports.ErrNotFound
	}
	return m, err
}

func (s *pgStore) GetMerchant(ctx context.Context, id uuid.UUID) (types.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT `+merchantCols+` FROM merchants WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ports.ErrNotFound
	}
	return m, err
}

func (s *pgStore) MerchantReceivableBalance(ctx context.Context, merchantID uuid.UUID) (types.U256, error) {
	var sum string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(balance),0)::text FROM ledger_accounts
		WHERE owner_merchant_id=$1 AND account_type=$2`,
		merchantID, types.AccountMerchantReorgReceivable.String()).Scan(&sum)
	if err != nil {
		return types.U256{}, err
	}
	return parseU256Text(sum), nil
}

// ---------------------------------------------------------------------------
// 订单 / 充值 / 资产
// ---------------------------------------------------------------------------

func scanDepositAddress(row pgx.Row) (types.DepositAddress, error) {
	var d types.DepositAddress
	var addr string
	var salt []byte
	var chain int64
	err := row.Scan(&d.ID, &chain, &addr, &salt, &d.IsDeployed, &d.IsQuarantined, &d.CreatedAt)
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

func (s *pgStore) GetDepositAddress(ctx context.Context, id uuid.UUID) (types.DepositAddress, error) {
	d, err := scanDepositAddress(s.pool.QueryRow(ctx,
		`SELECT id, chain_id, address, create2_salt, is_deployed, is_quarantined, created_at
		   FROM deposit_addresses WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ports.ErrNotFound
	}
	return d, err
}

func scanAsset(row pgx.Row) (types.SupportedAsset, error) {
	var a types.SupportedAsset
	var tc string
	var chain int64
	err := row.Scan(&chain, &a.AssetSymbol, &tc, &a.Decimals, &a.SupportsEIP3009)
	if err != nil {
		return a, err
	}
	a.ChainID = types.ChainID(chain)
	a.TokenContract = types.HexToAddress(tc)
	return a, nil
}

func (s *pgStore) GetSupportedAsset(ctx context.Context, chainID types.ChainID, symbol string) (types.SupportedAsset, error) {
	a, err := scanAsset(s.pool.QueryRow(ctx,
		`SELECT chain_id, asset_symbol, token_contract, decimals, supports_eip3009
		   FROM supported_assets WHERE chain_id=$1 AND asset_symbol=$2`,
		int64(chainID), symbol))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ports.ErrNotFound
	}
	return a, err
}

func (s *pgStore) ListSupportedAssets(ctx context.Context) ([]types.SupportedAsset, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT chain_id, asset_symbol, token_contract, decimals, supports_eip3009
		   FROM supported_assets ORDER BY chain_id, asset_symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.SupportedAsset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const orderCols = `id, merchant_id, idempotency_key, request_hash, chain_id, asset_symbol,
	expected_amount::text, received_amount::text, deposit_address_id, status, expires_at, settled_at`

func scanOrderRow(row pgx.Row) (types.PaymentOrder, error) {
	var o types.PaymentOrder
	var exp, recv, status string
	var chain int64
	err := row.Scan(&o.ID, &o.MerchantID, &o.IdempotencyKey, &o.RequestHash, &chain,
		&o.AssetSymbol, &exp, &recv, &o.DepositAddressID, &status, &o.ExpiresAt, &o.SettledAt)
	if err != nil {
		return o, err
	}
	o.ChainID = types.ChainID(chain)
	o.Status = types.OrderStatus(status)
	o.ExpectedAmount = parseU256Text(exp)
	o.ReceivedAmount = parseU256Text(recv)
	return o, nil
}

func (s *pgStore) ListOrders(ctx context.Context, q api.OrderListQuery) (api.OrderPage, error) {
	var sb strings.Builder
	args := []any{q.MerchantID}
	sb.WriteString(`SELECT ` + orderCols + ` FROM payment_orders WHERE merchant_id=$1`)
	n := 1
	if len(q.Statuses) > 0 {
		strs := make([]string, len(q.Statuses))
		for i, st := range q.Statuses {
			strs[i] = st.String()
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND status = ANY($%d)`, n))
		args = append(args, strs)
	}
	if q.ChainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND chain_id=$%d`, n))
		args = append(args, int64(*q.ChainID))
	}
	if q.AssetSymbol != "" {
		n++
		sb.WriteString(fmt.Sprintf(` AND asset_symbol=$%d`, n))
		args = append(args, q.AssetSymbol)
	}
	if q.After != nil && q.After.ID != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND id < $%d`, n))
		args = append(args, q.After.ID)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.OrderPage{}, err
	}
	defer rows.Close()
	page := api.OrderPage{Data: []types.PaymentOrder{}}
	for rows.Next() {
		o, err := scanOrderRow(rows)
		if err != nil {
			return page, err
		}
		page.Data = append(page.Data, o)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

func (s *pgStore) ListOrderDeposits(ctx context.Context, orderID uuid.UUID, limit int) ([]types.OnchainDeposit, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, chain_id, block_hash, tx_hash, log_index, block_number,
		       token_contract, from_address, to_address, amount::text,
		       deposit_category, status, matched_order_id
		  FROM onchain_deposits
		 WHERE matched_order_id=$1
		 ORDER BY block_number DESC, log_index DESC LIMIT $2`, orderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.OnchainDeposit{}
	for rows.Next() {
		d, err := scanDepositRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDepositRow(row pgx.Row) (types.OnchainDeposit, error) {
	var d types.OnchainDeposit
	var bh, th, tc, fa, ta, amt, cat, st string
	var chain int64
	err := row.Scan(&d.ID, &chain, &bh, &th, &d.LogIndex, &d.BlockNumber,
		&tc, &fa, &ta, &amt, &cat, &st, &d.MatchedOrderID)
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
	d.Category = types.DepositCategory(cat)
	d.Status = types.DepositStatus(st)
	return d, nil
}

// ---------------------------------------------------------------------------
// 提现 / 退款
// ---------------------------------------------------------------------------

const payoutCols = `id, merchant_id, idempotency_key, request_hash, payout_kind,
	source_account_type, source_order_id, chain_id, asset_symbol, to_address, amount::text,
	retry_seq, admin_refund_ticket, status, created_at`

func scanPayoutRow(row pgx.Row) (types.MerchantPayout, error) {
	var p types.MerchantPayout
	var amt, kind, source, status, toAddr string
	var chain int64
	err := row.Scan(&p.ID, &p.MerchantID, &p.IdempotencyKey, &p.RequestHash, &kind,
		&source, &p.SourceOrderID, &chain, &p.AssetSymbol, &toAddr, &amt,
		&p.RetrySeq, &p.AdminRefundTicket, &status, &p.CreatedAt)
	if err != nil {
		return p, err
	}
	p.ChainID = types.ChainID(chain)
	p.PayoutKind = types.PayoutKind(kind)
	p.SourceAccount = types.LedgerAccountCode(source)
	p.Status = types.PayoutStatus(status)
	p.ToAddress = types.HexToAddress(toAddr)
	p.Amount = parseU256Text(amt)
	return p, nil
}

func (s *pgStore) ListPayouts(ctx context.Context, q api.PayoutListQuery) (api.PayoutPage, error) {
	var sb strings.Builder
	args := []any{q.MerchantID}
	sb.WriteString(`SELECT ` + payoutCols + ` FROM merchant_payouts WHERE merchant_id=$1`)
	n := 1
	if len(q.Statuses) > 0 {
		strs := make([]string, len(q.Statuses))
		for i, st := range q.Statuses {
			strs[i] = st.String()
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND status = ANY($%d)`, n))
		args = append(args, strs)
	}
	if q.Kind != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND payout_kind=$%d`, n))
		args = append(args, q.Kind.String())
	}
	if q.SourceAccount != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND source_account_type=$%d`, n))
		args = append(args, q.SourceAccount.String())
	}
	if q.ChainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND chain_id=$%d`, n))
		args = append(args, int64(*q.ChainID))
	}
	if q.After != nil && q.After.ID != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, n, n+1))
		args = append(args, q.After.Sort, q.After.ID)
		n++
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.PayoutPage{}, err
	}
	defer rows.Close()
	page := api.PayoutPage{Data: []types.MerchantPayout{}}
	for rows.Next() {
		p, err := scanPayoutRow(rows)
		if err != nil {
			return page, err
		}
		page.Data = append(page.Data, p)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

func (s *pgStore) FindOrderRefundPayout(ctx context.Context, orderID uuid.UUID) (types.MerchantPayout, error) {
	p, err := scanPayoutRow(s.pool.QueryRow(ctx,
		`SELECT `+payoutCols+` FROM merchant_payouts
		  WHERE source_order_id=$1 AND payout_kind='CUSTOMER_REFUND'
		  ORDER BY created_at DESC LIMIT 1`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ports.ErrNotFound
	}
	return p, err
}

func (s *pgStore) FindIntentByReference(ctx context.Context, referenceID uuid.UUID) (types.OutboundTxIntent, error) {
	it, err := scanIntentRow(s.pool.QueryRow(ctx,
		`SELECT `+intentCols+` FROM outbound_tx_intents
		  WHERE reference_id=$1 ORDER BY created_at DESC LIMIT 1`, referenceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return it, ports.ErrNotFound
	}
	return it, err
}

const intentCols = `id, intent_type, reference_id, chain_id, wallet_address, nonce, status, created_at`

func scanIntentRow(row pgx.Row) (types.OutboundTxIntent, error) {
	var it types.OutboundTxIntent
	var itype, wallet, status string
	var chain int64
	err := row.Scan(&it.ID, &itype, &it.ReferenceID, &chain, &wallet, &it.Nonce, &status, &it.CreatedAt)
	if err != nil {
		return it, err
	}
	it.IntentType = types.IntentType(itype)
	it.ChainID = types.ChainID(chain)
	it.WalletAddress = types.HexToAddress(wallet)
	it.Status = types.TxIntentStatus(status)
	return it, nil
}

func (s *pgStore) LatestFinalizedAttempt(ctx context.Context, intentID uuid.UUID) (types.OutboundTxAttempt, error) {
	a, err := scanAttemptRow(s.pool.QueryRow(ctx,
		`SELECT `+attemptCols+` FROM outbound_tx_attempts
		  WHERE intent_id=$1 AND status='FINALIZED'
		  ORDER BY attempt_no DESC LIMIT 1`, intentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ports.ErrNotFound
	}
	return a, err
}

const attemptCols = `id, intent_id, attempt_no, attempt_kind, chain_id, tx_hash, block_number,
	block_hash, receipt_status, max_fee_per_gas::text, max_priority_fee_per_gas::text,
	gas_used, effective_gas_price::text, status, broadcasted_at`

func scanAttemptRow(row pgx.Row) (types.OutboundTxAttempt, error) {
	var a types.OutboundTxAttempt
	var kind, th, mf, mp, egp, st string
	var bhn *string
	var chain int64
	err := row.Scan(&a.ID, &a.IntentID, &a.AttemptNo, &kind, &chain, &th,
		&a.BlockNumber, &bhn, &a.ReceiptStatus, &mf, &mp, &a.GasUsed, &egp,
		&st, &a.BroadcastedAt)
	if err != nil {
		return a, err
	}
	a.Kind = types.AttemptKind(kind)
	a.ChainID = types.ChainID(chain)
	a.TxHash = types.HexToHash(th)
	if bhn != nil && *bhn != "" {
		h := types.HexToHash(*bhn)
		a.BlockHash = &h
	}
	a.MaxFeePerGas = parseU256Text(mf)
	a.MaxPriorityFeePerGas = parseU256Text(mp)
	if egp != "" {
		v := parseU256Text(egp)
		a.EffectiveGasPrice = &v
	}
	a.Status = types.TxAttemptStatus(st)
	return a, nil
}

// ---------------------------------------------------------------------------
// 提现白名单 (唯一写投影 — 直接 ACTIVE, 冷却权威在 signer)
// ---------------------------------------------------------------------------

const mwaCols = `id, merchant_id, chain_id, address, admin_approval_sig, valid_until, status`

func scanMWA(row pgx.Row) (types.MerchantWithdrawalAddress, error) {
	var m types.MerchantWithdrawalAddress
	var addr, st string
	var chain int64
	err := row.Scan(&m.ID, &m.MerchantID, &chain, &addr, &m.AdminApprovalSig, &m.ValidUntil, &st)
	if err != nil {
		return m, err
	}
	m.ChainID = types.ChainID(chain)
	m.Address = types.HexToAddress(addr)
	m.Status = types.WithdrawalAddressStatus(st)
	return m, nil
}

// CreateWithdrawalAddress I1 契约: signer 侧 RegisterWhitelist 先行成功后,
// 网关投影直接落 status='ACTIVE' (签名人持有冷却/吊销权威, 网关仅作读投影).
// (merchant,chain,address) 唯一键冲突时按重注册语义刷新租期与签名.
func (s *pgStore) CreateWithdrawalAddress(ctx context.Context, rec api.NewWithdrawalAddress) (types.MerchantWithdrawalAddress, error) {
	m, err := scanMWA(s.pool.QueryRow(ctx, `
		INSERT INTO merchant_withdrawal_addresses
		  (merchant_id, chain_id, address, admin_approval_sig, valid_until, status)
		VALUES ($1,$2,$3,$4,$5,'ACTIVE')
		ON CONFLICT (merchant_id, chain_id, address) DO UPDATE
		  SET admin_approval_sig=EXCLUDED.admin_approval_sig,
		      valid_until=EXCLUDED.valid_until,
		      status='ACTIVE'
		RETURNING `+mwaCols,
		rec.MerchantID, int64(rec.ChainID), addrHex(rec.Address), rec.AdminSig, rec.ValidUntil))
	return m, err
}

func (s *pgStore) GetWithdrawalAddress(ctx context.Context, id uuid.UUID) (types.MerchantWithdrawalAddress, error) {
	m, err := scanMWA(s.pool.QueryRow(ctx,
		`SELECT `+mwaCols+` FROM merchant_withdrawal_addresses WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ports.ErrNotFound
	}
	return m, err
}

func (s *pgStore) ListWithdrawalAddresses(ctx context.Context, q api.WhitelistQuery) (api.WhitelistPage, error) {
	var sb strings.Builder
	args := []any{q.MerchantID}
	sb.WriteString(`SELECT ` + mwaCols + ` FROM merchant_withdrawal_addresses WHERE merchant_id=$1`)
	n := 1
	if len(q.Statuses) > 0 {
		strs := make([]string, len(q.Statuses))
		for i, st := range q.Statuses {
			strs[i] = st.String()
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND status = ANY($%d)`, n))
		args = append(args, strs)
	}
	if q.ChainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND chain_id=$%d`, n))
		args = append(args, int64(*q.ChainID))
	}
	if q.After != nil && *q.After != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND id > $%d`, n))
		args = append(args, *q.After)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY id LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.WhitelistPage{}, err
	}
	defer rows.Close()
	page := api.WhitelistPage{Data: []types.MerchantWithdrawalAddress{}}
	for rows.Next() {
		m, err := scanMWA(rows)
		if err != nil {
			return page, err
		}
		page.Data = append(page.Data, m)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

func (s *pgStore) MarkWithdrawalAddressRevoked(ctx context.Context, id uuid.UUID) (types.MerchantWithdrawalAddress, error) {
	m, err := scanMWA(s.pool.QueryRow(ctx,
		`UPDATE merchant_withdrawal_addresses SET status='REVOKED'
		  WHERE id=$1 RETURNING `+mwaCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ports.ErrNotFound
	}
	return m, err
}

// ---------------------------------------------------------------------------
// 账本
// ---------------------------------------------------------------------------

func (s *pgStore) ListLedgerAccounts(ctx context.Context, q api.LedgerAccountQuery) ([]types.LedgerAccount, error) {
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT id, owner_merchant_id, chain_id, asset_symbol, account_type,
		normal_side, balance::text FROM ledger_accounts WHERE TRUE`)
	n := 0
	if len(q.Owners) > 0 {
		n++
		sb.WriteString(fmt.Sprintf(` AND owner_merchant_id = ANY($%d)`, n))
		args = append(args, q.Owners)
	}
	if q.ChainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND chain_id=$%d`, n))
		args = append(args, int64(*q.ChainID))
	}
	if q.AssetSymbol != "" {
		n++
		sb.WriteString(fmt.Sprintf(` AND asset_symbol=$%d`, n))
		args = append(args, q.AssetSymbol)
	}
	if len(q.Accounts) > 0 {
		strs := make([]string, len(q.Accounts))
		for i, a := range q.Accounts {
			strs[i] = a.String()
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND account_type = ANY($%d)`, n))
		args = append(args, strs)
	}
	sb.WriteString(` ORDER BY owner_merchant_id, chain_id, asset_symbol, account_type`)
	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.LedgerAccount{}
	for rows.Next() {
		var a types.LedgerAccount
		var at, side, bal string
		var chain int64
		if err := rows.Scan(&a.ID, &a.OwnerMerchantID, &chain, &a.AssetSymbol, &at, &side, &bal); err != nil {
			return nil, err
		}
		a.ChainID = types.ChainID(chain)
		a.Account = types.LedgerAccountCode(at)
		a.NormalSide = types.Direction(side)
		a.Balance = parseU256Text(bal)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *pgStore) ListLedgerTransactions(ctx context.Context, q api.LedgerTxQuery) (api.LedgerTxPage, error) {
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT id, reference_type, reference_id, COALESCE(description,''), created_at
		FROM ledger_transactions WHERE TRUE`)
	n := 0
	if len(q.RefTypes) > 0 {
		strs := make([]string, len(q.RefTypes))
		for i, t := range q.RefTypes {
			strs[i] = t.String()
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND reference_type = ANY($%d)`, n))
		args = append(args, strs)
	}
	if q.RefID != "" {
		n++
		sb.WriteString(fmt.Sprintf(` AND reference_id=$%d`, n))
		args = append(args, q.RefID)
	}
	if q.After != nil && q.After.ID != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, n, n+1))
		args = append(args, q.After.Sort, q.After.ID)
		n++
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.LedgerTxPage{}, err
	}
	defer rows.Close()
	page := api.LedgerTxPage{Data: []types.LedgerTransaction{}}
	for rows.Next() {
		var t types.LedgerTransaction
		var rt string
		if err := rows.Scan(&t.ID, &rt, &t.ReferenceID, &t.Description, &t.CreatedAt); err != nil {
			return page, err
		}
		t.ReferenceType = types.EntryTemplateCode(rt)
		page.Data = append(page.Data, t)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

func (s *pgStore) ListLedgerEntries(ctx context.Context, q api.LedgerEntryQuery) (api.LedgerEntryPage, error) {
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT e.id, e.transaction_id, e.account_id, e.direction,
		e.amount::text, e.balance_after::text, e.created_at, a.account_type, a.owner_merchant_id
		FROM ledger_entries e JOIN ledger_accounts a ON a.id=e.account_id WHERE TRUE`)
	n := 0
	if q.TransactionID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND e.transaction_id=$%d`, n))
		args = append(args, *q.TransactionID)
	}
	if q.AccountID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND e.account_id=$%d`, n))
		args = append(args, *q.AccountID)
	}
	if q.Direction != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND e.direction=$%d`, n))
		args = append(args, q.Direction.String())
	}
	if q.After != nil && q.After.ID != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND (e.created_at, e.id) < ($%d, $%d)`, n, n+1))
		args = append(args, q.After.Sort, q.After.ID)
		n++
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY e.created_at DESC, e.id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.LedgerEntryPage{}, err
	}
	defer rows.Close()
	page := api.LedgerEntryPage{Data: []api.LedgerEntryView{}}
	for rows.Next() {
		var v api.LedgerEntryView
		var dir, amt, bal, at string
		err := rows.Scan(&v.Entry.ID, &v.Entry.TransactionID, &v.Entry.AccountID, &dir,
			&amt, &bal, &v.Entry.CreatedAt, &at, &v.OwnerMerchantID)
		if err != nil {
			return page, err
		}
		v.Entry.Direction = types.Direction(dir)
		v.Entry.Amount = parseU256Text(amt)
		v.Entry.BalanceAfter = parseU256Text(bal)
		v.AccountCode = types.LedgerAccountCode(at)
		page.Data = append(page.Data, v)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

// ---------------------------------------------------------------------------
// 运维
// ---------------------------------------------------------------------------

func (s *pgStore) GetChainSyncCursor(ctx context.Context, chainID types.ChainID) (types.ChainSyncCursor, error) {
	var c types.ChainSyncCursor
	var hh string
	var chain int64
	err := s.pool.QueryRow(ctx, `
		SELECT chain_id, latest_synced_height, latest_synced_hash, safe_head_height, updated_at
		  FROM chain_sync_cursors WHERE chain_id=$1`, int64(chainID)).
		Scan(&chain, &c.LatestSyncedHeight, &hh, &c.SafeHeadHeight, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ports.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.ChainID = types.ChainID(chain)
	if hh != "" {
		c.LatestSyncedHash = types.HexToHash(hh)
	}
	return c, nil
}

func (s *pgStore) ListNonceCursors(ctx context.Context, chainID *types.ChainID) ([]api.NonceCursorView, error) {
	q := `SELECT c.chain_id, c.wallet_address, c.next_nonce, c.updated_at,
		EXISTS(SELECT 1 FROM outbound_tx_intents i
			WHERE i.chain_id=c.chain_id AND i.wallet_address=c.wallet_address
			  AND i.status='NONCE_STOLEN_HALT')
		FROM wallet_nonce_cursors c`
	args := []any{}
	if chainID != nil {
		q += ` WHERE c.chain_id=$1`
		args = append(args, int64(*chainID))
	}
	q += ` ORDER BY c.chain_id, c.wallet_address`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.NonceCursorView{}
	for rows.Next() {
		var v api.NonceCursorView
		var w string
		var chain int64
		if err := rows.Scan(&chain, &w, &v.Cursor.NextNonce, &v.Cursor.UpdatedAt, &v.Halted); err != nil {
			return nil, err
		}
		v.Cursor.ChainID = types.ChainID(chain)
		v.Cursor.WalletAddress = types.HexToAddress(w)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *pgStore) ListOutboundIntents(ctx context.Context, q api.OutboundListQuery) (api.OutboundPage, error) {
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT ` + intentCols + ` FROM outbound_tx_intents WHERE TRUE`)
	n := 0
	if len(q.Statuses) > 0 {
		strs := make([]string, len(q.Statuses))
		for i, st := range q.Statuses {
			strs[i] = st.String()
		}
		n++
		sb.WriteString(fmt.Sprintf(` AND status = ANY($%d)`, n))
		args = append(args, strs)
	}
	if q.IntentType != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND intent_type=$%d`, n))
		args = append(args, q.IntentType.String())
	}
	if q.ChainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND chain_id=$%d`, n))
		args = append(args, int64(*q.ChainID))
	}
	if q.Wallet != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND wallet_address=$%d`, n))
		args = append(args, addrHex(*q.Wallet))
	}
	if q.After != nil && q.After.ID != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, n, n+1))
		args = append(args, q.After.Sort, q.After.ID)
		n++
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.OutboundPage{}, err
	}
	defer rows.Close()
	page := api.OutboundPage{Data: []types.OutboundTxIntent{}}
	for rows.Next() {
		it, err := scanIntentRow(rows)
		if err != nil {
			return page, err
		}
		page.Data = append(page.Data, it)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

func (s *pgStore) ListQuarantinedVaults(ctx context.Context, q api.QuarantineQuery) (api.QuarantinePage, error) {
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT da.id, da.chain_id, da.address, da.is_quarantined,
		COALESCE(po.asset_symbol,''), i.id, i.batch_id, i.item_status, po.id, da.created_at
		FROM deposit_addresses da
		LEFT JOIN payment_orders po ON po.deposit_address_id = da.id
		LEFT JOIN LATERAL (
			SELECT id, batch_id, item_status FROM sweep_batch_items sbi
			WHERE sbi.deposit_address_id = da.id
			ORDER BY sbi.id DESC LIMIT 1
		) i ON TRUE
		WHERE da.is_quarantined = TRUE`)
	n := 0
	if q.ChainID != nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND da.chain_id=$%d`, n))
		args = append(args, int64(*q.ChainID))
	}
	if q.After != nil && q.After.ID != uuid.Nil {
		n++
		sb.WriteString(fmt.Sprintf(` AND (da.created_at, da.id) < ($%d, $%d)`, n, n+1))
		args = append(args, q.After.Sort, q.After.ID)
		n++
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	n++
	sb.WriteString(fmt.Sprintf(` ORDER BY da.created_at DESC, da.id DESC LIMIT $%d`, n))
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return api.QuarantinePage{}, err
	}
	defer rows.Close()
	page := api.QuarantinePage{Data: []api.QuarantinedVaultView{}}
	for rows.Next() {
		var v api.QuarantinedVaultView
		var addr, itemStatus *string
		var chain int64
		var createdAt time.Time
		err := rows.Scan(&v.DepositAddressID, &chain, &addr, &v.IsQuarantined,
			&v.AssetSymbol, &v.SweepBatchItemID, &v.SweepBatchID, &itemStatus, &v.RelatedOrderID, &createdAt)
		if err != nil {
			return page, err
		}
		v.ChainID = types.ChainID(chain)
		if addr != nil {
			v.Address = types.HexToAddress(*addr)
		}
		if itemStatus != nil {
			v.ItemStatus = *itemStatus
		}
		// DDL 无 reason/quarantined_at 列 — 契约缺陷已上报; 尽力以 created_at 近似.
		t := createdAt.UTC()
		v.QuarantinedAt = &t
		v.Reason = "QUARANTINED_REVERT"
		page.Data = append(page.Data, v)
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	return page, rows.Err()
}

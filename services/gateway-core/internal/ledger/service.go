// Package ledger 实现 VaultFlow NUMERIC(78,0) 全值域整数复式记账引擎 (W3-2).
//
// 核心不变量 (AGENTS.md §2 / ARCHITECTURE.md §6 / migrations/001_init.sql P0-3):
//   - 金额全程 types.U256 (*big.Int) 对应 NUMERIC(78,0), 零浮点;
//     手续费 fee = amount * fee_rate_bps / 10000 向下取整 (types.U256.MulBpsFloor).
//   - 每笔记账事务 SUM(DEBIT) == SUM(CREDIT) > 0; 全账户 balance >= 0.
//   - 分录腿方向相对科目 normal_side 入账: direction == normal_side 则
//     balance += amount, 否则 balance -= amount (P0-3 触发器链式校验兼容:
//     同账户同事务仅允许一条合并分录, 见 consolidateLegs).
//   - 防死锁行锁协议: 先按需幂等建户, 再按 account_id ASC 单条
//     SELECT ... FOR UPDATE 加锁, 校验后写 ledger_transactions +
//     带 balance_after 快照的 ledger_entries (+ webhook_outbox).
//   - 幂等: (reference_type, reference_id) 已存在且分录一致 → AlreadyExisted;
//     不一致 → ports.ErrDuplicateReference.
//   - 模板 9 REORG_REVERSAL 深回滚透支 (FIX-A/A2 泛化): 商户自有负债科目
//     (MERCHANT_AVAILABLE / ORDER_ESCROW_LIAB / PAYOUT_LOCKED_LIAB /
//     SUSPENSE_*_LIAB — 判据: owner 非系统哨兵且 normal_side=CREDIT)
//     借方不足抵扣时, 差额在账户行锁内自动钳制并记入同范围
//     MERCHANT_REORG_RECEIVABLE, 同事务置 merchants.payout_frozen = true;
//     平台托管/权益科目 (CUSTODY_* / FEE_REVENUE / TREASURY_EQUITY_CAPITAL)
//     一律不拆分 — 余额不足即 ErrNegativeBalance 拒账.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// nativeAssetSymbol HOT_WALLET_NATIVE 核算所用的资产维度 (§6.1:
// 原生 Gas 币按 chain 维度的 'ETH' 科目核算, 与稳定币科目正交).
const nativeAssetSymbol = "ETH"

// Service 复式记账引擎. 实现 ports.LedgerService.
type Service struct {
	pool *pgxpool.Pool
}

// NewService 构造记账引擎.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// compile-time 契约锚点.
var _ ports.LedgerService = (*Service)(nil)

// ---------------------------------------------------------------------------
// NUMERIC(78,0) <-> types.U256 编解码 (pgtype.Numeric, 零浮点)
// ---------------------------------------------------------------------------

func u256ToNumeric(u types.U256) pgtype.Numeric {
	return pgtype.Numeric{Int: u.Big(), Valid: true}
}

func numericToU256(n pgtype.Numeric) (types.U256, error) {
	if !n.Valid {
		return types.U256{}, errors.New("ledger: NULL numeric")
	}
	v := new(big.Int).Set(n.Int)
	switch {
	case n.Exp > 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil))
	case n.Exp < 0:
		d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n.Exp)), nil)
		q, r := new(big.Int).QuoRem(v, d, new(big.Int))
		if r.Sign() != 0 {
			return types.U256{}, fmt.Errorf("ledger: numeric %s has fractional part", n.Int.String())
		}
		v = q
	}
	if v.Sign() < 0 {
		return types.U256{}, fmt.Errorf("ledger: negative numeric %s", v.String())
	}
	return types.U256FromBig(v), nil
}

func scanU256(v pgtype.Numeric) (types.U256, error) { return numericToU256(v) }

// ---------------------------------------------------------------------------
// 分录腿规范化: 合并 / 对消 / 白名单校验 / 试算平衡
// ---------------------------------------------------------------------------

// accountScope 账户四元组 (ledger_accounts 联合唯一键).
type accountScope struct {
	Owner   uuid.UUID
	Chain   types.ChainID
	Asset   string
	Account types.LedgerAccountCode
}

// leg 规范化后的分录腿 (同账户同方向已合并, 同账户双向已对消).
type leg struct {
	Scope     accountScope
	Direction types.Direction
	Amount    *big.Int
}

// legKey 幂等比较规范键.
type legKey struct {
	Scope     accountScope
	Direction types.Direction
	Amount    string
}

// consolidateLegs 规范化调用方分录腿:
//  1. 逐腿校验科目码 / 方向 / amount > 0;
//  2. 同 (scope, direction) 合并求和; 同 scope 双向对消取净额
//     (保证每账户每笔事务至多一条分录 — P0-3 触发器对同账户多腿的
//     balance_after 链存在歧义, 合并后天然兼容);
//  3. 对消后清零的腿丢弃; 全空事务为非法.
//
// 合并/对消不改变 SUM(DEBIT)/SUM(CREDIT) 差值, 试算平衡语义保持.
func consolidateLegs(legs []ports.PostEntryLeg) ([]leg, error) {
	if len(legs) == 0 {
		return nil, fmt.Errorf("%w: posting must contain at least one leg", ports.ErrUnbalancedEntry)
	}
	// 按 scope+方向聚合 (保持首见顺序).
	type aggKey struct {
		Scope accountScope
		Dir   types.Direction
	}
	order := []aggKey{}
	agg := map[aggKey]*big.Int{}
	for i, l := range legs {
		if !l.Account.IsValid() {
			return nil, fmt.Errorf("%w: %q", ports.ErrInvalidAccountCode, l.Account)
		}
		if l.Direction != types.DirectionDebit && l.Direction != types.DirectionCredit {
			return nil, fmt.Errorf("%w: invalid direction %q", ports.ErrInvalidTransition, l.Direction)
		}
		if l.Amount.Sign() <= 0 {
			return nil, fmt.Errorf("%w: leg %d amount %s", ports.ErrNonPositiveAmount, i, l.Amount.String())
		}
		k := aggKey{
			Scope: accountScope{Owner: l.OwnerMerchantID, Chain: l.ChainID, Asset: l.AssetSymbol, Account: l.Account},
			Dir:   l.Direction,
		}
		if _, ok := agg[k]; !ok {
			agg[k] = new(big.Int)
			order = append(order, k)
		}
		agg[k].Add(agg[k], l.Amount.Big())
	}
	// 同 scope 双向对消.
	byScope := map[accountScope]map[types.Direction]*big.Int{}
	scopeOrder := []accountScope{}
	for _, k := range order {
		m, ok := byScope[k.Scope]
		if !ok {
			m = map[types.Direction]*big.Int{}
			byScope[k.Scope] = m
			scopeOrder = append(scopeOrder, k.Scope)
		}
		m[k.Dir] = agg[k]
	}
	out := []leg{}
	for _, s := range scopeOrder {
		m := byScope[s]
		d, c := new(big.Int), new(big.Int)
		if v := m[types.DirectionDebit]; v != nil {
			d.Set(v)
		}
		if v := m[types.DirectionCredit]; v != nil {
			c.Set(v)
		}
		net := new(big.Int).Sub(d, c)
		switch net.Sign() {
		case 1:
			out = append(out, leg{Scope: s, Direction: types.DirectionDebit, Amount: net})
		case -1:
			out = append(out, leg{Scope: s, Direction: types.DirectionCredit, Amount: net.Neg(net)})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: all legs net to zero", ports.ErrUnbalancedEntry)
	}
	return out, nil
}

// validateLegs 校验规范化分录腿: 逐 (借 x 贷) 组合必须命中 §6.2 模板白名单,
// 且 SUM(DEBIT) == SUM(CREDIT) > 0.
func validateLegs(template types.EntryTemplateCode, legs []leg) error {
	if !template.IsValid() {
		return fmt.Errorf("%w: invalid template %q", ports.ErrInvalidTemplateLeg, template)
	}
	var drSum, crSum = new(big.Int), new(big.Int)
	for _, l := range legs {
		if l.Direction == types.DirectionDebit {
			drSum.Add(drSum, l.Amount)
		} else {
			crSum.Add(crSum, l.Amount)
		}
	}
	if drSum.Cmp(crSum) != 0 {
		return fmt.Errorf("%w: SUM(DEBIT)=%s != SUM(CREDIT)=%s",
			ports.ErrUnbalancedEntry, drSum.String(), crSum.String())
	}
	if drSum.Sign() <= 0 {
		return fmt.Errorf("%w: empty posting", ports.ErrUnbalancedEntry)
	}
	for _, d := range legs {
		if d.Direction != types.DirectionDebit {
			continue
		}
		for _, c := range legs {
			if c.Direction != types.DirectionCredit {
				continue
			}
			if !template.Allows(d.Scope.Account, c.Scope.Account) {
				return fmt.Errorf("%w: %s does not allow %s -> %s",
					ports.ErrInvalidTemplateLeg, template, d.Scope.Account, c.Scope.Account)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// PostEntry — ports.LedgerService 主入口 (自带 PG 事务)
// ---------------------------------------------------------------------------

// PostEntry 模板化复式记账 (幂等). 见 ports.LedgerService 契约.
func (s *Service) PostEntry(ctx context.Context, req ports.PostEntryRequest) (ports.PostedEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ports.PostedEntry{}, fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	posted, err := s.PostEntryInTx(ctx, tx, req)
	if err != nil {
		return ports.PostedEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ports.PostedEntry{}, fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	return posted, nil
}

// PostEntryInTx 在调用方持有的 PG 事务内执行同一记账流程 (供 order 等编排包
// 把状态跃迁与分录写入放进单一 ACID 边界). 事务由调用方提交/回滚.
func (s *Service) PostEntryInTx(ctx context.Context, tx pgx.Tx, req ports.PostEntryRequest) (ports.PostedEntry, error) {
	legs, err := consolidateLegs(req.Legs)
	if err != nil {
		return ports.PostedEntry{}, err
	}
	if err := validateLegs(req.Template, legs); err != nil {
		return ports.PostedEntry{}, err
	}

	// (1) 幂等键占位: 已存在同 (reference_type, reference_id) 时按
	//     分录一致性返回 AlreadyExisted 或 ErrDuplicateReference.
	txn, claimed, err := s.claimReference(ctx, tx, req)
	if err != nil {
		return ports.PostedEntry{}, err
	}
	if !claimed {
		return s.reconcileExisting(ctx, tx, txn, legs)
	}

	// (2) 按需幂等建户 (模板 9 透支场景可能动态追加 MERCHANT_REORG_RECEIVABLE
	//     腿, 故先收集全部 scope 再统一建户).
	scopes := map[accountScope]struct{}{}
	for _, l := range legs {
		scopes[l.Scope] = struct{}{}
	}
	if req.Template == types.TemplateReorgReversal {
		for _, l := range legs {
			if l.Direction == types.DirectionDebit && isMerchantLiabilityScope(l.Scope) {
				scopes[accountScope{Owner: l.Scope.Owner, Chain: l.Scope.Chain, Asset: l.Scope.Asset,
					Account: types.AccountMerchantReorgReceivable}] = struct{}{}
			}
		}
	}
	accountIDs := map[accountScope]uuid.UUID{}
	for sc := range scopes {
		id, err := s.ensureAccount(ctx, tx, sc)
		if err != nil {
			return ports.PostedEntry{}, err
		}
		accountIDs[sc] = id
	}

	// (2b) FIX-E/E3 全局锁序: merchants → ledger_accounts. 模板 9 可能在
	//     (4)/(5) 透支拆分后回写 merchants.payout_frozen (freezeMerchants);
	//     而订单提现路径 (order.createPayoutLocked) 先持 merchants 行锁再
	//     进账本锁账户 — 若此处先锁账户再锁商户, 两条跨表取锁序列相交即
	//     40P01. 故对模板 9 先把"可能被冻结的商户" (商户自有负债借方腿的
	//     owner) 去重排序后 FOR UPDATE 预锁, 与提现路径构成单向全序.
	if req.Template == types.TemplateReorgReversal {
		if err := s.prelockFreezeCandidates(ctx, tx, legs); err != nil {
			return ports.PostedEntry{}, err
		}
	}

	// (3) 防死锁行锁: 按 account_id ASC 单条 SELECT ... FOR UPDATE.
	ids := make([]uuid.UUID, 0, len(accountIDs))
	idToScope := map[uuid.UUID]accountScope{}
	for sc, id := range accountIDs {
		ids = append(ids, id)
		idToScope[id] = sc
	}
	sort.Slice(ids, func(i, j int) bool {
		return string(ids[i][:]) < string(ids[j][:])
	})
	balances, err := s.lockAccounts(ctx, tx, ids)
	if err != nil {
		return ports.PostedEntry{}, err
	}

	// (4) 应用分录腿 (含模板 9 透支自动拆分与冻结).
	applied, frozenMerchants, err := s.applyLegs(req.Template, legs, accountIDs, idToScope, balances)
	if err != nil {
		return ports.PostedEntry{}, err
	}

	// (5) 写余额更新 + 分录 (balance_after 快照) + 透支冻结 + outbox.
	entries, err := s.writePosting(ctx, tx, txn.ID, applied, balances)
	if err != nil {
		return ports.PostedEntry{}, err
	}
	if err := s.freezeMerchants(ctx, tx, frozenMerchants); err != nil {
		return ports.PostedEntry{}, err
	}
	if err := s.writeOutbox(ctx, tx, txn, req, applied); err != nil {
		return ports.PostedEntry{}, err
	}

	txn.ReferenceType = req.Template
	txn.ReferenceID = req.ReferenceID
	txn.Description = req.Description
	return ports.PostedEntry{Transaction: txn, Entries: entries, AlreadyExisted: false}, nil
}

// ---------------------------------------------------------------------------
// 内部步骤
// ---------------------------------------------------------------------------

// claimReference 抢占 (reference_type, reference_id) 幂等键.
// 返回 (txn, true) 表示本次获得插入权; (txn, false) 表示已存在同事务.
func (s *Service) claimReference(ctx context.Context, tx pgx.Tx, req ports.PostEntryRequest) (types.LedgerTransaction, bool, error) {
	var txn types.LedgerTransaction
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (reference_type, reference_id, description)
		VALUES ($1, $2, $3)
		ON CONFLICT (reference_type, reference_id) DO NOTHING
		RETURNING id, created_at`,
		req.Template.String(), req.ReferenceID, req.Description).
		Scan(&txn.ID, &txn.CreatedAt)
	if err == nil {
		return txn, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return txn, false, fmt.Errorf("ledger: claim reference: %w", err)
	}
	// 已存在: 读回 (并发事务提交后在 READ COMMITTED 下可见).
	err = tx.QueryRow(ctx, `
		SELECT id, description, created_at FROM ledger_transactions
		WHERE reference_type = $1 AND reference_id = $2`,
		req.Template.String(), req.ReferenceID).
		Scan(&txn.ID, &txn.Description, &txn.CreatedAt)
	if err != nil {
		return txn, false, fmt.Errorf("ledger: read existing reference: %w", err)
	}
	txn.ReferenceType = req.Template
	txn.ReferenceID = req.ReferenceID
	return txn, false, nil
}

// reconcileExisting 幂等命中: 已存分录与本次规范化腿完全一致 →
// AlreadyExisted; 不一致 → ErrDuplicateReference.
func (s *Service) reconcileExisting(ctx context.Context, tx pgx.Tx, txn types.LedgerTransaction, legs []leg) (ports.PostedEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.id, e.account_id, e.direction, e.amount, e.balance_after, e.created_at,
		       a.owner_merchant_id, a.chain_id, a.asset_symbol, a.account_type
		  FROM ledger_entries e
		  JOIN ledger_accounts a ON a.id = e.account_id
		 WHERE e.transaction_id = $1
		 ORDER BY e.id`, txn.ID)
	if err != nil {
		return ports.PostedEntry{}, fmt.Errorf("ledger: load existing entries: %w", err)
	}
	defer rows.Close()

	stored := map[legKey]int{}
	var entries []types.LedgerEntry
	for rows.Next() {
		var e types.LedgerEntry
		var amount, balanceAfter pgtype.Numeric
		var sc accountScope
		var dir string
		if err := rows.Scan(&e.ID, &e.AccountID, &dir, &amount, &balanceAfter, &e.CreatedAt,
			&sc.Owner, &sc.Chain, &sc.Asset, &sc.Account); err != nil {
			return ports.PostedEntry{}, fmt.Errorf("ledger: scan entry: %w", err)
		}
		e.TransactionID = txn.ID
		e.Direction = types.Direction(dir)
		e.Amount, err = scanU256(amount)
		if err != nil {
			return ports.PostedEntry{}, err
		}
		e.BalanceAfter, err = scanU256(balanceAfter)
		if err != nil {
			return ports.PostedEntry{}, err
		}
		entries = append(entries, e)
		stored[legKey{Scope: sc, Direction: e.Direction, Amount: e.Amount.String()}]++
	}
	if err := rows.Err(); err != nil {
		return ports.PostedEntry{}, err
	}

	want := map[legKey]int{}
	for _, l := range legs {
		want[legKey{Scope: l.Scope, Direction: l.Direction, Amount: l.Amount.String()}]++
	}
	if !sameMultiset(stored, want) {
		return ports.PostedEntry{}, fmt.Errorf("%w: template=%s ref=%s",
			ports.ErrDuplicateReference, txn.ReferenceType, txn.ReferenceID)
	}
	return ports.PostedEntry{Transaction: txn, Entries: entries, AlreadyExisted: true}, nil
}

func sameMultiset(a, b map[legKey]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	return true
}

// ensureAccount 幂等建户 (owner+chain+asset+account 联合唯一键).
func (s *Service) ensureAccount(ctx context.Context, tx pgx.Tx, sc accountScope) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_merchant_id, chain_id, asset_symbol, account_type) DO NOTHING
		RETURNING id`,
		sc.Owner, int64(sc.Chain), sc.Asset, sc.Account.String(), sc.Account.NormalSide().String()).
		Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, fmt.Errorf("ledger: ensure account %s: %w", sc.Account, err)
	}
	err = tx.QueryRow(ctx, `
		SELECT id FROM ledger_accounts
		WHERE owner_merchant_id = $1 AND chain_id = $2 AND asset_symbol = $3 AND account_type = $4`,
		sc.Owner, int64(sc.Chain), sc.Asset, sc.Account.String()).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("ledger: find account %s: %w", sc.Account, err)
	}
	return id, nil
}

// prelockFreezeCandidates E3 — 模板 9 (REORG_REVERSAL) 商户行预锁:
// 收集商户自有负债借方腿的 owner (即可能触发 payout_frozen 回写的商户),
// dedup + id ASC 后在账户行锁之前取 merchants FOR UPDATE. 与
// createPayoutLocked (merchants → 账户) 构成全序单向依赖, 消除
// 「提现冻结 × 深回滚红冲」跨事务 40P01 环.
func (s *Service) prelockFreezeCandidates(ctx context.Context, tx pgx.Tx, legs []leg) error {
	set := map[uuid.UUID]struct{}{}
	for _, l := range legs {
		if l.Direction == types.DirectionDebit && isMerchantLiabilityScope(l.Scope) {
			set[l.Scope.Owner] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return string(ids[i][:]) < string(ids[j][:]) })
	rows, err := tx.Query(ctx,
		`SELECT id FROM merchants WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return fmt.Errorf("ledger: prelock freeze-candidate merchants: %w", err)
	}
	rows.Close()
	return rows.Err()
}

// lockAccounts 按 id ASC 单条 SELECT ... FOR UPDATE 加锁并读余额.
func (s *Service) lockAccounts(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]*big.Int, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*big.Int{}, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id, balance FROM ledger_accounts
		WHERE id = ANY($1::uuid[])
		ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	defer rows.Close()
	balances := map[uuid.UUID]*big.Int{}
	for rows.Next() {
		var id uuid.UUID
		var bal pgtype.Numeric
		if err := rows.Scan(&id, &bal); err != nil {
			return nil, fmt.Errorf("ledger: scan balance: %w", err)
		}
		u, err := numericToU256(bal)
		if err != nil {
			return nil, err
		}
		balances[id] = u.Big()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := balances[id]; !ok {
			return nil, fmt.Errorf("ledger: locked account %s missing", id)
		}
	}
	return balances, nil
}

// appliedLeg 应用后的分录 (含账户 ID 与 balance_after).
type appliedLeg struct {
	AccountID    uuid.UUID
	Scope        accountScope
	Direction    types.Direction
	Amount       *big.Int
	BalanceAfter *big.Int
}

// isMerchantLiabilityScope 商户自有负债科目判定 (FIX-A/A2):
// owner 非系统哨兵 且 normal_side=CREDIT — 即 MERCHANT_AVAILABLE /
// ORDER_ESCROW_LIAB / PAYOUT_LOCKED_LIAB / SUSPENSE_*_LIAB 四类;
// 平台托管 (CUSTODY_*, 借方科目) 与平台权益/收入 (TREASURY_EQUITY_CAPITAL /
// FEE_REVENUE, 系统哨兵持有) 均不在此列 — 后者模板 9 借方不足即拒账,
// 绝不透支转应收.
func isMerchantLiabilityScope(sc accountScope) bool {
	return sc.Owner != types.SystemMerchantID &&
		sc.Account.NormalSide() == types.DirectionCredit
}

// applyLegs 逐腿应用余额变动: direction == normal_side 则 +amount 否则 -amount;
// 变动后 < 0 → ErrNegativeBalance; 模板 9 对商户自有负债科目透支自动
// 钳制到可用余额, 差额拆分进 MERCHANT_REORG_RECEIVABLE 并标记商户冻结.
func (s *Service) applyLegs(template types.EntryTemplateCode, legs []leg,
	accountIDs map[accountScope]uuid.UUID, idToScope map[uuid.UUID]accountScope,
	balances map[uuid.UUID]*big.Int) ([]appliedLeg, []uuid.UUID, error) {

	var applied []appliedLeg
	frozenSet := map[uuid.UUID]struct{}{}

	for _, l := range legs {
		id := accountIDs[l.Scope]
		normal := l.Scope.Account.NormalSide()
		bal := balances[id]
		amt := new(big.Int).Set(l.Amount)

		// 模板 9 (A2): 商户自有负债科目借方透支 -> 锁内钳制 bal,
		// 差额转 MERCHANT_REORG_RECEIVABLE + 商户冻结.
		if template == types.TemplateReorgReversal &&
			l.Direction == types.DirectionDebit &&
			isMerchantLiabilityScope(l.Scope) &&
			bal.Cmp(amt) < 0 {

			shortfall := new(big.Int).Sub(amt, bal)
			receivableScope := accountScope{
				Owner: l.Scope.Owner, Chain: l.Scope.Chain, Asset: l.Scope.Asset,
				Account: types.AccountMerchantReorgReceivable,
			}
			rid, ok := accountIDs[receivableScope]
			if !ok {
				return nil, nil, fmt.Errorf("ledger: missing receivable account for spill")
			}
			// 追偿应收为借方科目: DEBIT 增加.
			rBal := balances[rid]
			rAfter := new(big.Int).Add(rBal, shortfall)
			applied = append(applied, appliedLeg{
				AccountID: rid, Scope: receivableScope, Direction: types.DirectionDebit,
				Amount: new(big.Int).Set(shortfall), BalanceAfter: new(big.Int).Set(rAfter),
			})
			balances[rid] = rAfter
			frozenSet[l.Scope.Owner] = struct{}{}

			if bal.Sign() == 0 {
				continue // 可用余额为零: 整条借方由应收承担.
			}
			amt.Set(bal) // 剩余部分按实际可用额借记.
		}

		var after *big.Int
		if l.Direction == normal {
			after = new(big.Int).Add(bal, amt)
		} else {
			after = new(big.Int).Sub(bal, amt)
		}
		if after.Sign() < 0 {
			return nil, nil, fmt.Errorf("%w: %s %s balance %s, %s %s",
				ports.ErrNegativeBalance, l.Scope.Account, id, bal.String(), l.Direction, amt.String())
		}
		applied = append(applied, appliedLeg{
			AccountID: id, Scope: l.Scope, Direction: l.Direction,
			Amount: new(big.Int).Set(amt), BalanceAfter: new(big.Int).Set(after),
		})
		balances[id] = after
	}

	frozen := make([]uuid.UUID, 0, len(frozenSet))
	for id := range frozenSet {
		frozen = append(frozen, id)
	}
	return applied, frozen, nil
}

// writePosting 更新账户余额并插入带 balance_after 的分录腿,
// 返回带生成主键/时间戳的 types.LedgerEntry 投影.
func (s *Service) writePosting(ctx context.Context, tx pgx.Tx, txnID uuid.UUID,
	applied []appliedLeg, balances map[uuid.UUID]*big.Int) ([]types.LedgerEntry, error) {

	for id, bal := range balances {
		tag, err := tx.Exec(ctx, `UPDATE ledger_accounts SET balance = $2 WHERE id = $1`,
			id, pgtype.Numeric{Int: bal, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("ledger: update balance %s: %w", id, err)
		}
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("ledger: update balance %s affected %d rows", id, tag.RowsAffected())
		}
	}
	entries := make([]types.LedgerEntry, 0, len(applied))
	for _, l := range applied {
		var e types.LedgerEntry
		err := tx.QueryRow(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, balance_after)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, created_at`,
			txnID, l.AccountID, l.Direction.String(),
			pgtype.Numeric{Int: l.Amount, Valid: true},
			pgtype.Numeric{Int: l.BalanceAfter, Valid: true}).
			Scan(&e.ID, &e.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("ledger: insert entry: %w", err)
		}
		e.TransactionID = txnID
		e.AccountID = l.AccountID
		e.Direction = l.Direction
		e.Amount = types.U256FromBig(l.Amount)
		e.BalanceAfter = types.U256FromBig(l.BalanceAfter)
		entries = append(entries, e)
	}
	return entries, nil
}

// freezeMerchants 同事务置 merchants.payout_frozen = true (模板 9 透支).
func (s *Service) freezeMerchants(ctx context.Context, tx pgx.Tx, merchants []uuid.UUID) error {
	for _, m := range merchants {
		if m == types.SystemMerchantID {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE merchants SET payout_frozen = true WHERE id = $1`, m); err != nil {
			return fmt.Errorf("ledger: freeze merchant %s: %w", m, err)
		}
	}
	return nil
}

// outboxPayload webhook_outbox.payload_json 内容.
type outboxPayload struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	ReferenceType string    `json:"reference_type"`
	ReferenceID   string    `json:"reference_id"`
	Legs          []struct {
		Account    string `json:"account"`
		Direction  string `json:"direction"`
		Amount     string `json:"amount"`
		Asset      string `json:"asset_symbol"`
		ChainID    uint64 `json:"chain_id"`
		MerchantID string `json:"owner_merchant_id"`
	} `json:"legs"`
}

// writeOutbox 同事务写 Transactional Outbox. 仅对分录腿中出现的非系统商户
// 发事件 (纯平台科目记账无业务回调接收方, 跳过).
func (s *Service) writeOutbox(ctx context.Context, tx pgx.Tx, txn types.LedgerTransaction,
	req ports.PostEntryRequest, applied []appliedLeg) error {

	merchants := map[uuid.UUID]struct{}{}
	var p outboxPayload
	p.TransactionID = txn.ID
	p.ReferenceType = req.Template.String()
	p.ReferenceID = req.ReferenceID
	for _, l := range applied {
		if l.Scope.Owner != types.SystemMerchantID {
			merchants[l.Scope.Owner] = struct{}{}
		}
		p.Legs = append(p.Legs, struct {
			Account    string `json:"account"`
			Direction  string `json:"direction"`
			Amount     string `json:"amount"`
			Asset      string `json:"asset_symbol"`
			ChainID    uint64 `json:"chain_id"`
			MerchantID string `json:"owner_merchant_id"`
		}{
			Account: l.Scope.Account.String(), Direction: l.Direction.String(),
			Amount: l.Amount.String(), Asset: l.Scope.Asset,
			ChainID: uint64(l.Scope.Chain), MerchantID: l.Scope.Owner.String(),
		})
	}
	if len(merchants) == 0 {
		return nil
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("ledger: marshal outbox payload: %w", err)
	}
	for m := range merchants {
		_, err := tx.Exec(ctx, `
			INSERT INTO webhook_outbox (merchant_id, event_type, payload_json)
			VALUES ($1, $2, $3)`,
			m, req.Template.String(), string(payload))
		if err != nil {
			return fmt.Errorf("ledger: insert outbox: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// GetBalances — ports.LedgerService
// ---------------------------------------------------------------------------

// GetBalances 查询 (owner, chain, asset) 维度各科目余额.
func (s *Service) GetBalances(ctx context.Context, q ports.BalanceQuery) ([]ports.AccountBalance, error) {
	query := `
		SELECT account_type, balance FROM ledger_accounts
		WHERE owner_merchant_id = $1 AND chain_id = $2 AND asset_symbol = $3`
	args := []any{q.OwnerMerchantID, int64(q.ChainID), q.AssetSymbol}
	if len(q.Accounts) > 0 {
		names := make([]string, len(q.Accounts))
		for i, a := range q.Accounts {
			names[i] = a.String()
		}
		query += ` AND account_type = ANY($4::varchar[])`
		args = append(args, names)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger: get balances: %w", err)
	}
	defer rows.Close()
	out := []ports.AccountBalance{}
	for rows.Next() {
		var acc string
		var bal pgtype.Numeric
		if err := rows.Scan(&acc, &bal); err != nil {
			return nil, fmt.Errorf("ledger: scan balance: %w", err)
		}
		u, err := numericToU256(bal)
		if err != nil {
			return nil, err
		}
		out = append(out, ports.AccountBalance{Account: types.LedgerAccountCode(acc), Balance: u})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// ---------------------------------------------------------------------------
// RunInvariantCheck — 双层账务巡检 (ports.LedgerService)
// ---------------------------------------------------------------------------

// RunInvariantCheck 双层巡检:
//
//	第一层 (按 chain_id, asset_symbol): SUM(debit-normal balances) ==
//	SUM(credit-normal balances) 且全部账户 balance >= 0;
//	第二层 (Snapshot != nil): safe_head 链上快照四项核对 —
//	UnsweptVaultTotal==CUSTODY_UNSWEPT, ColdWalletBalance==CUSTODY_COLD,
//	HotPayoutTokenBalance==CUSTODY_HOT_PAYOUT, HotWalletNativeTotal==
//	HOT_WALLET_NATIVE (原生币恒定以 asset='ETH' 维度核算).
func (s *Service) RunInvariantCheck(ctx context.Context, req ports.InvariantCheckRequest) (ports.InvariantReport, error) {
	report := ports.InvariantReport{
		ChainID:        req.ChainID,
		AssetSymbol:    req.AssetSymbol,
		SafeHeadHeight: req.SafeHeadHeight,
		CheckedAt:      time.Now().UTC(),
	}

	// ---- 第一层: 账本内试算平衡 ----
	rows, err := s.pool.Query(ctx, `
		SELECT account_type, normal_side, balance
		  FROM ledger_accounts
		 WHERE chain_id = $1 AND asset_symbol = $2`,
		int64(req.ChainID), req.AssetSymbol)
	if err != nil {
		return report, fmt.Errorf("ledger: invariant query: %w", err)
	}
	drTotal := new(big.Int)
	crTotal := new(big.Int)
	for rows.Next() {
		var acc, side string
		var bal pgtype.Numeric
		if err := rows.Scan(&acc, &side, &bal); err != nil {
			rows.Close()
			return report, fmt.Errorf("ledger: invariant scan: %w", err)
		}
		u, err := numericToU256(bal)
		if err != nil {
			rows.Close()
			return report, err
		}
		if side == types.DirectionDebit.String() {
			drTotal.Add(drTotal, u.Big())
		} else {
			crTotal.Add(crTotal, u.Big())
		}
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	report.TotalDebits = types.U256FromBig(drTotal)
	report.TotalCredits = types.U256FromBig(crTotal)
	report.TrialBalanced = drTotal.Cmp(crTotal) == 0
	if !report.TrialBalanced {
		report.Violations = append(report.Violations, fmt.Sprintf(
			"trial balance violated on (%d, %s): SUM(DEBIT-side)=%s != SUM(CREDIT-side)=%s",
			req.ChainID, req.AssetSymbol, drTotal.String(), crTotal.String()))
	}
	// balance < 0 违例 (CHECK 应已杜绝, 双保险).
	report.NegativeAccounts = []types.LedgerAccountCode{}
	negRows, err := s.pool.Query(ctx, `
		SELECT account_type FROM ledger_accounts
		 WHERE chain_id = $1 AND asset_symbol = $2 AND balance < 0`,
		int64(req.ChainID), req.AssetSymbol)
	if err != nil {
		return report, fmt.Errorf("ledger: negative scan: %w", err)
	}
	for negRows.Next() {
		var acc string
		if err := negRows.Scan(&acc); err != nil {
			negRows.Close()
			return report, err
		}
		report.NegativeAccounts = append(report.NegativeAccounts, types.LedgerAccountCode(acc))
		report.Violations = append(report.Violations,
			fmt.Sprintf("negative balance: account %s on (%d, %s)", acc, req.ChainID, req.AssetSymbol))
	}
	negRows.Close()

	// ---- 第二层: 链上链下快照核对 ----
	if req.Snapshot != nil {
		type check struct {
			account types.LedgerAccountCode
			asset   string
			onchain types.U256
		}
		checks := []check{
			{types.AccountCustodyUnswept, req.AssetSymbol, req.Snapshot.UnsweptVaultTotal},
			{types.AccountCustodyCold, req.AssetSymbol, req.Snapshot.ColdWalletBalance},
			{types.AccountCustodyHotPayout, req.AssetSymbol, req.Snapshot.HotPayoutTokenBalance},
			// 原生 Gas 科目恒定核算在 asset='ETH' 维度 (与 req.AssetSymbol 正交).
			{types.AccountHotWalletNative, nativeAssetSymbol, req.Snapshot.HotWalletNativeTotal},
		}
		report.ChainDiffs = []ports.AccountDiff{}
		for _, c := range checks {
			ledgerBal, err := s.systemAccountBalance(ctx, req.ChainID, c.asset, c.account)
			if err != nil {
				return report, err
			}
			delta := new(big.Int).Sub(c.onchain.Big(), ledgerBal.Big())
			report.ChainDiffs = append(report.ChainDiffs, ports.AccountDiff{
				Account: c.account, Ledger: ledgerBal, OnChain: c.onchain, Delta: delta,
			})
			if delta.Sign() != 0 {
				report.Violations = append(report.Violations, fmt.Sprintf(
					"chain mismatch %s on chain %d: ledger=%s onchain=%s delta=%s",
					c.account, req.ChainID, ledgerBal.String(), c.onchain.String(), delta.String()))
			}
		}
	}
	return report, nil
}

// systemAccountBalance 查询系统哨兵商户 (SystemMerchantID) 名下科目余额,
// 无账户行视为 0.
func (s *Service) systemAccountBalance(ctx context.Context, chain types.ChainID,
	asset string, account types.LedgerAccountCode) (types.U256, error) {
	var bal pgtype.Numeric
	err := s.pool.QueryRow(ctx, `
		SELECT balance FROM ledger_accounts
		WHERE owner_merchant_id = $1 AND chain_id = $2 AND asset_symbol = $3 AND account_type = $4`,
		types.SystemMerchantID, int64(chain), asset, account.String()).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.U256{}, nil
	}
	if err != nil {
		return types.U256{}, fmt.Errorf("ledger: system balance %s: %w", account, err)
	}
	return numericToU256(bal)
}

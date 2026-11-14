// Package order 实现收单、周期性过期重评与人工审核/提现退款编排 (W3-3).
//
// 四层正交状态机白名单迁移以 internal/types 冻结表为准
// (onchain_deposits 4 态 / payment_orders 11 态 / outbound_tx_intents 7 态 /
// merchant_payouts 5 态), 本包所有跃迁先查 CanTransition 再落库.
//
// 编排要点 (ARCHITECTURE.md §4.1/§5/§6.2):
//   - CreateOrder: 同事务 1:1 建 deposit_addresses + payment_orders
//     (AWAITING_PAYMENT), salt/地址由 internal/create2 离线推导;
//     (merchant_id, idempotency_key) + request_hash 幂等.
//   - 过期重评: AWAITING_PAYMENT -> EXPIRED (无同币种在途);
//     PARTIALLY_PAID -> UNDERPAID_EXPIRED_REVIEW 同事务模板 5 转挂
//     SUSPENSE_REFUNDABLE_LIAB.
//   - 人工审核: 少付折让/迟付履约 -> PAID (模板 4 借 SUSPENSE_REFUNDABLE_LIAB
//     结转 MERCHANT_AVAILABLE + FEE_REVENUE); 创建/取消 CUSTOMER_REFUND;
//     SETTLED_REVERSED 应收清零 -> REVERSAL_RESOLVED; payout_frozen 解冻.
//   - 提现/退款: 四类来源负债 (MERCHANT_AVAILABLE + 三类 SUSPENSE_*_LIAB)
//     申请即冻结入 PAYOUT_LOCKED_LIAB (模板 8 第一腿); 取消原路解冻.
//   - W4 联动钩子: OnPayoutInFlight / OnPayoutConfirmed / OnPayoutFailed /
//     OnPayoutRetry 由出站 Intent 结算驱动 §5.4 状态机 (含 OVERPAID ->
//     PAID 溢缴退款完结、REFUND_IN_PROGRESS -> REFUNDED 联动).
package order

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/ledger"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// Outbox 事件类型 (webhook_outbox.event_type; 由本包发出的订单生命周期事件).
const (
	EventOrderStatusChanged = "ORDER_STATUS_CHANGED"
	EventOrderExpired       = "ORDER_EXPIRED"
	EventPayoutQueued       = "PAYOUT_QUEUED"
	EventPayoutCancelled    = "PAYOUT_CANCELLED"
	EventPayoutCompleted    = "PAYOUT_COMPLETED"
)

// Service 收单/审核/提现退款编排服务. 实现 ports.OrderService;
// 另提供出站结算联动钩子供 W4 indexer/txmanager 在 Wave 2 装配时调用.
type Service struct {
	pool    *pgxpool.Pool
	deriver *create2.Deriver
	ledger  *ledger.Service
	sse     ports.SSEBroker // FIX-G BUG-2: 可选订单实时事件汇 (nil 即降级不发)
}

// NewService 构造编排服务. deriver 为启动期已 VerifyChainFactoryHash
// 强断言的单链 CREATE2 推导器; ledgerSvc 为同窗口复式记账引擎
// (分录写入经其 PostEntryInTx 融入编排事务边界).
func NewService(pool *pgxpool.Pool, deriver *create2.Deriver, ledgerSvc *ledger.Service) *Service {
	return &Service{pool: pool, deriver: deriver, ledger: ledgerSvc}
}

// SetOrderEventSink 装配期可选注入 SSE broker (nil-safe; FIX-G BUG-2:
// 由 cmd/server 在 broker 就绪后注入, 与构造器参数解耦避免签名扩散).
func (s *Service) SetOrderEventSink(b ports.SSEBroker) { s.sse = b }

// publishOrderEvent 提交后 SSE 广播: 仅允许在业务事务 COMMIT 之后调用 —
// 先发后提与回滚后虚报都由调用点的 "commit 成功才触达" 顺序保证.
// broker 未注入为 no-op; 发布失败仅降级 (SSE advisory 面不回滚业务).
func (s *Service) publishOrderEvent(ctx context.Context, ev ports.OrderEvent) {
	if s.sse == nil {
		return
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	_ = s.sse.PublishOrderEvent(ctx, ev)
}

var _ ports.OrderService = (*Service)(nil)

// ---------------------------------------------------------------------------
// 通用扫描/写入辅助
// ---------------------------------------------------------------------------

const orderColumns = `id, merchant_id, idempotency_key, request_hash, chain_id, asset_symbol,
	expected_amount, received_amount, deposit_address_id, status, expires_at, settled_at`

func scanOrder(row pgx.Row) (types.PaymentOrder, error) {
	var o types.PaymentOrder
	var expected, received pgtype.Numeric
	var status string
	var chain int64
	err := row.Scan(&o.ID, &o.MerchantID, &o.IdempotencyKey, &o.RequestHash, &chain,
		&o.AssetSymbol, &expected, &received, &o.DepositAddressID, &status, &o.ExpiresAt, &o.SettledAt)
	if err != nil {
		return o, err
	}
	o.ChainID = types.ChainID(chain)
	o.Status = types.OrderStatus(status)
	if o.ExpectedAmount, err = numericU256(expected); err != nil {
		return o, err
	}
	if o.ReceivedAmount, err = numericU256(received); err != nil {
		return o, err
	}
	return o, nil
}

const payoutColumns = `id, merchant_id, idempotency_key, request_hash, payout_kind,
	source_account_type, source_order_id, chain_id, asset_symbol, to_address, amount,
	retry_seq, admin_refund_ticket, status, created_at`

func scanPayout(row pgx.Row) (types.MerchantPayout, error) {
	var p types.MerchantPayout
	var amount pgtype.Numeric
	var kind, source, status, toAddr string
	var chain int64
	err := row.Scan(&p.ID, &p.MerchantID, &p.IdempotencyKey, &p.RequestHash, &kind,
		&source, &p.SourceOrderID, &chain, &p.AssetSymbol, &toAddr, &amount,
		&p.RetrySeq, &p.AdminRefundTicket, &status, &p.CreatedAt)
	if err != nil {
		return p, err
	}
	p.ChainID = types.ChainID(chain)
	p.PayoutKind = types.PayoutKind(kind)
	p.SourceAccount = types.LedgerAccountCode(source)
	p.Status = types.PayoutStatus(status)
	p.ToAddress = types.HexToAddress(toAddr)
	if p.Amount, err = numericU256(amount); err != nil {
		return p, err
	}
	return p, nil
}

// numericU256 NUMERIC(78,0) -> types.U256 (ledger 包同款语义, 独立实现避免
// 导出私有解码器).
func numericU256(n pgtype.Numeric) (types.U256, error) {
	if !n.Valid {
		return types.U256{}, errors.New("order: NULL numeric")
	}
	v := new(big.Int).Set(n.Int)
	switch {
	case n.Exp > 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil))
	case n.Exp < 0:
		d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n.Exp)), nil)
		q, r := new(big.Int).QuoRem(v, d, new(big.Int))
		if r.Sign() != 0 {
			return types.U256{}, fmt.Errorf("order: fractional numeric %s", n.Int.String())
		}
		v = q
	}
	return types.U256FromBig(v), nil
}

func u256Numeric(u types.U256) pgtype.Numeric {
	return pgtype.Numeric{Int: u.Big(), Valid: true}
}

// addrHex 0x 前缀小写地址编码 (P0-1 hex 规范; 禁用 EIP-55 混合大小写).
func addrHex(a types.Address) string {
	return "0x" + hex.EncodeToString(a.Bytes())
}

// supportedTokenContract 解析 supported_assets 该链该币种的 ERC-20 合约
// (退款票据 token 字段校验与 dispatcher ERC-20 transfer 目标同一口径).
func (s *Service) supportedTokenContract(ctx context.Context, chainID types.ChainID, symbol string) (types.Address, error) {
	var tokenHex string
	err := s.pool.QueryRow(ctx,
		`SELECT token_contract FROM supported_assets WHERE chain_id = $1 AND asset_symbol = $2`,
		int64(chainID), symbol).Scan(&tokenHex)
	if err != nil {
		return types.Address{}, fmt.Errorf("%w: resolve token %s@%d", ports.ErrNotFound, symbol, chainID)
	}
	return types.HexToAddress(tokenHex), nil
}

// isUniqueViolation 判定 PG 23505 且可选约束名匹配.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return constraint == "" || pgErr.ConstraintName == constraint
	}
	return false
}

// lockOrder 事务内锁定订单行并返回当前投影.
func (s *Service) lockOrder(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) (types.PaymentOrder, error) {
	o, err := scanOrder(tx.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE id = $1 FOR UPDATE`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ports.ErrNotFound
	}
	return o, err
}

// transitionOrderStatus 白名单校验 + 状态落库 (调用方已持有行锁).
func transitionOrderStatus(ctx context.Context, tx pgx.Tx, o types.PaymentOrder, to types.OrderStatus) error {
	if !o.Status.CanTransition(to) {
		return fmt.Errorf("%w: order %s %s -> %s", ports.ErrInvalidTransition, o.ID, o.Status, to)
	}
	_, err := tx.Exec(ctx, `UPDATE payment_orders SET status = $2 WHERE id = $1`, o.ID, to.String())
	return err
}

// lockPayout 事务内锁定提现/退款行.
func (s *Service) lockPayout(ctx context.Context, tx pgx.Tx, payoutID uuid.UUID) (types.MerchantPayout, error) {
	p, err := scanPayout(tx.QueryRow(ctx,
		`SELECT `+payoutColumns+` FROM merchant_payouts WHERE id = $1 FOR UPDATE`, payoutID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ports.ErrNotFound
	}
	return p, err
}

// transitionPayoutStatus 白名单校验 + 状态落库.
func transitionPayoutStatus(ctx context.Context, tx pgx.Tx, p types.MerchantPayout, to types.PayoutStatus) error {
	if !p.Status.CanTransition(to) {
		return fmt.Errorf("%w: payout %s %s -> %s", ports.ErrInvalidTransition, p.ID, p.Status, to)
	}
	_, err := tx.Exec(ctx, `UPDATE merchant_payouts SET status = $2 WHERE id = $1`, p.ID, to.String())
	return err
}

// notifyOrderEvent 同事务写订单 outbox 事件 (供 W4 webhook 投递器消费).
func notifyOrderEvent(ctx context.Context, tx pgx.Tx, o types.PaymentOrder, eventType string) error {
	payload, err := json.Marshal(map[string]any{
		"order_id":        o.ID.String(),
		"merchant_id":     o.MerchantID.String(),
		"status":          o.Status.String(),
		"expected_amount": o.ExpectedAmount.String(),
		"received_amount": o.ReceivedAmount.String(),
		"chain_id":        uint64(o.ChainID),
		"asset_symbol":    o.AssetSymbol,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO webhook_outbox (merchant_id, event_type, payload_json) VALUES ($1,$2,$3)`,
		o.MerchantID, eventType, string(payload))
	return err
}

// ---------------------------------------------------------------------------
// CreateOrder — 下单幂等编排 (同事务 1:1 deposit_addresses + payment_orders)
// ---------------------------------------------------------------------------

// CreateOrder 幂等下单. (merchant_id, idempotency_key) 同 request_hash 幂等
// 返回既有订单; 异 request_hash 返回 ErrIdempotencyConflict.
func (s *Service) CreateOrder(ctx context.Context, req ports.CreateOrderRequest) (types.PaymentOrder, error) {
	if req.ExpectedAmount.Sign() <= 0 {
		return types.PaymentOrder{}, fmt.Errorf("%w: expected_amount must be > 0", ports.ErrNonPositiveAmount)
	}

	// 快速幂等预检.
	if existing, found, err := s.findOrderByIdem(ctx, req.MerchantID, req.IdempotencyKey); err != nil {
		return types.PaymentOrder{}, err
	} else if found {
		return resolveIdemOrder(existing, req.RequestHash)
	}

	orderID := uuid.New()
	addrID := uuid.New()
	salt := s.deriver.Salt(req.ChainID, req.MerchantID, orderID)
	vault := s.deriver.VaultAddress(salt)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return types.PaymentOrder{}, fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, `
		INSERT INTO deposit_addresses (id, chain_id, address, create2_salt)
		VALUES ($1, $2, $3, $4)`,
		addrID, int64(req.ChainID), addrHex(vault), salt[:]); err != nil {
		return types.PaymentOrder{}, fmt.Errorf("order: insert deposit address: %w", err)
	}

	o := types.PaymentOrder{
		ID:               orderID,
		MerchantID:       req.MerchantID,
		IdempotencyKey:   req.IdempotencyKey,
		RequestHash:      req.RequestHash,
		ChainID:          req.ChainID,
		AssetSymbol:      req.AssetSymbol,
		ExpectedAmount:   req.ExpectedAmount,
		ReceivedAmount:   types.U256{},
		DepositAddressID: addrID,
		Status:           types.OrderAwaitingPayment,
		ExpiresAt:        req.ExpiresAt,
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO payment_orders
		  (id, merchant_id, idempotency_key, request_hash, chain_id, asset_symbol,
		   expected_amount, received_amount, deposit_address_id, status, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$9,$10)`,
		o.ID, o.MerchantID, o.IdempotencyKey, o.RequestHash, int64(o.ChainID),
		o.AssetSymbol, u256Numeric(o.ExpectedAmount), o.DepositAddressID,
		o.Status.String(), o.ExpiresAt)
	if err != nil {
		if isUniqueViolation(err, "uq_orders_merchant_idem") {
			// 并发首写已提交: 回滚本次插入 (连带 deposit_addresses),
			// 读回赢家订单做 request_hash 比对.
			_ = tx.Rollback(ctx)
			if existing, found, rerr := s.findOrderByIdem(ctx, req.MerchantID, req.IdempotencyKey); rerr != nil {
				return types.PaymentOrder{}, rerr
			} else if found {
				return resolveIdemOrder(existing, req.RequestHash)
			}
			return types.PaymentOrder{}, fmt.Errorf("%w: idempotency race lost", ports.ErrUnavailable)
		}
		return types.PaymentOrder{}, fmt.Errorf("order: insert payment order: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.PaymentOrder{}, fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	return o, nil
}

func (s *Service) findOrderByIdem(ctx context.Context, merchantID uuid.UUID, key string) (types.PaymentOrder, bool, error) {
	o, err := scanOrder(s.pool.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE merchant_id = $1 AND idempotency_key = $2`,
		merchantID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return o, false, nil
	}
	if err != nil {
		return o, false, fmt.Errorf("order: find by idempotency key: %w", err)
	}
	return o, true, nil
}

// resolveIdemOrder 幂等命中仲裁: request_hash 一致返回既有订单, 否则冲突.
func resolveIdemOrder(o types.PaymentOrder, requestHash string) (types.PaymentOrder, error) {
	if o.RequestHash != requestHash {
		return types.PaymentOrder{}, fmt.Errorf("%w: merchant=%s key=%s",
			ports.ErrIdempotencyConflict, o.MerchantID, o.IdempotencyKey)
	}
	return o, nil
}

// GetOrder 查询订单.
func (s *Service) GetOrder(ctx context.Context, orderID uuid.UUID) (types.PaymentOrder, error) {
	o, err := scanOrder(s.pool.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE id = $1`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ports.ErrNotFound
	}
	if err != nil {
		return o, fmt.Errorf("order: get order: %w", err)
	}
	return o, nil
}

// GetPayout 查询提现/退款单.
func (s *Service) GetPayout(ctx context.Context, payoutID uuid.UUID) (types.MerchantPayout, error) {
	p, err := scanPayout(s.pool.QueryRow(ctx,
		`SELECT `+payoutColumns+` FROM merchant_payouts WHERE id = $1`, payoutID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ports.ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("order: get payout: %w", err)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// RequestPayout / CancelPayout — §5.4 冻结/解冻编排
// ---------------------------------------------------------------------------

// RequestPayout 发起提现或客户退款 (幂等): 同事务记 PAYOUT_FLOW 冻结腿
// (DR source_account / CR PAYOUT_LOCKED_LIAB) 并落 LOCKED_QUEUED 单.
func (s *Service) RequestPayout(ctx context.Context, req ports.PayoutRequest) (types.MerchantPayout, error) {
	if !req.SourceAccount.IsPayoutSource() {
		return types.MerchantPayout{}, fmt.Errorf("%w: source %s is not a payout source liability",
			ports.ErrInvalidAccountCode, req.SourceAccount)
	}
	if req.Amount.Sign() <= 0 {
		return types.MerchantPayout{}, fmt.Errorf("%w: payout amount must be > 0", ports.ErrNonPositiveAmount)
	}
	// FIX-D1: 行主键 = 票据绑定 payout_uuid — 广播时
	// intent.reference_id == merchant_payouts.id == SignTransactionRequest.
	// payout_uuid, 与票据内嵌 uuid 逐字节相等才可通过 signer 校验.
	payoutID := req.PayoutID
	switch req.Kind {
	case types.PayoutKindWithdrawal:
		// 常规提现: RefundTicket 必须为 nil (DTO 契约).
		if req.RefundTicket != nil {
			return types.MerchantPayout{}, fmt.Errorf("order: WITHDRAWAL must not carry RefundTicket")
		}
	case types.PayoutKindCustomerRefund:
		// 客户异常退款: 127B 票据必带且内嵌 retry_seq 必须为 0
		// (新单 retry_seq 列恒 0, 与 signer 槽位绑定逐字节对齐).
		ticketID, terr := validateNewRefundTicket(req.RefundTicket)
		if terr != nil {
			return types.MerchantPayout{}, terr
		}
		// FIX-D1: 载荷字段 vs 请求字段交叉校验 — 与 signer 验签后
		// 对 calldata 的逐字节比对同一冻结口径, 建单即拒错票.
		token, terr := s.supportedTokenContract(ctx, req.ChainID, req.AssetSymbol)
		if terr != nil {
			return types.MerchantPayout{}, terr
		}
		if terr := validateTicketFields(req.RefundTicket, req.ChainID,
			token, req.ToAddress, req.Amount, time.Now()); terr != nil {
			return types.MerchantPayout{}, terr
		}
		if payoutID != uuid.Nil && payoutID != ticketID {
			return types.MerchantPayout{}, fmt.Errorf("%w: payout_uuid %s != ticket-embedded uuid %s",
				ErrInvalidRefundTicket, payoutID, ticketID)
		}
		payoutID = ticketID
	default:
		return types.MerchantPayout{}, fmt.Errorf("%w: invalid payout kind %q", ports.ErrInvalidTransition, req.Kind)
	}
	if payoutID == uuid.Nil {
		payoutID = uuid.New()
	}

	// 幂等预检.
	if existing, found, err := s.findPayoutByIdem(ctx, req.MerchantID, req.IdempotencyKey); err != nil {
		return types.MerchantPayout{}, err
	} else if found {
		if existing.RequestHash != req.RequestHash {
			return types.MerchantPayout{}, fmt.Errorf("%w: merchant=%s key=%s",
				ports.ErrIdempotencyConflict, req.MerchantID, req.IdempotencyKey)
		}
		return existing, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return types.MerchantPayout{}, fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := s.createPayoutLocked(ctx, tx, payoutParams{
		ID:             payoutID,
		MerchantID:     req.MerchantID,
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    req.RequestHash,
		Kind:           req.Kind,
		SourceAccount:  req.SourceAccount,
		SourceOrderID:  req.SourceOrderID,
		ChainID:        req.ChainID,
		AssetSymbol:    req.AssetSymbol,
		ToAddress:      req.ToAddress,
		Amount:         req.Amount,
		RefundTicket:   req.RefundTicket,
	})
	if err != nil {
		if isUniqueViolation(err, "uq_payouts_merchant_idem") {
			_ = tx.Rollback(ctx)
			if existing, found, rerr := s.findPayoutByIdem(ctx, req.MerchantID, req.IdempotencyKey); rerr != nil {
				return types.MerchantPayout{}, rerr
			} else if found {
				if existing.RequestHash != req.RequestHash {
					return types.MerchantPayout{}, ports.ErrIdempotencyConflict
				}
				return existing, nil
			}
			return types.MerchantPayout{}, fmt.Errorf("%w: idempotency race lost", ports.ErrUnavailable)
		}
		if isUniqueViolation(err, "pk_merchant_payouts") {
			// FIX-D1: 客户端 payout_uuid 幂等锚撞上既有主键 — 同一锚
			// 同 request_hash 回放返回既有单, 异载重用即幂等冲突.
			_ = tx.Rollback(ctx)
			existing, gerr := s.GetPayout(ctx, payoutID)
			if gerr != nil {
				return types.MerchantPayout{}, gerr
			}
			if existing.RequestHash != req.RequestHash {
				return types.MerchantPayout{}, fmt.Errorf("%w: payout_uuid %s already used with a different request",
					ports.ErrIdempotencyConflict, payoutID)
			}
			return existing, nil
		}
		return types.MerchantPayout{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.MerchantPayout{}, fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	return p, nil
}

// payoutParams createPayoutLocked 参数包 (RequestPayout 与 ReviewOrder 共用).
type payoutParams struct {
	ID             uuid.UUID
	MerchantID     uuid.UUID
	IdempotencyKey string
	RequestHash    string
	Kind           types.PayoutKind
	SourceAccount  types.LedgerAccountCode
	SourceOrderID  *uuid.UUID
	ChainID        types.ChainID
	AssetSymbol    string
	ToAddress      types.Address
	Amount         types.U256
	RefundTicket   *ports.RefundTicket
}

// createPayoutLocked 编排事务内: 校验商户未冻结 -> 记 PAYOUT_FLOW 冻结腿
// -> 落 LOCKED_QUEUED 单 -> outbox 事件.
func (s *Service) createPayoutLocked(ctx context.Context, tx pgx.Tx, pp payoutParams) (types.MerchantPayout, error) {
	var frozen bool
	if err := tx.QueryRow(ctx, `SELECT payout_frozen FROM merchants WHERE id = $1 FOR UPDATE`,
		pp.MerchantID).Scan(&frozen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.MerchantPayout{}, ports.ErrNotFound
		}
		return types.MerchantPayout{}, fmt.Errorf("order: lock merchant: %w", err)
	}
	if frozen {
		return types.MerchantPayout{}, ports.ErrMerchantFrozen
	}

	// FIX-D3: WITHDRAWAL 建单前白名单前置校验 — 须存在 (merchant, chain,
	// to_address) 三元匹配且 status='ACTIVE'、now < valid_until 的
	// merchant_withdrawal_addresses 记录; CUSTOMER_REFUND 收款方由
	// 离线管理员票据授权, 不走商户白名单 (openapi 语义一致).
	if pp.Kind == types.PayoutKindWithdrawal {
		var whitelisted bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM merchant_withdrawal_addresses
				WHERE merchant_id = $1 AND chain_id = $2 AND address = $3
				  AND status = 'ACTIVE' AND valid_until > now())`,
			pp.MerchantID, int64(pp.ChainID), addrHex(pp.ToAddress)).Scan(&whitelisted); err != nil {
			return types.MerchantPayout{}, fmt.Errorf("order: whitelist check: %w", err)
		}
		if !whitelisted {
			return types.MerchantPayout{}, fmt.Errorf("%w: %s chain=%d merchant=%s",
				ErrAddressNotWhitelisted, addrHex(pp.ToAddress), pp.ChainID, pp.MerchantID)
		}
	}

	// 冻结腿: DR source_account / CR PAYOUT_LOCKED_LIAB (模板 8 第一腿).
	// reference_id 以 payout 主键派生, 天然幂等.
	if _, err := s.ledger.PostEntryInTx(ctx, tx, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "payout_freeze:" + pp.ID.String(),
		Description: fmt.Sprintf("payout %s freeze %s %s", pp.ID, pp.Amount.String(), pp.AssetSymbol),
		Legs: []ports.PostEntryLeg{
			{
				OwnerMerchantID: pp.MerchantID, ChainID: pp.ChainID, AssetSymbol: pp.AssetSymbol,
				Account: pp.SourceAccount, Direction: types.DirectionDebit, Amount: pp.Amount,
			},
			{
				OwnerMerchantID: pp.MerchantID, ChainID: pp.ChainID, AssetSymbol: pp.AssetSymbol,
				Account: types.AccountPayoutLockedLiab, Direction: types.DirectionCredit, Amount: pp.Amount,
			},
		},
	}); err != nil {
		return types.MerchantPayout{}, err
	}

	var ticket []byte
	if pp.RefundTicket != nil {
		// admin_refund_ticket: 127B 载荷 || 64B Ed25519 签名打包 (§2.3 B).
		ticket = append(append([]byte{}, pp.RefundTicket.SignedPayload...), pp.RefundTicket.AdminSignature...)
	}
	p := types.MerchantPayout{
		ID:                pp.ID,
		MerchantID:        pp.MerchantID,
		IdempotencyKey:    pp.IdempotencyKey,
		RequestHash:       pp.RequestHash,
		PayoutKind:        pp.Kind,
		SourceAccount:     pp.SourceAccount,
		SourceOrderID:     pp.SourceOrderID,
		ChainID:           pp.ChainID,
		AssetSymbol:       pp.AssetSymbol,
		ToAddress:         pp.ToAddress,
		Amount:            pp.Amount,
		RetrySeq:          0,
		AdminRefundTicket: ticket,
		Status:            types.PayoutLockedQueued,
	}
	var srcOrder any
	if pp.SourceOrderID != nil {
		srcOrder = *pp.SourceOrderID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO merchant_payouts
		  (id, merchant_id, idempotency_key, request_hash, payout_kind, source_account_type,
		   source_order_id, chain_id, asset_symbol, to_address, amount, retry_seq,
		   admin_refund_ticket, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,0,$12,$13)`,
		p.ID, p.MerchantID, p.IdempotencyKey, p.RequestHash, p.PayoutKind.String(),
		p.SourceAccount.String(), srcOrder, int64(p.ChainID), p.AssetSymbol,
		addrHex(p.ToAddress), u256Numeric(p.Amount), p.AdminRefundTicket, p.Status.String())
	if err != nil {
		return types.MerchantPayout{}, fmt.Errorf("order: insert payout: %w", err)
	}

	payload, _ := json.Marshal(map[string]any{
		"payout_id":      p.ID.String(),
		"merchant_id":    p.MerchantID.String(),
		"payout_kind":    p.PayoutKind.String(),
		"source_account": p.SourceAccount.String(),
		"amount":         p.Amount.String(),
		"to_address":     addrHex(p.ToAddress),
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO webhook_outbox (merchant_id, event_type, payload_json) VALUES ($1,$2,$3)`,
		p.MerchantID, EventPayoutQueued, string(payload)); err != nil {
		return types.MerchantPayout{}, fmt.Errorf("order: payout outbox: %w", err)
	}
	return p, nil
}

func (s *Service) findPayoutByIdem(ctx context.Context, merchantID uuid.UUID, key string) (types.MerchantPayout, bool, error) {
	p, err := scanPayout(s.pool.QueryRow(ctx,
		`SELECT `+payoutColumns+` FROM merchant_payouts WHERE merchant_id = $1 AND idempotency_key = $2`,
		merchantID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("order: find payout by idempotency key: %w", err)
	}
	return p, true, nil
}

// CancelPayout 取消未广播 (LOCKED_QUEUED) 或 FAILED_RETRYABLE 的提现/退款:
// 同事务记 PAYOUT_FLOW 解冻腿 (DR PAYOUT_LOCKED_LIAB / CR source_account)
// -> CANCELLED_UNLOCKED; 若关联订单处于 REFUND_IN_PROGRESS 则退回
// LATE_PAID_REVIEW 人工决策位.
func (s *Service) CancelPayout(ctx context.Context, payoutID uuid.UUID) (types.MerchantPayout, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return types.MerchantPayout{}, fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, oChanged, err := s.cancelPayoutInTx(ctx, tx, payoutID)
	if err != nil {
		return types.MerchantPayout{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.MerchantPayout{}, fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	if oChanged != nil {
		// FIX-G BUG-2: 退款单取消订单退回人工决策位 — 提交后广播.
		s.publishOrderEvent(ctx, ports.OrderEvent{
			OrderID: oChanged.ID, Type: ports.OrderEventStatusChanged,
			Status: oChanged.Status, ReceivedAmount: oChanged.ReceivedAmount,
		})
	}
	return p, nil
}

// cancelPayoutInTx 同事务取消提现/退款单; 返回取消的 payout 与因退款单取消
// 而退回 LATE_PAID_REVIEW 的订单快照 (未涉订单时 nil — FIX-G BUG-2:
// 调用方在 commit 成功后据此发 SSE 事件).
func (s *Service) cancelPayoutInTx(ctx context.Context, tx pgx.Tx, payoutID uuid.UUID) (types.MerchantPayout, *types.PaymentOrder, error) {
	p, err := s.lockPayout(ctx, tx, payoutID)
	if err != nil {
		return p, nil, err
	}
	if p.Status == types.PayoutCancelledUnlocked {
		return p, nil, ports.ErrTerminalState
	}
	if err := transitionPayoutStatus(ctx, tx, p, types.PayoutCancelledUnlocked); err != nil {
		return p, nil, err
	}
	// 解冻腿: DR PAYOUT_LOCKED_LIAB / CR source_account (模板 8 第三腿).
	if _, err := s.ledger.PostEntryInTx(ctx, tx, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "payout_unfreeze:" + p.ID.String(),
		Description: fmt.Sprintf("payout %s cancel unfreeze %s %s", p.ID, p.Amount.String(), p.AssetSymbol),
		Legs: []ports.PostEntryLeg{
			{
				OwnerMerchantID: p.MerchantID, ChainID: p.ChainID, AssetSymbol: p.AssetSymbol,
				Account: types.AccountPayoutLockedLiab, Direction: types.DirectionDebit, Amount: p.Amount,
			},
			{
				OwnerMerchantID: p.MerchantID, ChainID: p.ChainID, AssetSymbol: p.AssetSymbol,
				Account: p.SourceAccount, Direction: types.DirectionCredit, Amount: p.Amount,
			},
		},
	}); err != nil {
		return p, nil, err
	}
	// 订单退款单取消 -> 订单退回人工决策位.
	var oChanged *types.PaymentOrder
	if p.SourceOrderID != nil && p.PayoutKind == types.PayoutKindCustomerRefund {
		o, err := s.lockOrder(ctx, tx, *p.SourceOrderID)
		if err == nil && o.Status == types.OrderRefundInProgress {
			if err := transitionOrderStatus(ctx, tx, o, types.OrderLatePaidReview); err != nil {
				return p, nil, err
			}
			o.Status = types.OrderLatePaidReview
			if err := notifyOrderEvent(ctx, tx, o, EventOrderStatusChanged); err != nil {
				return p, nil, err
			}
			snap := o
			oChanged = &snap
		}
	}
	p.Status = types.PayoutCancelledUnlocked
	return p, oChanged, nil
}

// UnfreezeMerchant 管理员复核解除 payout_frozen
// (前置: MERCHANT_REORG_RECEIVABLE 全维度已清零).
func (s *Service) UnfreezeMerchant(ctx context.Context, merchantID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var outstanding pgtype.Numeric
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(balance), 0) FROM ledger_accounts
		WHERE owner_merchant_id = $1 AND account_type = $2`,
		merchantID, types.AccountMerchantReorgReceivable.String()).Scan(&outstanding); err != nil {
		return fmt.Errorf("order: sum receivable: %w", err)
	}
	u, err := numericU256(outstanding)
	if err != nil {
		return err
	}
	if u.Sign() != 0 {
		return fmt.Errorf("%w: merchant %s reorg receivable %s outstanding",
			ports.ErrInvalidTransition, merchantID, u.String())
	}
	if _, err := tx.Exec(ctx,
		`UPDATE merchants SET payout_frozen = false WHERE id = $1`, merchantID); err != nil {
		return fmt.Errorf("order: unfreeze merchant: %w", err)
	}
	return tx.Commit(ctx)
}

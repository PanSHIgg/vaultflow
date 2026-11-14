package order

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// ReviewOrder — 人工审核闭环 (§5.2 迁移白名单 + §6.2 模板 4/8)
// ---------------------------------------------------------------------------

// ReviewOrder 人工审核编排:
//
//	ACCEPT_UNDERPAID  UNDERPAID_EXPIRED_REVIEW -> PAID
//	                  (模板 4: DR SUSPENSE_REFUNDABLE_LIAB /
//	                   CR MERCHANT_AVAILABLE + FEE_REVENUE, 按实收额结算)
//	ACCEPT_LATE_PAID  LATE_PAID_REVIEW -> PAID (同上)
//	CREATE_REFUND     UNDERPAID_EXPIRED_REVIEW / LATE_PAID_REVIEW
//	                  -> REFUND_IN_PROGRESS (建 CUSTOMER_REFUND 单 + 冻结腿)
//	CANCEL_REFUND     REFUND_IN_PROGRESS -> LATE_PAID_REVIEW
//	                  (取消未成行退款单, 资金原路解冻)
//	RESOLVE_REVERSAL  SETTLED_REVERSED -> REVERSAL_RESOLVED
//	                  (前置: MERCHANT_REORG_RECEIVABLE 已清零)
//
// 迁移不在 §5.2 白名单或当前状态不承载该动作时返回 ErrInvalidTransition.
// OperatorNote 仅记录于 outbox 载荷 (表内无审核备注列, 见交付报告).
func (s *Service) ReviewOrder(ctx context.Context, req ports.ReviewOrderRequest) (types.PaymentOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return types.PaymentOrder{}, fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := s.lockOrder(ctx, tx, req.OrderID)
	if err != nil {
		return types.PaymentOrder{}, err
	}

	switch req.Action {
	case ports.ReviewAcceptUnderpaid:
		if o.Status != types.OrderUnderpaidExpiredReview {
			return o, fmt.Errorf("%w: ACCEPT_UNDERPAID requires UNDERPAID_EXPIRED_REVIEW, got %s",
				ports.ErrInvalidTransition, o.Status)
		}
		if err := s.settleSuspendedToPaid(ctx, tx, &o); err != nil {
			return o, err
		}

	case ports.ReviewAcceptLatePaid:
		if o.Status != types.OrderLatePaidReview {
			return o, fmt.Errorf("%w: ACCEPT_LATE_PAID requires LATE_PAID_REVIEW, got %s",
				ports.ErrInvalidTransition, o.Status)
		}
		if err := s.settleSuspendedToPaid(ctx, tx, &o); err != nil {
			return o, err
		}

	case ports.ReviewCreateRefund:
		if o.Status != types.OrderUnderpaidExpiredReview && o.Status != types.OrderLatePaidReview {
			return o, fmt.Errorf("%w: CREATE_REFUND requires review state, got %s",
				ports.ErrInvalidTransition, o.Status)
		}
		if req.RefundToAddress == nil {
			return o, fmt.Errorf("%w: CREATE_REFUND requires RefundToAddress (§9.2 禁止自动退回 from)",
				ports.ErrInvalidTransition)
		}
		// FIX-D1: 审核退款必带离线管理员 127B 票据 — 票据内嵌 payout_uuid
		// 即退款单主键, 落 admin_refund_ticket 供广播时透传 signer 自证.
		if req.RefundTicket == nil {
			return o, fmt.Errorf("%w: CREATE_REFUND requires RefundTicket (admin_refund_ticket)",
				ErrInvalidRefundTicket)
		}
		if err := s.createOrderRefundInTx(ctx, tx, &o, *req.RefundToAddress, req.RefundTicket, req.OperatorNote); err != nil {
			return o, err
		}

	case ports.ReviewCancelRefund:
		if o.Status != types.OrderRefundInProgress {
			return o, fmt.Errorf("%w: CANCEL_REFUND requires REFUND_IN_PROGRESS, got %s",
				ports.ErrInvalidTransition, o.Status)
		}
		if err := s.cancelOrderRefundInTx(ctx, tx, &o); err != nil {
			return o, err
		}

	case ports.ReviewResolveReversal:
		if o.Status != types.OrderSettledReversed {
			return o, fmt.Errorf("%w: RESOLVE_REVERSAL requires SETTLED_REVERSED, got %s",
				ports.ErrInvalidTransition, o.Status)
		}
		if err := s.resolveReversalInTx(ctx, tx, &o); err != nil {
			return o, err
		}

	default:
		return o, fmt.Errorf("%w: unknown review action %q", ports.ErrInvalidTransition, req.Action)
	}

	// 重读终态投影返回.
	out, err := scanOrder(tx.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE id = $1`, o.ID))
	if err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	// FIX-G BUG-2: 人工审核终态 — 提交后广播 (PAID / REFUND_IN_PROGRESS /
	// LATE_PAID_REVIEW / REVERSAL_RESOLVED; api 蛇形映射 OpenAPI 事件名).
	s.publishOrderEvent(ctx, ports.OrderEvent{
		OrderID: out.ID, Type: ports.OrderEventStatusChanged,
		Status: out.Status, ReceivedAmount: out.ReceivedAmount,
	})
	return out, nil
}

// settleSuspendedToPaid 挂起单人工接受履约: 模板 4 第二规则
// DR SUSPENSE_REFUNDABLE_LIAB / CR MERCHANT_AVAILABLE + FEE_REVENUE,
// 金额按实收 received_amount 结算 (fee = received * bps / 10000 向下取整).
func (s *Service) settleSuspendedToPaid(ctx context.Context, tx pgx.Tx, o *types.PaymentOrder) error {
	received := o.ReceivedAmount
	if received.Sign() <= 0 {
		return fmt.Errorf("%w: order %s has no received amount to settle", ports.ErrInvalidTransition, o.ID)
	}
	var feeBps int32
	if err := tx.QueryRow(ctx, `SELECT fee_rate_bps FROM merchants WHERE id = $1`, o.MerchantID).Scan(&feeBps); err != nil {
		return fmt.Errorf("order: read fee_rate_bps: %w", err)
	}
	fee := received.MulBpsFloor(uint64(feeBps))
	net, err := received.Sub(fee)
	if err != nil {
		return err
	}
	if err := transitionOrderStatus(ctx, tx, *o, types.OrderPaid); err != nil {
		return err
	}
	legs := []ports.PostEntryLeg{{
		OwnerMerchantID: o.MerchantID, ChainID: o.ChainID, AssetSymbol: o.AssetSymbol,
		Account: types.AccountSuspenseRefundableLiab, Direction: types.DirectionDebit, Amount: received,
	}}
	if net.Sign() > 0 {
		legs = append(legs, ports.PostEntryLeg{
			OwnerMerchantID: o.MerchantID, ChainID: o.ChainID, AssetSymbol: o.AssetSymbol,
			Account: types.AccountMerchantAvailable, Direction: types.DirectionCredit, Amount: net,
		})
	}
	if fee.Sign() > 0 {
		legs = append(legs, ports.PostEntryLeg{
			OwnerMerchantID: types.SystemMerchantID, ChainID: o.ChainID, AssetSymbol: o.AssetSymbol,
			Account: types.AccountFeeRevenue, Direction: types.DirectionCredit, Amount: fee,
		})
	}
	if _, err := s.ledger.PostEntryInTx(ctx, tx, ports.PostEntryRequest{
		Template:    types.TemplateOrderSettle,
		ReferenceID: types.RefPrefixOrderSettle + o.ID.String(),
		Description: fmt.Sprintf("order %s manual-accept settle received=%s", o.ID, received.String()),
		Legs:        legs,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET settled_at = now() WHERE id = $1`, o.ID); err != nil {
		return err
	}
	o.Status = types.OrderPaid
	return notifyOrderEvent(ctx, tx, *o, EventOrderStatusChanged)
}

// createOrderRefundInTx 全额异常单退款: 建 CUSTOMER_REFUND payout
// (SUSPENSE_REFUNDABLE_LIAB 冻结入 PAYOUT_LOCKED_LIAB) + 订单转
// REFUND_IN_PROGRESS. 退款收款地址由审核方显式核实后传入, 绝不自动退回
// tx.from_address (§9.2). ticket 为离线管理员签署的 127B 票据; 其内嵌
// payout_uuid 即退款单主键 (FIX-D1: PK 与票据绑定, signer 逐字节比对).
func (s *Service) createOrderRefundInTx(ctx context.Context, tx pgx.Tx, o *types.PaymentOrder,
	to types.Address, ticket *ports.RefundTicket, note string) error {

	if o.ReceivedAmount.Sign() <= 0 {
		return fmt.Errorf("%w: order %s has no received amount to refund", ports.ErrInvalidTransition, o.ID)
	}
	payoutID, err := validateNewRefundTicket(ticket)
	if err != nil {
		return err
	}
	// FIX-D1: 票据载荷字段 vs 订单派生字段交叉校验 (chain/token/to/
	// received_amount/expiry) — 与 signer 验签口径一致, 建单即拒错票.
	token, err := s.supportedTokenContract(ctx, o.ChainID, o.AssetSymbol)
	if err != nil {
		return err
	}
	if err := validateTicketFields(ticket, o.ChainID, token, to,
		o.ReceivedAmount, time.Now()); err != nil {
		return err
	}
	// 幂等键派生: 同订单至多一张活跃全额退款单 — 已取消
	// (CANCELLED_UNLOCKED) 的历史单不阻塞再次审核建单; 键并入票据
	// payout_uuid 保证取消后换新票可重新建单, 同票重放仍经 PK/uq
	// 冲突收敛 (signer (payout_uuid,retry_seq) 槽位已消费, 同一 uuid
	// 不得复活, 换票即换 uuid).
	idemKey := "order-refund:" + o.ID.String() + ":" + payoutID.String()
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM merchant_payouts
		WHERE merchant_id = $1 AND source_order_id = $2 AND payout_kind = $3
		  AND status <> 'CANCELLED_UNLOCKED'`,
		o.MerchantID, o.ID, types.PayoutKindCustomerRefund.String()).Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w: refund payout %s already exists for order %s",
			ports.ErrDuplicateReference, existing, o.ID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("order: check refund payout: %w", err)
	}

	h := sha256.Sum256([]byte(fmt.Sprintf("refund|%s|%s|%s|%s",
		o.ID, o.MerchantID, addrHex(to), o.ReceivedAmount.String())))
	if _, err := s.createPayoutLocked(ctx, tx, payoutParams{
		ID:             payoutID,
		MerchantID:     o.MerchantID,
		IdempotencyKey: idemKey,
		RequestHash:    hex.EncodeToString(h[:]),
		Kind:           types.PayoutKindCustomerRefund,
		SourceAccount:  types.AccountSuspenseRefundableLiab,
		SourceOrderID:  &o.ID,
		ChainID:        o.ChainID,
		AssetSymbol:    o.AssetSymbol,
		ToAddress:      to,
		Amount:         o.ReceivedAmount,
		RefundTicket:   ticket,
	}); err != nil {
		if isUniqueViolation(err, "uq_payouts_merchant_idem") ||
			isUniqueViolation(err, "pk_merchant_payouts") {
			return fmt.Errorf("%w: refund ticket %s already consumed for order %s",
				ports.ErrDuplicateReference, payoutID, o.ID)
		}
		return err
	}
	if err := transitionOrderStatus(ctx, tx, *o, types.OrderRefundInProgress); err != nil {
		return err
	}
	o.Status = types.OrderRefundInProgress
	_ = note // OperatorNote 无持久列, 仅体现在 outbox 事件载荷扩展位.
	return notifyOrderEvent(ctx, tx, *o, EventOrderStatusChanged)
}

// cancelOrderRefundInTx 取消订单关联的未成行退款单
// (LOCKED_QUEUED / FAILED_RETRYABLE 可取消; IN_FLIGHT 已广播须走
// GAP_FILLER 取消路径, 返回 ErrInvalidTransition).
func (s *Service) cancelOrderRefundInTx(ctx context.Context, tx pgx.Tx, o *types.PaymentOrder) error {
	var payoutID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM merchant_payouts
		WHERE source_order_id = $1 AND payout_kind = $2
		  AND status IN ('LOCKED_QUEUED','IN_FLIGHT','FAILED_RETRYABLE')
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		o.ID, types.PayoutKindCustomerRefund.String()).Scan(&payoutID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no active refund payout for order %s", ports.ErrNotFound, o.ID)
	}
	if err != nil {
		return fmt.Errorf("order: find refund payout: %w", err)
	}
	// cancelPayoutInTx 负责: 状态白名单校验 + 解冻腿 + 订单退回
	// LATE_PAID_REVIEW (其内部对 REFUND_IN_PROGRESS 订单自动回退;
	// 订单快照弃用 — ReviewOrder 在 commit 后统一广播).
	if _, _, err := s.cancelPayoutInTx(ctx, tx, payoutID); err != nil {
		return err
	}
	o.Status = types.OrderLatePaidReview
	return nil
}

// resolveReversalInTx SETTLED_REVERSED -> REVERSAL_RESOLVED:
// 前置校验 MERCHANT_REORG_RECEIVABLE 该商户该链该币种已清零
// (客户补款或运营核销已在账本完成, 本方法仅做状态闭环).
func (s *Service) resolveReversalInTx(ctx context.Context, tx pgx.Tx, o *types.PaymentOrder) error {
	var outstandingSum []byte
	// NUMERIC 聚合以 text 返回再解析, 与 numericU256 双轨一致 (此处直接查
	// 单账户行, 账户可能不存在 -> COALESCE 0).
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(balance),0)::text FROM ledger_accounts
		WHERE owner_merchant_id = $1 AND chain_id = $2 AND asset_symbol = $3
		  AND account_type = $4`,
		o.MerchantID, int64(o.ChainID), o.AssetSymbol,
		types.AccountMerchantReorgReceivable.String()).Scan(&outstandingSum)
	if err != nil {
		return fmt.Errorf("order: sum reorg receivable: %w", err)
	}
	outstanding, err := types.ParseU256(string(outstandingSum))
	if err != nil {
		return fmt.Errorf("order: parse receivable: %w", err)
	}
	if outstanding.Sign() != 0 {
		return fmt.Errorf("%w: order %s reorg receivable %s outstanding",
			ports.ErrInvalidTransition, o.ID, outstanding.String())
	}
	if err := transitionOrderStatus(ctx, tx, *o, types.OrderReversalResolved); err != nil {
		return err
	}
	o.Status = types.OrderReversalResolved
	return notifyOrderEvent(ctx, tx, *o, EventOrderStatusChanged)
}

// ---------------------------------------------------------------------------
// ReevaluateExpiredOrders — 周期性过期重评器 (§5.2 周期重评边)
// ---------------------------------------------------------------------------

// ReevaluateExpiredOrders 扫描超期订单并按 §5.2 周期重评:
//   - AWAITING_PAYMENT + 过期 + 无同币种在途(DETECTED)充值 -> EXPIRED;
//   - PARTIALLY_PAID + 过期 + 无同币种在途充值 -> UNDERPAID_EXPIRED_REVIEW,
//     同事务走模板 5 (DR ORDER_ESCROW_LIAB / CR SUSPENSE_REFUNDABLE_LIAB)
//     将已收 escrow 转挂待退负债;
//   - 存在在途 DETECTED 充值的订单跳过, 待其 FINALIZED 后由入站分支 D
//     走 LATE_PAID_REVIEW 路径.
//
// 每订单独立 PG 事务 (批内失败不互相影响); 返回成功迁移的订单数.
func (s *Service) ReevaluateExpiredOrders(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM payment_orders
		WHERE status IN ('AWAITING_PAYMENT','PARTIALLY_PAID') AND expires_at <= $1
		ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("order: scan expired: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	migrated := 0
	for _, id := range ids {
		ok, err := s.reevaluateOne(ctx, id, now)
		if err != nil {
			return migrated, err
		}
		if ok {
			migrated++
		}
	}
	return migrated, nil
}

// reevaluateOne 单订单过期重评 (独立事务, FOR UPDATE 行锁后二次校验).
func (s *Service) reevaluateOne(ctx context.Context, orderID uuid.UUID, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := s.lockOrder(ctx, tx, orderID)
	if err != nil {
		return false, err
	}
	if (o.Status != types.OrderAwaitingPayment && o.Status != types.OrderPartiallyPaid) ||
		o.ExpiresAt.After(now) {
		return false, nil // 已被并发路径处理或尚未过期
	}

	// 同币种在途充值: to_address == 订单专属地址 且 token_contract ==
	// 订单币种白名单合约 且状态 DETECTED.
	var inflight int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM onchain_deposits d
		JOIN deposit_addresses da ON da.id = $1
		JOIN supported_assets sa ON sa.chain_id = $2 AND sa.asset_symbol = $3
		WHERE d.chain_id = $2 AND d.to_address = da.address
		  AND d.token_contract = sa.token_contract AND d.status = 'DETECTED'`,
		o.DepositAddressID, int64(o.ChainID), o.AssetSymbol).Scan(&inflight); err != nil {
		return false, fmt.Errorf("order: count inflight deposits: %w", err)
	}
	if inflight > 0 {
		return false, nil // 有在途: 等待 FINALIZED (分支 D 走 LATE_PAID_REVIEW)
	}

	switch o.Status {
	case types.OrderAwaitingPayment:
		if err := transitionOrderStatus(ctx, tx, o, types.OrderExpired); err != nil {
			return false, err
		}
		o.Status = types.OrderExpired
	case types.OrderPartiallyPaid:
		if err := transitionOrderStatus(ctx, tx, o, types.OrderUnderpaidExpiredReview); err != nil {
			return false, err
		}
		// 模板 5: DR ORDER_ESCROW_LIAB / CR SUSPENSE_REFUNDABLE_LIAB
		// (金额 = 已累计同币种有效入账).
		if _, err := s.ledger.PostEntryInTx(ctx, tx, ports.PostEntryRequest{
			Template:    types.TemplateOrderSuspense,
			ReferenceID: "order_suspense:" + o.ID.String(),
			Description: fmt.Sprintf("order %s underpaid expired, escrow %s -> refundable suspense", o.ID, o.ReceivedAmount.String()),
			Legs: []ports.PostEntryLeg{
				{
					OwnerMerchantID: o.MerchantID, ChainID: o.ChainID, AssetSymbol: o.AssetSymbol,
					Account: types.AccountOrderEscrowLiab, Direction: types.DirectionDebit, Amount: o.ReceivedAmount,
				},
				{
					OwnerMerchantID: o.MerchantID, ChainID: o.ChainID, AssetSymbol: o.AssetSymbol,
					Account: types.AccountSuspenseRefundableLiab, Direction: types.DirectionCredit, Amount: o.ReceivedAmount,
				},
			},
		}); err != nil {
			return false, err
		}
		o.Status = types.OrderUnderpaidExpiredReview
	}
	if err := notifyOrderEvent(ctx, tx, o, EventOrderExpired); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	// FIX-G BUG-2: 过期跃迁 — 提交后广播 expired / underpaid_expired_review.
	et := ports.OrderEventStatusChanged
	if o.Status == types.OrderExpired {
		et = ports.OrderEventExpired
	}
	s.publishOrderEvent(ctx, ports.OrderEvent{
		OrderID: o.ID, Type: et, Status: o.Status,
		ReceivedAmount: o.ReceivedAmount,
	})
	return true, nil
}

// ---------------------------------------------------------------------------
// W4 出站结算联动钩子 (§5.4 状态机 + §6.2 模板 8)
// 以下方法由 W4 indexer 在出站 Intent 生命周期事件落账时调用
// (Wave 2 装配; 同属订单/账本域故由本窗口实现).
// ---------------------------------------------------------------------------

// OnPayoutInFlight 出站 Intent 已广播: LOCKED_QUEUED -> IN_FLIGHT.
func (s *Service) OnPayoutInFlight(ctx context.Context, payoutID uuid.UUID) error {
	return s.payoutSimpleTransition(ctx, payoutID, types.PayoutInFlight)
}

// OnPayoutRetry FAILED_RETRYABLE 重发: retry_seq++ 且 -> IN_FLIGHT.
func (s *Service) OnPayoutRetry(ctx context.Context, payoutID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := s.lockPayout(ctx, tx, payoutID)
	if err != nil {
		return err
	}
	if p.Status != types.PayoutFailedRetryable {
		return fmt.Errorf("%w: OnPayoutRetry requires FAILED_RETRYABLE, got %s",
			ports.ErrInvalidTransition, p.Status)
	}
	if err := transitionPayoutStatus(ctx, tx, p, types.PayoutInFlight); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE merchant_payouts SET retry_seq = retry_seq + 1 WHERE id = $1`, p.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OnPayoutFailed 出站 Intent FAILED_REVERTED / CANCELLED_BY_FILLER:
// IN_FLIGHT -> FAILED_RETRYABLE (余额保持锁定待重排).
func (s *Service) OnPayoutFailed(ctx context.Context, payoutID uuid.UUID) error {
	return s.payoutSimpleTransition(ctx, payoutID, types.PayoutFailedRetryable)
}

// OnPayoutConfirmed 出站 Intent CONFIRMED: IN_FLIGHT -> COMPLETED,
// 同事务记 PAYOUT_FLOW 成功腿 (DR PAYOUT_LOCKED_LIAB / CR CUSTODY_HOT_PAYOUT),
// 并联动订单状态:
//   - 订单全额退款 (REFUND_IN_PROGRESS) -> REFUNDED;
//   - OVERPAID 溢缴退款单完结 -> PAID.
func (s *Service) OnPayoutConfirmed(ctx context.Context, payoutID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := s.lockPayout(ctx, tx, payoutID)
	if err != nil {
		return err
	}
	if err := transitionPayoutStatus(ctx, tx, p, types.PayoutCompleted); err != nil {
		return err
	}
	// 成功腿: DR PAYOUT_LOCKED_LIAB / CR CUSTODY_HOT_PAYOUT (模板 8 第二腿,
	// CUSTODY_HOT_PAYOUT 为系统科目, owner = SystemMerchantID).
	if _, err := s.ledger.PostEntryInTx(ctx, tx, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "payout_settle:" + p.ID.String(),
		Description: fmt.Sprintf("payout %s confirmed, custodial release %s %s", p.ID, p.Amount.String(), p.AssetSymbol),
		Legs: []ports.PostEntryLeg{
			{
				OwnerMerchantID: p.MerchantID, ChainID: p.ChainID, AssetSymbol: p.AssetSymbol,
				Account: types.AccountPayoutLockedLiab, Direction: types.DirectionDebit, Amount: p.Amount,
			},
			{
				OwnerMerchantID: types.SystemMerchantID, ChainID: p.ChainID, AssetSymbol: p.AssetSymbol,
				Account: types.AccountCustodyHotPayout, Direction: types.DirectionCredit, Amount: p.Amount,
			},
		},
	}); err != nil {
		return err
	}
	// 订单联动.
	var oChanged *types.PaymentOrder // FIX-G BUG-2: 提交后 SSE 广播快照
	if p.SourceOrderID != nil && p.PayoutKind == types.PayoutKindCustomerRefund {
		o, lerr := s.lockOrder(ctx, tx, *p.SourceOrderID)
		if lerr != nil {
			return lerr
		}
		switch o.Status {
		case types.OrderRefundInProgress:
			if err := transitionOrderStatus(ctx, tx, o, types.OrderRefunded); err != nil {
				return err
			}
			o.Status = types.OrderRefunded
			if err := notifyOrderEvent(ctx, tx, o, EventOrderStatusChanged); err != nil {
				return err
			}
			snap := o
			oChanged = &snap
		case types.OrderOverpaid:
			// 溢缴差额 CUSTOMER_REFUND 完结 -> PAID (§5.2).
			if err := transitionOrderStatus(ctx, tx, o, types.OrderPaid); err != nil {
				return err
			}
			o.Status = types.OrderPaid
			if err := notifyOrderEvent(ctx, tx, o, EventOrderStatusChanged); err != nil {
				return err
			}
			snap := o
			oChanged = &snap
		}
	}
	p.Status = types.PayoutCompleted
	payload, _ := json.Marshal(map[string]any{
		"payout_id": p.ID.String(), "merchant_id": p.MerchantID.String(),
		"status": p.Status.String(), "amount": p.Amount.String(),
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO webhook_outbox (merchant_id, event_type, payload_json) VALUES ($1,$2,$3)`,
		p.MerchantID, EventPayoutCompleted, string(payload)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit: %v", ports.ErrUnavailable, err)
	}
	if oChanged != nil {
		// FIX-G BUG-2: refunded / paid 终态帧 — 提交后广播.
		s.publishOrderEvent(ctx, ports.OrderEvent{
			OrderID: oChanged.ID, Type: ports.OrderEventStatusChanged,
			Status: oChanged.Status, ReceivedAmount: oChanged.ReceivedAmount,
		})
	}
	return nil
}

func (s *Service) payoutSimpleTransition(ctx context.Context, payoutID uuid.UUID, to types.PayoutStatus) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin tx: %v", ports.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := s.lockPayout(ctx, tx, payoutID)
	if err != nil {
		return err
	}
	if err := transitionPayoutStatus(ctx, tx, p, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

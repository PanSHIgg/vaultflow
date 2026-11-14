package indexer

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// §4.3 统一 LIFO 逆序回滚: 父哈希断裂 -> 分叉点 -> latest_synced 逐块逆退
// ---------------------------------------------------------------------------

// findForkPoint 自 latest_synced 逐块下钻, 定位本地主链与远端多数哈希一致的
// 分叉点高度; 超过 MaxForkDepth 报 CRITICAL (异常深分叉, 人工介入).
func (ix *Indexer) findForkPoint(ctx context.Context, cur types.ChainSyncCursor) (uint64, error) {
	for h := cur.LatestSyncedHeight; ; h-- {
		var local string
		err := ix.db.QueryRow(ctx,
			`SELECT block_hash FROM chain_blocks
			  WHERE chain_id=$1 AND block_number=$2 AND status='CANONICAL'`,
			int64(ix.cfg.ChainID), int64(h)).Scan(&local)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		remote, rerr := ix.chain.HeaderAt(ctx, h)
		if rerr != nil {
			return 0, fmt.Errorf("indexer: header at %d during fork scan: %w", h, rerr)
		}
		if remote != nil && errors.Is(err, pgx.ErrNoRows) == false &&
			hashStr(remote.Hash) == local {
			return h, nil
		}
		if cur.LatestSyncedHeight-h >= uint64(ix.cfg.MaxForkDepth) || h == 0 {
			ix.raise(Alert{
				Severity: "CRITICAL", Code: "FORK_DEPTH_EXCEEDED",
				Message: fmt.Sprintf("no common ancestor within %d blocks — manual intervention", ix.cfg.MaxForkDepth),
				Detail:  fmt.Sprintf("chain=%d latest_synced=%d", ix.cfg.ChainID, cur.LatestSyncedHeight),
			})
			return 0, fmt.Errorf("indexer: fork point not found within depth bound (chain %d)", ix.cfg.ChainID)
		}
	}
}

// rollbackToFork 将 (fork, latest] 区间逐块 LIFO 逆退, 游标回退至分叉点,
// 并重广播全部受影响意图.
func (ix *Indexer) rollbackToFork(ctx context.Context, cur types.ChainSyncCursor, fork uint64) error {
	ix.raise(Alert{
		Severity: "WARNING", Code: "REORG_DETECTED",
		Message: fmt.Sprintf("reorg: rolling back heights (%d, %d] LIFO",
			fork, cur.LatestSyncedHeight),
		Detail: fmt.Sprintf("chain=%d", ix.cfg.ChainID),
	})

	intentSet := map[uuid.UUID]bool{}
	for h := cur.LatestSyncedHeight; h > fork; h-- {
		intents, err := ix.rollbackBlock(ctx, h)
		if err != nil {
			return fmt.Errorf("rollback block %d: %w", h, err)
		}
		for _, id := range intents {
			intentSet[id] = true
		}
	}

	// 游标回退到分叉点 (其块仍 CANONICAL).
	var forkHash string
	if err := ix.db.QueryRow(ctx,
		`SELECT block_hash FROM chain_blocks
		  WHERE chain_id=$1 AND block_number=$2 AND status='CANONICAL'`,
		int64(ix.cfg.ChainID), int64(fork)).Scan(&forkHash); err != nil {
		return err
	}
	if _, err := ix.db.Exec(ctx,
		`UPDATE chain_sync_cursors SET latest_synced_height=$2, latest_synced_hash=$3, updated_at=now()
		 WHERE chain_id=$1`, int64(ix.cfg.ChainID), int64(fork), forkHash); err != nil {
		return err
	}

	// 重置态意图重广播 (签名确定性 → 同 tx_hash 平滑重入).
	if ix.rebc != nil {
		for id := range intentSet {
			if err := ix.rebc.RebroadcastIntent(ctx, id); err != nil {
				ix.log.Warn("post-rollback rebroadcast failed",
					zap.String("intent", id.String()), zap.Error(err))
			}
		}
	}
	return nil
}

// rollbackBlock 单块 LIFO 逆退 (块 ORPHANED + 出站重置/红冲 + 入站孤块化),
// 返回需要重广播的意图集合.
func (ix *Indexer) rollbackBlock(ctx context.Context, h uint64) ([]uuid.UUID, error) {
	var blkHash string
	err := ix.db.QueryRow(ctx,
		`SELECT block_hash FROM chain_blocks
		  WHERE chain_id=$1 AND block_number=$2 AND status='CANONICAL'`,
		int64(ix.cfg.ChainID), int64(h)).Scan(&blkHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // 高度块未索引 (防御)
	}
	if err != nil {
		return nil, err
	}

	var intents []uuid.UUID
	var after []func() // FIX-G BUG-2: 提交后 SSE 发布器 (回滚产生的新状态)
	err = pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE chain_blocks SET status='ORPHANED'
			 WHERE chain_id=$1 AND block_hash=$2 AND status='CANONICAL'`,
			int64(ix.cfg.ChainID), blkHash); err != nil {
			return err
		}
		is, err := ix.rollbackOutbound(ctx, tx, blkHash, &after)
		if err != nil {
			return err
		}
		intents = append(intents, is...)
		return ix.rollbackInbound(ctx, tx, blkHash, &after)
	})
	if err == nil {
		for _, fn := range after {
			fn()
		}
	}
	return intents, err
}

// ---------------------------------------------------------------------------
// 出站回滚: 浅退清块字段 / 深退先红冲再重置; SUPERSEDED 兄弟一并复活;
// 意图退回 IN_FLIGHT 交由 TxManager 重广播 — 同 tx_hash 可平滑重入.
// ---------------------------------------------------------------------------

func (ix *Indexer) rollbackOutbound(ctx context.Context, tx pgx.Tx,
	blkHash string, after *[]func()) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT a.id, a.intent_id, a.status, a.attempt_kind, a.receipt_status,
		        a.gas_used, a.effective_gas_price::text,
		        i.intent_type, i.reference_id
		   FROM outbound_tx_attempts a JOIN outbound_tx_intents i ON i.id=a.intent_id
		  WHERE a.chain_id=$1 AND a.block_hash=$2
		    AND a.status IN ('INCLUDED_UNSAFE','FINALIZED')`,
		int64(ix.cfg.ChainID), blkHash)
	if err != nil {
		return nil, err
	}
	type row struct {
		attID, intentID uuid.UUID
		status          string
		kind            string
		receipt         *int32
		gasUsed         *int64
		egp             *string
		intentType      string
		referenceID     uuid.UUID
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.attID, &r.intentID, &r.status, &r.kind, &r.receipt,
			&r.gasUsed, &r.egp, &r.intentType, &r.referenceID); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()

	var intents []uuid.UUID
	for _, r := range list {
		if r.status == string(types.AttemptFinalized) {
			if err := ix.reverseAttempt(ctx, tx, r.attID, r.intentID,
				types.IntentType(r.intentType), types.AttemptKind(r.kind),
				r.referenceID, r.receipt, r.gasUsed, r.egp, after); err != nil {
				return nil, err
			}
		}
		// 本候选重置 BROADCASTED (终态 FINALIZED 亦放行 — §4.3 特例).
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts
			   SET status='BROADCASTED', block_number=NULL, block_hash=NULL,
			       receipt_status=NULL, gas_used=NULL, effective_gas_price=NULL
			 WHERE id=$1 AND status IN ('INCLUDED_UNSAFE','FINALIZED')`, r.attID); err != nil {
			return nil, err
		}
		// 同 Nonce SUPERSEDED 兄弟复活 (DROPPED 永不复活).
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts SET status='BROADCASTED',
			       block_number=NULL, block_hash=NULL, receipt_status=NULL,
			       gas_used=NULL, effective_gas_price=NULL
			 WHERE intent_id=$1 AND status='SUPERSEDED'`, r.intentID); err != nil {
			return nil, err
		}
		// 意图退回 IN_FLIGHT (全部已结算态均可退回 — §5.3 白名单出边).
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_intents SET status='IN_FLIGHT'
			 WHERE id=$1 AND status IN
			   ('IN_FLIGHT','INCLUDED_PENDING_SAFE','CONFIRMED','FAILED_REVERTED','CANCELLED_BY_FILLER')`,
			r.intentID); err != nil {
			return nil, err
		}
		intents = append(intents, r.intentID)
	}
	return intents, nil
}

// reverseAttempt 已结算出站候选的红冲: Gas (必记) + 业务分录 (成功路径).
// after 为提交后 SSE 发布器收集槽 (FIX-G BUG-2).
func (ix *Indexer) reverseAttempt(ctx context.Context, tx pgx.Tx,
	attID, intentID uuid.UUID, intentType types.IntentType, kind types.AttemptKind,
	referenceID uuid.UUID, receipt *int32, gasUsed *int64, egp *string,
	after *[]func()) error {

	sys := types.SystemMerchantID
	rn, err := ix.cycleOf(ctx, "reorggas:"+attID.String())
	if err != nil {
		return err
	}
	// ① Gas 红冲: DR HOT_WALLET_NATIVE / CR GAS_EXPENSE (模板 9 规则 1).
	if gasUsed != nil && egp != nil {
		gasAmt := new(big.Int).Mul(types.MustParseU256(*egp).Big(), big.NewInt(*gasUsed))
		if gasAmt.Sign() > 0 {
			if err := ix.post(ctx, types.TemplateReorgReversal,
				fmt.Sprintf("reorggas:%s:r%d", attID, rn),
				"reorg gas reversal attempt "+attID.String(), []ports.PostEntryLeg{
					leg(sys, ix.cfg.ChainID, "ETH", types.AccountHotWalletNative, types.DirectionDebit, gasAmt),
					leg(sys, ix.cfg.ChainID, "ETH", types.AccountGasExpense, types.DirectionCredit, gasAmt),
				}); err != nil {
				return err
			}
		}
	}

	reverted := receipt == nil || *receipt != 1

	switch {
	case intentType == types.IntentTypeSweepBatch:
		// 批次内已 SWEPT 条目逐条红冲 (Revert 批次无记账, 仅状态还原).
		return ix.reverseSweepBatch(ctx, tx, attID, referenceID, rn, reverted)
	case intentType == types.IntentTypeMerchantPayout:
		if !reverted && kind == types.AttemptKindPrimary {
			return ix.reversePayoutExec(ctx, tx, attID, referenceID, rn, after)
		}
		// Revert 结算或 Filler 取消: payout FAILED_RETRYABLE -> IN_FLIGHT.
		_, err := tx.Exec(ctx,
			`UPDATE merchant_payouts SET status='IN_FLIGHT'
			 WHERE id=$1 AND status='FAILED_RETRYABLE'`, referenceID)
		return err
	case kind == types.AttemptKindGapFiller && !reverted:
		// Filler 成功取消结算被回滚: 业务 payout 恢复在途.
		if intentType == types.IntentTypeMerchantPayout {
			_, err := tx.Exec(ctx,
				`UPDATE merchant_payouts SET status='IN_FLIGHT'
				 WHERE id=$1 AND status='FAILED_RETRYABLE'`, referenceID)
			return err
		}
	}
	return nil
}

// reverseSweepBatch 归集成功结算的红冲: 每已记条目 DR UNSWEPT / CR COLD.
func (ix *Indexer) reverseSweepBatch(ctx context.Context, tx pgx.Tx,
	attID, batchID uuid.UUID, rn int, reverted bool) error {

	sys := types.SystemMerchantID
	var asset string
	if err := tx.QueryRow(ctx,
		`SELECT asset_symbol FROM sweep_batches WHERE id=$1 FOR UPDATE`, batchID).
		Scan(&asset); err != nil {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT i.id, i.deposit_address_id, i.item_status,
		        COALESCE(i.actual_swept_amount, i.exact_sweep_amount)::text
		   FROM sweep_batch_items i
		  WHERE i.batch_id=$1 AND i.item_status IN ('SWEPT','QUARANTINED_REVERT')`,
		batchID)
	if err != nil {
		return err
	}
	type item struct {
		id, daID uuid.UUID
		status   string
		amount   string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.daID, &it.status, &it.amount); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()

	for _, it := range items {
		if it.status == "SWEPT" {
			amt := types.MustParseU256(it.amount).Big()
			if err := ix.post(ctx, types.TemplateReorgReversal,
				fmt.Sprintf("reorgsweepitem:%s:%s:r%d", it.id, attID, rn),
				"reorg sweep reversal item "+it.id.String(), []ports.PostEntryLeg{
					leg(sys, ix.cfg.ChainID, asset, types.AccountCustodyUnswept, types.DirectionDebit, amt),
					leg(sys, ix.cfg.ChainID, asset, types.AccountCustodyCold, types.DirectionCredit, amt),
				}); err != nil {
				return err
			}
		} else {
			// 隔离标记随块回滚一并还原.
			if _, err := tx.Exec(ctx,
				`UPDATE deposit_addresses SET is_quarantined=FALSE WHERE id=$1`, it.daID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sweep_batch_items SET item_status='PENDING', actual_swept_amount=NULL
			 WHERE id=$1`, it.id); err != nil {
			return err
		}
	}
	// 批次恢复在途, 待重广播重新归集.
	_, err = tx.Exec(ctx,
		`UPDATE sweep_batches SET status='IN_FLIGHT', total_swept_amount=0
		 WHERE id=$1 AND status IN ('COMPLETED','PARTIAL_QUARANTINED','FAILED')`, batchID)
	return err
}

// reversePayoutExec 提现执行腿红冲: DR CUSTODY_HOT_PAYOUT / CR PAYOUT_LOCKED_LIAB.
// after 为提交后 SSE 发布器收集槽 (FIX-G BUG-2).
func (ix *Indexer) reversePayoutExec(ctx context.Context, tx pgx.Tx,
	attID, payoutID uuid.UUID, rn int, after *[]func()) error {
	var p types.MerchantPayout
	var amount string
	err := tx.QueryRow(ctx,
		`SELECT merchant_id, asset_symbol, amount::text, status,
		        payout_kind, source_order_id
		   FROM merchant_payouts WHERE id=$1 FOR UPDATE`, payoutID).
		Scan(&p.MerchantID, &p.AssetSymbol, &amount, &p.Status,
			&p.PayoutKind, &p.SourceOrderID)
	if err != nil {
		return err
	}
	amt := types.MustParseU256(amount).Big()
	sys := types.SystemMerchantID
	if err := ix.post(ctx, types.TemplateReorgReversal,
		fmt.Sprintf("reorgpayoutexec:%s:r%d", attID, rn),
		"reorg payout exec reversal "+payoutID.String(), []ports.PostEntryLeg{
			leg(sys, ix.cfg.ChainID, p.AssetSymbol, types.AccountCustodyHotPayout, types.DirectionDebit, amt),
			leg(p.MerchantID, ix.cfg.ChainID, p.AssetSymbol, types.AccountPayoutLockedLiab, types.DirectionCredit, amt),
		}); err != nil {
		return err
	}
	// payout COMPLETED -> IN_FLIGHT (白名单出边); FAILED_RETRYABLE -> IN_FLIGHT.
	if _, err := tx.Exec(ctx,
		`UPDATE merchant_payouts SET status='IN_FLIGHT'
		 WHERE id=$1 AND status IN ('COMPLETED','FAILED_RETRYABLE')`, payoutID); err != nil {
		return err
	}
	// 退款完结订单退回退款中 (无白名单迁移 — 深回滚特许直改, 记审计告警).
	if p.PayoutKind == types.PayoutKindCustomerRefund && p.SourceOrderID != nil {
		var recvStr string
		err := tx.QueryRow(ctx,
			`UPDATE payment_orders SET status='REFUND_IN_PROGRESS'
			 WHERE id=$1 AND status='REFUNDED'
			 RETURNING received_amount::text`, *p.SourceOrderID).Scan(&recvStr)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			ix.raise(Alert{Severity: "WARNING", Code: "REFUNDED_ORDER_REOPENED",
				Message: "deep reorg reopened a REFUNDED order",
				Detail:  fmt.Sprintf("order=%s payout=%s", p.SourceOrderID, payoutID)})
			// FIX-G BUG-2: 广播回滚后的真实新状态 REFUND_IN_PROGRESS
			// (绝不发被回滚掉的 REFUNDED — 只发提交后仍然成立的状态).
			recv, oid := types.MustParseU256(recvStr), *p.SourceOrderID
			*after = append(*after, func() {
				ix.publishOrderEvent(ctx, ports.OrderEvent{
					OrderID: oid, Type: ports.OrderEventStatusChanged,
					Status: types.OrderRefundInProgress, ReceivedAmount: recv,
					OccurredAt: ix.now()})
			})
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 入站回滚: DETECTED->ORPHANED_SHALLOW / FINALIZED->ORPHANED_DEEP + 红冲.
// LIFO 序保证晚块归集已先冲回, CUSTODY_UNSWEPT 余额必然充足.
// ---------------------------------------------------------------------------

func (ix *Indexer) rollbackInbound(ctx context.Context, tx pgx.Tx,
	blkHash string, after *[]func()) error {
	rows, err := tx.Query(ctx,
		`SELECT id, status FROM onchain_deposits
		  WHERE chain_id=$1 AND block_hash=$2
		    AND status IN ('DETECTED','FINALIZED')`,
		int64(ix.cfg.ChainID), blkHash)
	if err != nil {
		return err
	}
	type dr struct {
		id     uuid.UUID
		status string
	}
	var deps []dr
	for rows.Next() {
		var d dr
		if err := rows.Scan(&d.id, &d.status); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, d)
	}
	rows.Close()

	for _, d := range deps {
		if d.status == string(types.DepositDetected) {
			if _, err := tx.Exec(ctx,
				`UPDATE onchain_deposits SET status='ORPHANED_SHALLOW' WHERE id=$1`, d.id); err != nil {
				return err
			}
			continue
		}
		if err := ix.reverseDeposit(ctx, tx, d.id, after); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE onchain_deposits SET status='ORPHANED_DEEP' WHERE id=$1`, d.id); err != nil {
			return err
		}
	}
	return nil
}

// reverseDeposit 已入账充值的深回滚红冲.
//   - 已结单 (PAID/OVERPAID): 整单 unwind (MA+FEE+OVR/RECV -> UNSWEPT) +
//     存活 FINALIZED 同单充值按原挂账科目重建 + 订单 SETTLED_REVERSED;
//   - 未结单: 按其 dep 分录的实际挂账科目反向 (ESCROW/SUSPENSE_* -> UNSWEPT);
//   - 分支 A (TOPUP/REBALANCE): 模板 9 无合法腿 — 记 CRITICAL 异常人工处置.
//
// after 为提交后 SSE 发布器收集槽 (FIX-G BUG-2).
func (ix *Indexer) reverseDeposit(ctx context.Context, tx pgx.Tx,
	depID uuid.UUID, after *[]func()) error {
	dep, err := ix.loadDeposit(ctx, depID)
	if err != nil {
		return err
	}
	rn, err := ix.cycleOf(ctx, "reorgdep:"+depID.String())
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("reorgdep:%s:r%d", depID, rn)
	sys := types.SystemMerchantID

	if dep.category == types.DepositCategoryTopup ||
		dep.category == types.DepositCategoryTreasuryRebalance {
		ix.raise(Alert{
			Severity: "CRITICAL", Code: "TREASURY_DEP_REORGED",
			Message: "finalized treasury deposit orphaned by deep reorg — no template-9 leg exists, manual reconciliation required",
			Detail:  fmt.Sprintf("deposit=%s category=%s tx=%s", depID, dep.category, dep.txHash.Hex()),
		})
		return nil
	}

	sym, err := ix.tokenSymbol(ctx, dep.tokenContract)
	if err != nil {
		return err
	}

	// 加载订单 (分支 B/C/D 必有 matched_order_id; 防御 nil).
	var oi orderInfo
	haveOrder := dep.matchedOrder != nil
	if haveOrder {
		if err := ix.loadOrderForUpdate(ctx, tx, *dep.matchedOrder, &oi); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				haveOrder = false
			} else {
				return err
			}
		}
	}

	settled := haveOrder &&
		(dep.category == types.DepositCategoryOrderMatch) &&
		(oi.status == types.OrderPaid || oi.status == types.OrderOverpaid ||
			oi.status == types.OrderRefunded || oi.status == types.OrderReversalResolved)

	if settled {
		return ix.unwindSettledOrder(ctx, tx, dep, &oi, sym, ref, after)
	}

	// 通用路径: 读 dep 分录实际挂账科目, 反向冲回.
	legs, err := ix.legsOfLatest(ctx, types.TemplateDepositFinalized, "dep:"+depID.String()+"%")
	if err != nil {
		return fmt.Errorf("locate dep posting for %s: %w", depID, err)
	}
	var creditAcct types.LedgerAccountCode
	for _, l := range legs {
		if l.Direction == types.DirectionCredit {
			creditAcct = l.Account
		}
	}
	if creditAcct == "" {
		return fmt.Errorf("indexer: no credit leg found for deposit %s", depID)
	}
	owner := sys
	if haveOrder {
		owner = oi.merchantID
	}
	if err := ix.post(ctx, types.TemplateReorgReversal, ref,
		"reorg inbound reversal dep "+depID.String(), []ports.PostEntryLeg{
			leg(owner, ix.cfg.ChainID, sym, creditAcct, types.DirectionDebit, dep.amount),
			leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyUnswept, types.DirectionCredit, dep.amount),
		}); err != nil {
		return err
	}

	if haveOrder && dep.category == types.DepositCategoryOrderMatch {
		prevStatus := oi.status
		newReceived := new(big.Int).Sub(oi.receivedAmount.Big(), dep.amount)
		if newReceived.Sign() < 0 {
			newReceived.SetInt64(0)
		}
		// 白名单内重估订单状态 (残余/归零).
		target := oi.status
		switch oi.status {
		case types.OrderPartiallyPaid:
			if newReceived.Sign() == 0 {
				target = types.OrderAwaitingPayment
			}
		case types.OrderLatePaidReview:
			if newReceived.Sign() == 0 {
				target = types.OrderExpired
			} else {
				target = types.OrderUnderpaidExpiredReview
			}
		case types.OrderUnderpaidExpiredReview:
			if newReceived.Sign() == 0 {
				target = types.OrderExpired
			}
		}
		if err := ix.bumpOrder(ctx, tx, &oi, newReceived, target); err != nil {
			return err
		}
		if oi.status != prevStatus {
			// FIX-G BUG-2: 回滚后重估的新状态 — 提交后广播
			// (EXPIRED/AWAITING_PAYMENT/UNDERPAID_EXPIRED_REVIEW 等).
			st, recv, oid := oi.status, oi.receivedAmount, oi.orderID
			*after = append(*after, func() {
				ix.publishOrderEvent(ctx, ports.OrderEvent{
					OrderID: oid, Type: ports.OrderEventStatusChanged,
					Status: st, ReceivedAmount: recv, OccurredAt: ix.now()})
			})
		}
	}
	if haveOrder {
		return emitOutbox(ctx, tx, oi.merchantID, "DEPOSIT_ORPHANED_DEEP", map[string]any{
			"event":      "DEPOSIT_ORPHANED_DEEP",
			"deposit_id": depID.String(),
			"order_id":   oi.orderID.String(),
			"tx_hash":    dep.txHash.Hex(),
		})
	}
	return nil
}

// unwindSettledOrder 已结单整单红冲 (§4.3 + 模板 9 规则 4/5):
//
//	DR MERCHANT_AVAILABLE(credited — 锁内钳制, 不足额自动拆
//	  MERCHANT_REORG_RECEIVABLE + payout_frozen, A2/A6) +
//	  FEE_REVENUE(credited) + SUSPENSE_OVERPAY_LIAB(credited)
//	/ CR CUSTODY_UNSWEPT (按原 settle 贷方逐项);
//
// 存活 FINALIZED 同单充值按各自原挂账科目重建 (DEPOSIT_FINALIZED 重记),
// 订单 -> SETTLED_REVERSED + received=存活同币种充值和.
// after 为提交后 SSE 发布器收集槽 (FIX-G BUG-2).
func (ix *Indexer) unwindSettledOrder(ctx context.Context, tx pgx.Tx, dep *depFull,
	oi *orderInfo, sym, ref string, after *[]func()) error {
	sys := types.SystemMerchantID

	settleLegs, err := ix.legsOfLatest(ctx, types.TemplateOrderSettle,
		types.RefPrefixOrderSettle+oi.orderID.String())
	if err != nil {
		// FIX-E/E1 防御: 已结单按规范前缀找不到 ORDER_SETTLE 分录时,
		// 必须区分两种根因再决策 — 绝不能把数据不一致静默混为一谈:
		//   (a) 前缀漂移 — 历史人工审核路径曾以其他前缀 (order_settle:)
		//       写入; 按 %<order-uuid> 宽匹配找回并按其红冲, 同时
		//       CRITICAL 告警要求运维把行迁回规范前缀;
		//   (b) 结算腿根本缺失 — 订单状态与账本背离 (从未记账/被删),
		//       无法构造红冲: CRITICAL 告警 + 硬错误阻断回滚待人工取证.
		var drift int
		if e2 := ix.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM ledger_transactions
			  WHERE reference_type=$1 AND reference_id LIKE $2`,
			string(types.TemplateOrderSettle), "%"+oi.orderID.String()).Scan(&drift); e2 == nil && drift > 0 {
			ix.raise(Alert{
				Severity: "CRITICAL",
				Code:     "SETTLE_REF_PREFIX_DRIFT",
				Message: "settled order's ORDER_SETTLE legs found only under a non-canonical " +
					"reference prefix — unwinding by suffix match; migrate rows to RefPrefixOrderSettle",
				Detail: fmt.Sprintf("order=%s merchant=%s drifted_rows=%d",
					oi.orderID, oi.merchantID, drift),
			})
			settleLegs, err = ix.legsOfLatest(ctx, types.TemplateOrderSettle,
				"%"+oi.orderID.String())
		}
		if err != nil {
			ix.raise(Alert{
				Severity: "CRITICAL",
				Code:     "SETTLED_ORDER_NO_LEGS",
				Message: "order is settled but no ORDER_SETTLE ledger legs exist — " +
					"books/rows inconsistent, blocking deep rollback for manual forensics",
				Detail: fmt.Sprintf("order=%s merchant=%s dep=%s: %v",
					oi.orderID, oi.merchantID, dep.id, err),
			})
			return fmt.Errorf("locate settle for order %s: %w", oi.orderID, err)
		}
	}
	credited := map[types.LedgerAccountCode]*big.Int{}
	for _, l := range settleLegs {
		if l.Direction == types.DirectionCredit {
			if credited[l.Account] == nil {
				credited[l.Account] = new(big.Int)
			}
			credited[l.Account].Add(credited[l.Account], l.Amount)
		}
	}

	var legs []ports.PostEntryLeg
	maCr := credited[types.AccountMerchantAvailable]
	if maCr != nil && maCr.Sign() > 0 {
		// A6: 快照 dr 核算下沉到 ledger 行锁内 — 直接发全额冲回腿,
		// MA 实际可扣额由 ledger.applyLegs 在 account 锁内按当时余额钳制,
		// 差额自动拆入 MERCHANT_REORG_RECEIVABLE 并同事务置
		// merchants.payout_frozen (FIX-A/A2). 快照读↔入账的 TOCTOU 窗口
		// 就此消除 (且外层本就不能对 ledger_accounts FOR UPDATE —
		// ledger 在自身连接内已持锁, 外层持锁 = 应用层自死锁).
		legs = append(legs,
			leg(oi.merchantID, ix.cfg.ChainID, sym,
				types.AccountMerchantAvailable, types.DirectionDebit, maCr),
			leg(sys, ix.cfg.ChainID, sym,
				types.AccountCustodyUnswept, types.DirectionCredit, maCr))
	}
	for _, acct := range []types.LedgerAccountCode{types.AccountFeeRevenue, types.AccountSuspenseOverpayLiab} {
		v := credited[acct]
		if v == nil || v.Sign() <= 0 {
			continue
		}
		owner := oi.merchantID
		if acct == types.AccountFeeRevenue {
			owner = sys
		}
		legs = append(legs,
			leg(owner, ix.cfg.ChainID, sym, acct, types.DirectionDebit, v),
			leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyUnswept, types.DirectionCredit, v))
	}
	if err := ix.post(ctx, types.TemplateReorgReversal, ref,
		"reorg settled-order unwind "+oi.orderID.String(), legs); err != nil {
		return err
	}

	// 存活 FINALIZED 同单充值重建 (含本孤块外所有仍有效的同币种记录).
	rows, err := tx.Query(ctx,
		`SELECT id, amount::text FROM onchain_deposits
		  WHERE matched_order_id=$1 AND status='FINALIZED' AND id<>$2
		    AND deposit_category='ORDER_MATCH'`, oi.orderID, dep.id)
	if err != nil {
		return err
	}
	type surv struct {
		id  uuid.UUID
		amt *big.Int
	}
	var survs []surv
	received := new(big.Int)
	for rows.Next() {
		var s surv
		var amt string
		if err := rows.Scan(&s.id, &amt); err != nil {
			rows.Close()
			return err
		}
		s.amt = types.MustParseU256(amt).Big()
		received.Add(received, s.amt)
		survs = append(survs, s)
	}
	rows.Close()
	for _, s := range survs {
		slegs, err := ix.legsOfLatest(ctx, types.TemplateDepositFinalized, "dep:"+s.id.String()+"%")
		if err != nil {
			return err
		}
		creditAcct := types.AccountOrderEscrowLiab
		for _, l := range slegs {
			if l.Direction == types.DirectionCredit {
				creditAcct = l.Account
			}
		}
		if err := ix.post(ctx, types.TemplateDepositFinalized,
			fmt.Sprintf("dep-re:%s:%s", dep.id, s.id),
			"re-establish surviving deposit "+s.id.String(), []ports.PostEntryLeg{
				leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyUnswept, types.DirectionDebit, s.amt),
				leg(oi.merchantID, ix.cfg.ChainID, sym, creditAcct, types.DirectionCredit, s.amt),
			}); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE payment_orders SET status='SETTLED_REVERSED', received_amount=$2::numeric
		 WHERE id=$1`, oi.orderID, received.String()); err != nil {
		return err
	}
	// FIX-G BUG-2: 已结单被深回滚翻案 — 提交后广播 settled_reversed.
	recvU, oid := types.U256FromBig(received), oi.orderID
	*after = append(*after, func() {
		ix.publishOrderEvent(ctx, ports.OrderEvent{
			OrderID: oid, Type: ports.OrderEventStatusChanged,
			Status: types.OrderSettledReversed, ReceivedAmount: recvU,
			OccurredAt: ix.now()})
	})
	return emitOutbox(ctx, tx, oi.merchantID, "ORDER_SETTLED_REVERSED", map[string]any{
		"event":           "ORDER_SETTLED_REVERSED",
		"order_id":        oi.orderID.String(),
		"orphan_deposit":  dep.id.String(),
		"received_amount": received.String(),
	})
}

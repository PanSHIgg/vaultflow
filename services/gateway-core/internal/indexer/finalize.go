package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 引用键与账本腿读取 (深回滚复活周期 cN / 红冲定位)
// ---------------------------------------------------------------------------

// cycleOf 统计某对象已被 REORG_REVERSAL 红冲的次数 — 复活后重记账时
// 引用键追加 :cN / :rN 后缀, 避免幂等命中被红冲过的旧分录.
func (ix *Indexer) cycleOf(ctx context.Context, reorgRefPrefix string) (int, error) {
	var n int
	err := ix.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions
		  WHERE reference_type='REORG_REVERSAL' AND reference_id LIKE $1`,
		reorgRefPrefix+"%").Scan(&n)
	return n, err
}

// legRow 账本腿投影 (红冲时按已存分录反向构造).
type legRow struct {
	Account   types.LedgerAccountCode
	Direction types.Direction
	Amount    *big.Int
	Owner     uuid.UUID
	Asset     string
}

// legsOfLatest 读取 reference_id LIKE likePat 的最新一笔该模板事务的分录腿.
// 用于: dep 充值腿反查 (确认其挂账科目), settle 腿反查 (已结单红冲).
func (ix *Indexer) legsOfLatest(ctx context.Context, refType types.EntryTemplateCode,
	likePat string) ([]legRow, error) {
	var txnID uuid.UUID
	err := ix.db.QueryRow(ctx,
		`SELECT id FROM ledger_transactions
		  WHERE reference_type=$1 AND reference_id LIKE $2
		  ORDER BY created_at DESC, id DESC LIMIT 1`, string(refType), likePat).Scan(&txnID)
	if err != nil {
		return nil, err
	}
	rows, err := ix.db.Query(ctx,
		`SELECT la.account_type, e.direction, e.amount::text,
		        la.owner_merchant_id, la.asset_symbol
		   FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.account_id
		  WHERE e.transaction_id=$1`, txnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legRow
	for rows.Next() {
		var l legRow
		var amt string
		if err := rows.Scan(&l.Account, &l.Direction, &amt, &l.Owner, &l.Asset); err != nil {
			return nil, err
		}
		l.Amount = types.MustParseU256(amt).Big()
		out = append(out, l)
	}
	return out, rows.Err()
}

func leg(owner uuid.UUID, chainID types.ChainID, asset string,
	acct types.LedgerAccountCode, dir types.Direction, amount *big.Int) ports.PostEntryLeg {
	return ports.PostEntryLeg{
		OwnerMerchantID: owner, ChainID: chainID, AssetSymbol: asset,
		Account: acct, Direction: dir, Amount: types.U256FromBig(amount),
	}
}

func (ix *Indexer) post(ctx context.Context, tmpl types.EntryTemplateCode, ref, desc string,
	legs []ports.PostEntryLeg) error {
	if len(legs) == 0 {
		return nil
	}
	_, err := ix.ledger.PostEntry(ctx, ports.PostEntryRequest{
		Template: tmpl, ReferenceID: ref, Description: desc, Legs: legs})
	return err
}

// emitOutbox webhook_outbox 同事务插入 (merchant 为系统级科目归属时仍须
// 指向真实商户 — 系统事件不产 webhook).
func emitOutbox(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID,
	eventType string, payload any) error {
	if merchantID == types.SystemMerchantID {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO webhook_outbox (merchant_id, event_type, payload_json)
		 VALUES ($1,$2,$3)`, merchantID, eventType, string(b))
	return err
}

// ---------------------------------------------------------------------------
// 入站两段式: DETECTED -> FINALIZED (block_number <= safe_head)
// ---------------------------------------------------------------------------

// settleDeposits 按块高序结算全部已越过安全线的 DETECTED 充值.
// A1 兜底: JOIN chain_blocks CANONICAL — 即使捕获侧混入过非主链哈希的
// DETECTED 行 (分叉视图), 结算也绝不落账, 只能等回滚将其 ORPHANED.
func (ix *Indexer) settleDeposits(ctx context.Context, safe uint64) error {
	rows, err := ix.db.Query(ctx,
		`SELECT d.id FROM onchain_deposits d
		  JOIN chain_blocks cb
		    ON cb.chain_id=d.chain_id AND cb.block_hash=d.block_hash
		   AND cb.status='CANONICAL'
		  WHERE d.chain_id=$1 AND d.status='DETECTED' AND d.block_number<=$2
		  ORDER BY d.block_number, d.tx_hash, d.log_index`,
		int64(ix.cfg.ChainID), int64(safe))
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ix.finalizeDeposit(ctx, id); err != nil {
			return fmt.Errorf("finalize deposit %s: %w", id, err)
		}
	}
	return nil
}

// depFull 充值行 + 关联订单快照.
type depFull struct {
	id            uuid.UUID
	blockHash     types.Hash
	txHash        types.Hash
	logIndex      int32
	blockNumber   uint64
	tokenContract types.Address
	from          types.Address
	to            types.Address
	amount        *big.Int
	category      types.DepositCategory
	matchedOrder  *uuid.UUID
}

func (ix *Indexer) loadDeposit(ctx context.Context, depID uuid.UUID) (*depFull, error) {
	var d depFull
	var bh, th, tc, fa, ta, amt string
	err := ix.db.QueryRow(ctx,
		`SELECT id, block_hash, tx_hash, log_index, block_number,
		        token_contract, from_address, to_address, amount::text,
		        deposit_category, matched_order_id
		   FROM onchain_deposits WHERE id=$1 AND chain_id=$2`,
		depID, int64(ix.cfg.ChainID)).
		Scan(&d.id, &bh, &th, &d.logIndex, &d.blockNumber, &tc, &fa, &ta, &amt,
			&d.category, &d.matchedOrder)
	if err != nil {
		return nil, err
	}
	d.blockHash, d.txHash = types.HexToHash(bh), types.HexToHash(th)
	d.tokenContract, d.from, d.to = types.HexToAddress(tc), types.HexToAddress(fa), types.HexToAddress(ta)
	d.amount = types.MustParseU256(amt).Big()
	return &d, nil
}

// tokenSymbol supported_assets 反查代币符号 (白名单代币必有行).
func (ix *Indexer) tokenSymbol(ctx context.Context, contract types.Address) (string, error) {
	var sym string
	err := ix.db.QueryRow(ctx,
		`SELECT asset_symbol FROM supported_assets
		  WHERE chain_id=$1 AND token_contract=$2`,
		int64(ix.cfg.ChainID), addrStr(contract)).Scan(&sym)
	return sym, err
}

// finalizeDeposit 单笔 DETECTED -> FINALIZED: 记账 -> 状态/订单 -> outbox.
// PostEntry 经 ports.LedgerService 独立提交, 幂等键保证崩溃重放安全.
func (ix *Indexer) finalizeDeposit(ctx context.Context, depID uuid.UUID) error {
	dep, err := ix.loadDeposit(ctx, depID)
	if err != nil {
		return err
	}
	sym, err := ix.tokenSymbol(ctx, dep.tokenContract)
	if err != nil {
		return fmt.Errorf("token symbol for %s: %w", dep.tokenContract.Hex(), err)
	}
	cyc, err := ix.cycleOf(ctx, "reorgdep:"+depID.String())
	if err != nil {
		return err
	}
	var after []func() // 提交后 SSE 发布器

	err = pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM onchain_deposits WHERE id=$1 FOR UPDATE`, depID).
			Scan(&status); err != nil {
			return err
		}
		if status != string(types.DepositDetected) {
			return nil // 并发/回滚已处置 — 不再记账
		}

		ref := fmt.Sprintf("dep:%s:c%d", depID, cyc)
		sys := types.SystemMerchantID

		switch dep.category {
		case types.DepositCategoryTopup:
			// 分支 A 外部注资: DR CUSTODY_HOT_PAYOUT / CR TREASURY_EQUITY_CAPITAL.
			if err := ix.post(ctx, types.TemplateTreasuryTopup, "topup:"+depID.String()+fmt.Sprintf(":c%d", cyc),
				"external treasury topup "+dep.txHash.Hex(), []ports.PostEntryLeg{
					leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyHotPayout, types.DirectionDebit, dep.amount),
					leg(sys, ix.cfg.ChainID, sym, types.AccountTreasuryEquityCapital, types.DirectionCredit, dep.amount),
				}); err != nil {
				return err
			}

		case types.DepositCategoryTreasuryRebalance:
			// 分支 A 冷转热调拨: DR CUSTODY_HOT_PAYOUT / CR CUSTODY_COLD.
			if err := ix.post(ctx, types.TemplateInternalTreasuryRebalance, "rebal:"+depID.String()+fmt.Sprintf(":c%d", cyc),
				"cold->hot treasury rebalance "+dep.txHash.Hex(), []ports.PostEntryLeg{
					leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyHotPayout, types.DirectionDebit, dep.amount),
					leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyCold, types.DirectionCredit, dep.amount),
				}); err != nil {
				return err
			}

		default:
			// 分支 B/C/D — 均需订单行; matched 缺失属异常 (防御: 按系统挂账).
			var oi orderInfo
			var prevStatus types.OrderStatus
			var prevRecv types.U256
			if dep.matchedOrder != nil {
				if err := ix.loadOrderForUpdate(ctx, tx, *dep.matchedOrder, &oi); err != nil {
					return err
				}
				prevStatus, prevRecv = oi.status, oi.receivedAmount
			}
			credit := types.AccountSuspenseUnmatchedLiab // 分支 B 默认
			switch dep.category {
			case types.DepositCategoryPostTerminal:
				credit = types.AccountSuspenseRefundableLiab
			case types.DepositCategoryOrderMatch:
				// 结算期二次复核: 订单在等待 safe_head 期间可能已跃迁.
				var err error
				credit, err = ix.applyOrderMatch(ctx, tx, dep, &oi, sym, cyc)
				if err != nil {
					return err
				}
			}
			owner := oi.merchantID
			if dep.matchedOrder == nil {
				owner = sys // 防御兜底
			}
			if credit != "" { // applyOrderMatch 返回 "" 表示分录已在内部完成
				if err := ix.post(ctx, types.TemplateDepositFinalized, ref,
					"inbound deposit finalized "+dep.txHash.Hex(), []ports.PostEntryLeg{
						leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyUnswept, types.DirectionDebit, dep.amount),
						leg(owner, ix.cfg.ChainID, sym, credit, types.DirectionCredit, dep.amount),
					}); err != nil {
					return err
				}
			}
			if dep.matchedOrder != nil &&
				(oi.status != prevStatus || oi.receivedAmount.Big().Cmp(prevRecv.Big()) != 0) {
				// FIX-G BUG-2: 订单状态/实收额在本事务内变更 — 提交后广播
				// STATUS_CHANGED (PAID/OVERPAID/PARTIALLY_PAID 及迟付人工
				// 复核 LATE_PAID_REVIEW/UNDERPAID_EXPIRED_REVIEW 等;
				// api 侧按 status 蛇形映射 OpenAPI 事件名).
				st, recv, oid := oi.status, oi.receivedAmount, oi.orderID
				after = append(after, func() {
					ix.publishOrderEvent(ctx, ports.OrderEvent{
						OrderID: oid, Type: ports.OrderEventStatusChanged,
						Status: st, ReceivedAmount: recv, OccurredAt: ix.now()})
				})
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE onchain_deposits SET status='FINALIZED' WHERE id=$1`, depID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, fn := range after {
		fn()
	}
	return nil
}

// loadOrderForUpdate FOR UPDATE 加载订单 + 商户费率.
func (ix *Indexer) loadOrderForUpdate(ctx context.Context, tx pgx.Tx,
	orderID uuid.UUID, oi *orderInfo) error {
	var exp, recv string
	err := tx.QueryRow(ctx,
		`SELECT po.id, po.merchant_id, po.asset_symbol, po.expected_amount::text,
		        po.received_amount::text, po.status, po.expires_at,
		        po.deposit_address_id, m.fee_rate_bps
		   FROM payment_orders po JOIN merchants m ON m.id=po.merchant_id
		  WHERE po.id=$1 FOR UPDATE OF po`, orderID).
		Scan(&oi.orderID, &oi.merchantID, &oi.assetSymbol, &exp, &recv,
			&oi.status, &oi.expiresAt, &oi.depositAddrID, &oi.feeRateBps)
	if err != nil {
		return err
	}
	oi.expectedAmount = types.MustParseU256(exp)
	oi.receivedAmount = types.MustParseU256(recv)
	return nil
}

// applyOrderMatch 分支 D 结算期复核 + 订单状态推进; 返回挂账科目 —
// 返回 "" 表示完整分录已在本函数内提交 (正常达标结算路径).
func (ix *Indexer) applyOrderMatch(ctx context.Context, tx pgx.Tx, dep *depFull,
	oi *orderInfo, sym string, cyc int) (types.LedgerAccountCode, error) {

	sys := types.SystemMerchantID
	depRef := fmt.Sprintf("dep:%s:c%d", dep.id, cyc)
	newReceived := new(big.Int).Add(oi.receivedAmount.Big(), dep.amount)

	// 复核 1: 结算期订单已业务终态 -> 按分支 C 语义处理 (不改订单).
	if oi.status.IsBusinessTerminal() || oi.status == types.OrderSettledReversed {
		return types.AccountSuspenseRefundableLiab, nil
	}

	expired := !ix.now().Before(oi.expiresAt)

	if expired {
		// 过期入账: 挂 REFUNDABLE, 状态流转至人工复核 (不改 received 语义:
		// received_amount 仅累计同币种 FINALIZED 充值 — 仍累加).
		legs := []ports.PostEntryLeg{
			leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyUnswept, types.DirectionDebit, dep.amount),
			leg(oi.merchantID, ix.cfg.ChainID, sym, types.AccountSuspenseRefundableLiab, types.DirectionCredit, dep.amount),
		}
		if err := ix.post(ctx, types.TemplateDepositFinalized, depRef,
			"expired-order inbound finalized "+dep.txHash.Hex(), legs); err != nil {
			return "", err
		}
		target, alt := types.OrderLatePaidReview, types.OrderUnderpaidExpiredReview
		if newReceived.Cmp(oi.expectedAmount.Big()) < 0 {
			target, alt = alt, target
		}
		if !oi.status.CanTransition(target) {
			target = alt
		}
		if err := ix.bumpOrder(ctx, tx, oi, newReceived, target); err != nil {
			return "", err
		}
		return "", nil
	}

	// 有效期内正常入账: DR UNSWEPT / CR ESCROW.
	legs := []ports.PostEntryLeg{
		leg(sys, ix.cfg.ChainID, sym, types.AccountCustodyUnswept, types.DirectionDebit, dep.amount),
		leg(oi.merchantID, ix.cfg.ChainID, sym, types.AccountOrderEscrowLiab, types.DirectionCredit, dep.amount),
	}
	if err := ix.post(ctx, types.TemplateDepositFinalized, depRef,
		"inbound deposit finalized "+dep.txHash.Hex(), legs); err != nil {
		return "", err
	}

	if newReceived.Cmp(oi.expectedAmount.Big()) < 0 {
		// 未达标 — 推进 PARTIALLY_PAID (AWAITING/PARTIALLY 均可).
		if err := ix.bumpOrder(ctx, tx, oi, newReceived, types.OrderPartiallyPaid); err != nil {
			return "", err
		}
		return "", nil
	}

	// 达标/超付 — ORDER_SETTLE 清分: DR ESCROW received /
	//   CR MA (expected-fee) + CR FEE (fee) + CR OVR (received-expected).
	fee := new(big.Int).Mul(oi.expectedAmount.Big(), big.NewInt(oi.feeRateBps))
	fee.Div(fee, big.NewInt(10_000))
	maNet := new(big.Int).Sub(oi.expectedAmount.Big(), fee)
	over := new(big.Int).Sub(newReceived, oi.expectedAmount.Big())

	settleLegs := []ports.PostEntryLeg{
		leg(oi.merchantID, ix.cfg.ChainID, sym, types.AccountOrderEscrowLiab, types.DirectionDebit, newReceived),
		leg(oi.merchantID, ix.cfg.ChainID, sym, types.AccountMerchantAvailable, types.DirectionCredit, maNet),
	}
	if fee.Sign() > 0 {
		settleLegs = append(settleLegs,
			leg(sys, ix.cfg.ChainID, sym, types.AccountFeeRevenue, types.DirectionCredit, fee))
	}
	if over.Sign() > 0 {
		settleLegs = append(settleLegs,
			leg(oi.merchantID, ix.cfg.ChainID, sym, types.AccountSuspenseOverpayLiab, types.DirectionCredit, over))
	}
	if err := ix.post(ctx, types.TemplateOrderSettle, types.RefPrefixOrderSettle+oi.orderID.String(),
		"order settle "+oi.orderID.String(), settleLegs); err != nil {
		return "", err
	}

	target := types.OrderPaid
	if over.Sign() > 0 {
		target = types.OrderOverpaid
	}
	if err := ix.settleOrderRow(ctx, tx, oi, newReceived, target); err != nil {
		return "", err
	}
	if err := emitOutbox(ctx, tx, oi.merchantID, "ORDER_SETTLED", map[string]any{
		"event":           "ORDER_SETTLED",
		"order_id":        oi.orderID.String(),
		"status":          string(target),
		"received_amount": newReceived.String(),
		"expected_amount": oi.expectedAmount.String(),
		"asset_symbol":    oi.assetSymbol,
		"chain_id":        uint64(ix.cfg.ChainID),
	}); err != nil {
		return "", err
	}
	return "", nil
}

// bumpOrder 在 CanTransition 白名单内推进订单状态并更新 received_amount.
func (ix *Indexer) bumpOrder(ctx context.Context, tx pgx.Tx, oi *orderInfo,
	newReceived *big.Int, target types.OrderStatus) error {
	st := oi.status
	if st != target && st.CanTransition(target) {
		st = target
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payment_orders SET status=$2, received_amount=$3::numeric WHERE id=$1`,
		oi.orderID, string(st), newReceived.String()); err != nil {
		return err
	}
	oi.status, oi.receivedAmount = st, types.U256FromBig(newReceived)
	return nil
}

// settleOrderRow 达标结算的状态/金额/时间戳落库.
func (ix *Indexer) settleOrderRow(ctx context.Context, tx pgx.Tx, oi *orderInfo,
	received *big.Int, target types.OrderStatus) error {
	if !oi.status.CanTransition(target) && oi.status != target {
		return fmt.Errorf("indexer: order %s cannot transition %s -> %s",
			oi.orderID, oi.status, target)
	}
	_, err := tx.Exec(ctx,
		`UPDATE payment_orders SET status=$2, received_amount=$3::numeric, settled_at=now()
		 WHERE id=$1`, oi.orderID, string(target), received.String())
	oi.status, oi.receivedAmount = target, types.U256FromBig(received)
	return err
}

// ---------------------------------------------------------------------------
// 出站两段式: INCLUDED_UNSAFE -> FINALIZED (block_number <= safe_head)
//   成功与 Revert 均必记 GAS_SETTLE; 成功联动 SWEEP_SETTLE / PAYOUT_FLOW.
// ---------------------------------------------------------------------------

// settleAttempts 结算全部越过安全线的 INCLUDED_UNSAFE 候选.
func (ix *Indexer) settleAttempts(ctx context.Context, safe uint64) error {
	rows, err := ix.db.Query(ctx,
		`SELECT a.id FROM outbound_tx_attempts a
		  WHERE a.chain_id=$1 AND a.status='INCLUDED_UNSAFE' AND a.block_number<=$2
		  ORDER BY a.block_number, a.id`, int64(ix.cfg.ChainID), int64(safe))
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ix.finalizeAttempt(ctx, id); err != nil {
			return fmt.Errorf("finalize attempt %s: %w", id, err)
		}
	}
	return nil
}

// attFull attempt + intent 联合投影.
type attFull struct {
	attemptID     uuid.UUID
	intentID      uuid.UUID
	intentType    types.IntentType
	referenceID   uuid.UUID
	wallet        types.Address
	nonce         uint64
	kind          types.AttemptKind
	txHash        types.Hash
	receiptStatus int32
	gasUsed       uint64
	effGasPrice   *big.Int
	blockNumber   uint64
}

func (ix *Indexer) loadAttemptIntent(ctx context.Context, attemptID uuid.UUID) (*attFull, error) {
	var a attFull
	var w, th, bh string
	var egp *string
	err := ix.db.QueryRow(ctx,
		`SELECT a.id, a.intent_id, i.intent_type, i.reference_id, i.wallet_address,
		        i.nonce, a.attempt_kind, a.tx_hash, a.receipt_status, a.gas_used,
		        a.effective_gas_price::text, a.block_number, a.block_hash
		   FROM outbound_tx_attempts a JOIN outbound_tx_intents i ON i.id=a.intent_id
		  WHERE a.id=$1`, attemptID).
		Scan(&a.attemptID, &a.intentID, &a.intentType, &a.referenceID, &w,
			&a.nonce, &a.kind, &th, &a.receiptStatus, &a.gasUsed, &egp,
			&a.blockNumber, &bh)
	if err != nil {
		return nil, err
	}
	a.wallet = types.HexToAddress(w)
	a.txHash = types.HexToHash(th)
	if egp != nil {
		a.effGasPrice = types.MustParseU256(*egp).Big()
	}
	return &a, nil
}

// finalizeAttempt 出站候选两段式结算: 必记 GAS_SETTLE + 业务联动.
func (ix *Indexer) finalizeAttempt(ctx context.Context, attemptID uuid.UUID) error {
	a, err := ix.loadAttemptIntent(ctx, attemptID)
	if err != nil {
		return err
	}
	gasAmt := new(big.Int)
	if a.effGasPrice != nil && a.gasUsed > 0 {
		gasAmt.Mul(a.effGasPrice, new(big.Int).SetUint64(a.gasUsed))
	}
	cyc, err := ix.cycleOf(ctx, "reorggas:"+attemptID.String())
	if err != nil {
		return err
	}
	gasRef := fmt.Sprintf("gas:%s:c%d", attemptID, cyc)

	// SWEEP 成功需回执事件; 先在事务外取回执 (网络 I/O).
	var events []chain.SweepEvent
	if a.intentType == types.IntentTypeSweepBatch && a.receiptStatus == 1 {
		rcpt, err := ix.chain.ReceiptAt(ctx, a.txHash)
		if err != nil {
			return fmt.Errorf("receipt for sweep events: %w", err)
		}
		if rcpt == nil {
			return fmt.Errorf("indexer: receipt %s vanished at finalize", a.txHash.Hex())
		}
		events = chain.ParseSweepEvents(rcpt.Logs, ix.cfg.BatchSweeper)
	}

	sys := types.SystemMerchantID

	var after []func() // FIX-G BUG-2: 提交后 SSE 发布器
	err = pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM outbound_tx_attempts WHERE id=$1 FOR UPDATE`, attemptID).
			Scan(&status); err != nil {
			return err
		}
		if status != string(types.AttemptIncludedUnsafe) {
			return nil
		}
		var intentStatus string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM outbound_tx_intents WHERE id=$1 FOR UPDATE`, a.intentID).
			Scan(&intentStatus); err != nil {
			return err
		}

		// ① 必记 GAS_SETTLE (成功与 Revert 同记).
		if gasAmt.Sign() > 0 {
			if err := ix.post(ctx, types.TemplateGasSettle, gasRef,
				"outbound gas settle "+a.txHash.Hex(), []ports.PostEntryLeg{
					leg(sys, ix.cfg.ChainID, "ETH", types.AccountGasExpense, types.DirectionDebit, gasAmt),
					leg(sys, ix.cfg.ChainID, "ETH", types.AccountHotWalletNative, types.DirectionCredit, gasAmt),
				}); err != nil {
				return err
			}
		} else {
			ix.raise(Alert{Severity: "WARNING", Code: "GAS_PRICE_MISSING",
				Message: "receipt missing gas fields, GAS_SETTLE skipped",
				Detail:  fmt.Sprintf("attempt=%s tx=%s", attemptID, a.txHash.Hex())})
		}

		newIntentStatus := types.IntentConfirmed
		if a.receiptStatus != 1 {
			newIntentStatus = types.IntentFailedReverted
		}
		if a.kind == types.AttemptKindGapFiller && a.receiptStatus == 1 {
			newIntentStatus = types.IntentCancelledByFiller
		}

		// ② 业务联动 (receipt==1 成功路径).
		if a.receiptStatus == 1 && a.kind == types.AttemptKindPrimary {
			switch a.intentType {
			case types.IntentTypeSweepBatch:
				if err := ix.settleSweepBatch(ctx, tx, a, events, cyc); err != nil {
					return err
				}
			case types.IntentTypeMerchantPayout:
				if err := ix.settlePayoutSuccess(ctx, tx, a, cyc, &after); err != nil {
					return err
				}
			}
		}
		// ③ 失败/取消联动.
		switch {
		case a.receiptStatus == 0 && a.intentType == types.IntentTypeMerchantPayout:
			if err := ix.failPayoutRetryable(ctx, tx, a.referenceID); err != nil {
				return err
			}
		case a.receiptStatus == 0 && a.intentType == types.IntentTypeSweepBatch:
			if _, err := tx.Exec(ctx,
				`UPDATE sweep_batches SET status='FAILED' WHERE id=$1 AND status='IN_FLIGHT'`,
				a.referenceID); err != nil {
				return err
			}
		case a.kind == types.AttemptKindGapFiller && a.receiptStatus == 1:
			// 业务意图被同 Nonce 自转取消 — 关联 payout 转可重试.
			if a.intentType == types.IntentTypeMerchantPayout {
				if err := ix.failPayoutRetryable(ctx, tx, a.referenceID); err != nil {
					return err
				}
			}
		}

		// ④ 状态跃迁落库.
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts SET status='FINALIZED' WHERE id=$1`, attemptID); err != nil {
			return err
		}
		if intentStatus == string(types.IntentIncludedPendingSafe) ||
			intentStatus == string(types.IntentInFlight) {
			if _, err := tx.Exec(ctx,
				`UPDATE outbound_tx_intents SET status=$2 WHERE id=$1`,
				a.intentID, string(newIntentStatus)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, fn := range after {
		fn()
	}
	return nil
}

// settleSweepBatch 解析 VaultSwept/VaultQuarantined 事件并逐条联动.
func (ix *Indexer) settleSweepBatch(ctx context.Context, tx pgx.Tx, a *attFull,
	events []chain.SweepEvent, cyc int) error {

	var asset string
	err := tx.QueryRow(ctx,
		`SELECT asset_symbol FROM sweep_batches WHERE id=$1 FOR UPDATE`,
		a.referenceID).Scan(&asset)
	if err != nil {
		return err
	}
	sys := types.SystemMerchantID
	total := new(big.Int)
	var quarantined bool

	for _, ev := range events {
		var itemID, daID uuid.UUID
		var exact string
		err := tx.QueryRow(ctx,
			`SELECT i.id, i.deposit_address_id, i.exact_sweep_amount::text
			   FROM sweep_batch_items i JOIN deposit_addresses da ON da.id=i.deposit_address_id
			  WHERE i.batch_id=$1 AND da.address=$2 AND i.item_status='PENDING'`,
			a.referenceID, addrStr(ev.Vault)).Scan(&itemID, &daID, &exact)
		if err != nil {
			if err == pgx.ErrNoRows {
				ix.raise(Alert{Severity: "WARNING", Code: "SWEEP_EVENT_UNMATCHED",
					Message: "sweep event vault not in batch pending items",
					Detail:  fmt.Sprintf("batch=%s vault=%s", a.referenceID, ev.Vault.Hex())})
				continue
			}
			return err
		}
		if ev.Kind == chain.SweepEventQuarantined {
			if _, err := tx.Exec(ctx,
				`UPDATE sweep_batch_items SET item_status='QUARANTINED_REVERT' WHERE id=$1`, itemID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE deposit_addresses SET is_quarantined=TRUE WHERE id=$1`, daID); err != nil {
				return err
			}
			quarantined = true
			continue
		}
		// A4: VaultSwept(..,0) 防御 — 合约侧 0 额已改走 VaultQuarantined(..,0),
		// 链上仍可能残留/异常产生 0 额 VaultSwept. 不记分录不致命: 降级结构化
		// 告警, item 保持 PENDING (余额在途占用不释放, 可重新归集), 其余项正常结算.
		if ev.Amount == nil || ev.Amount.Sign() <= 0 {
			ix.raise(Alert{Severity: "WARNING", Code: "SWEEP_EVENT_ZERO_AMOUNT",
				Message: "VaultSwept event with zero amount — no ledger entry, item left PENDING",
				Detail: fmt.Sprintf("batch=%s item=%s vault=%s tx=%s salt=%s",
					a.referenceID, itemID, ev.Vault.Hex(), a.txHash.Hex(), ev.Salt.Hex())})
			continue
		}
		// VaultSwept — SWEEP_SETTLE: DR CUSTODY_COLD / CR CUSTODY_UNSWEPT (精确额).
		if err := ix.post(ctx, types.TemplateSweepSettle,
			fmt.Sprintf("sweepitem:%s:%s:c%d", itemID, a.attemptID, cyc),
			"vault swept "+ev.Vault.Hex(), []ports.PostEntryLeg{
				leg(sys, ix.cfg.ChainID, asset, types.AccountCustodyCold, types.DirectionDebit, ev.Amount),
				leg(sys, ix.cfg.ChainID, asset, types.AccountCustodyUnswept, types.DirectionCredit, ev.Amount),
			}); err != nil {
			return err
		}
		total.Add(total, ev.Amount)
		if _, err := tx.Exec(ctx,
			`UPDATE sweep_batch_items SET item_status='SWEPT', actual_swept_amount=$2::numeric
			 WHERE id=$1`, itemID, ev.Amount.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deposit_addresses SET is_deployed=TRUE WHERE id=$1`, daID); err != nil {
			return err
		}
	}
	batchStatus := "COMPLETED"
	if quarantined {
		batchStatus = "PARTIAL_QUARANTINED"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sweep_batches SET status=$2, total_swept_amount=$3::numeric
		 WHERE id=$1`, a.referenceID, batchStatus, total.String()); err != nil {
		return err
	}
	return emitOutbox(ctx, tx, sys, "SWEEP_"+batchStatus, map[string]any{
		"event":      "SWEEP_" + batchStatus,
		"batch_id":   a.referenceID.String(),
		"total":      total.String(),
		"asset":      asset,
		"quarantine": quarantined,
	})
}

// settlePayoutSuccess MERCHANT_PAYOUT 成功: PAYOUT_FLOW 执行腿 +
// payout COMPLETED + 退款单关联订单 REFUNDED.
// after 为提交后 SSE 发布器收集槽 (FIX-G BUG-2).
func (ix *Indexer) settlePayoutSuccess(ctx context.Context, tx pgx.Tx,
	a *attFull, cyc int, after *[]func()) error {
	var p types.MerchantPayout
	var amount string
	err := tx.QueryRow(ctx,
		`SELECT merchant_id, payout_kind, source_account_type, source_order_id,
		        asset_symbol, amount::text, status
		   FROM merchant_payouts WHERE id=$1 FOR UPDATE`, a.referenceID).
		Scan(&p.MerchantID, &p.PayoutKind, &p.SourceAccount, &p.SourceOrderID,
			&p.AssetSymbol, &amount, &p.Status)
	if err != nil {
		return err
	}
	amt := types.MustParseU256(amount).Big()
	sys := types.SystemMerchantID

	// FIX-E/E2: 行锁内先判可迁移再记账 — QueueIntent→首播窗口内
	// CancelPayout 竞速获胜时 payout 已是 CANCELLED_UNLOCKED (冻结腿
	// 已解冻回来源科目). 此时链上候选仍真实成交, 但账上绝不可再记
	// PAYOUT_FLOW 执行腿 (否则 PAYOUT_LOCKED_LIAB 双扣 / CUSTODY 双贷);
	// 也不得以 COMPLETED 复活已取消单据. 跳过记账 + 结构化告警交给
	// 人工对账 — 返回 nil 让索引器继续推进 (不可硬卡 settleAttempts).
	if !p.Status.CanTransition(types.PayoutCompleted) {
		ix.raise(Alert{
			Severity: "CRITICAL",
			Code:     "PAYOUT_EXEC_AFTER_CANCEL",
			Message: "chain transfer executed but payout is no longer completable — " +
				"execution accounting skipped, manual reconciliation required",
			Detail: fmt.Sprintf("payout=%s status=%s merchant=%s amount=%s %s attempt=%s tx=%s",
				a.referenceID, p.Status, p.MerchantID, amount, p.AssetSymbol, a.attemptID, a.txHash.Hex()),
		})
		return nil
	}

	// PAYOUT_FLOW 执行腿: DR PAYOUT_LOCKED_LIAB / CR CUSTODY_HOT_PAYOUT.
	if err := ix.post(ctx, types.TemplatePayoutFlow,
		fmt.Sprintf("payoutexec:%s:c%d", a.attemptID, cyc),
		"payout execution "+a.txHash.Hex(), []ports.PostEntryLeg{
			leg(p.MerchantID, ix.cfg.ChainID, p.AssetSymbol, types.AccountPayoutLockedLiab, types.DirectionDebit, amt),
			leg(sys, ix.cfg.ChainID, p.AssetSymbol, types.AccountCustodyHotPayout, types.DirectionCredit, amt),
		}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE merchant_payouts SET status='COMPLETED' WHERE id=$1`, a.referenceID); err != nil {
		return err
	}
	// CUSTOMER_REFUND 关联订单 -> REFUNDED (白名单 REFUND_IN_PROGRESS->REFUNDED).
	if p.PayoutKind == types.PayoutKindCustomerRefund && p.SourceOrderID != nil {
		var recvStr string
		err := tx.QueryRow(ctx,
			`UPDATE payment_orders SET status='REFUNDED'
			 WHERE id=$1 AND status='REFUND_IN_PROGRESS'
			 RETURNING received_amount::text`, *p.SourceOrderID).Scan(&recvStr)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			// FIX-G BUG-2: 退款完结订单 — 提交后广播 refunded (终态帧,
			// api 侧在 REFUNDED 后关闭 SSE 流).
			recv, oid := types.MustParseU256(recvStr), *p.SourceOrderID
			*after = append(*after, func() {
				ix.publishOrderEvent(ctx, ports.OrderEvent{
					OrderID: oid, Type: ports.OrderEventStatusChanged,
					Status: types.OrderRefunded, ReceivedAmount: recv,
					OccurredAt: ix.now()})
			})
		}
	}
	return emitOutbox(ctx, tx, p.MerchantID, "PAYOUT_COMPLETED", map[string]any{
		"event":     "PAYOUT_COMPLETED",
		"payout_id": a.referenceID.String(),
		"amount":    amount,
		"asset":     p.AssetSymbol,
		"tx_hash":   a.txHash.Hex(),
	})
}

// failPayoutRetryable 出站 Revert/Filler 取消 -> payout FAILED_RETRYABLE.
func (ix *Indexer) failPayoutRetryable(ctx context.Context, tx pgx.Tx, payoutID uuid.UUID) error {
	var merchant uuid.UUID
	var st string
	if err := tx.QueryRow(ctx,
		`SELECT merchant_id, status FROM merchant_payouts WHERE id=$1 FOR UPDATE`,
		payoutID).Scan(&merchant, &st); err != nil {
		return err
	}
	if types.PayoutStatus(st).CanTransition(types.PayoutFailedRetryable) {
		if _, err := tx.Exec(ctx,
			`UPDATE merchant_payouts SET status='FAILED_RETRYABLE' WHERE id=$1`, payoutID); err != nil {
			return err
		}
		return emitOutbox(ctx, tx, merchant, "PAYOUT_FAILED_RETRYABLE", map[string]any{
			"event":     "PAYOUT_FAILED_RETRYABLE",
			"payout_id": payoutID.String(),
		})
	}
	return nil
}

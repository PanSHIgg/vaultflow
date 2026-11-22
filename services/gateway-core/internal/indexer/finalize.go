package indexer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 入站两段式: DETECTED -> FINALIZED (block_number <= safe_head)
//
// 交易所托管模型下本层只做纯链侧状态推进 — 用户余额贷记 (FINALIZED ->
// CREDITED + user_balances UPSERT) 由 backend credit worker 独立事务完成
// (store.CreditDepositAtomically, 幂等), 与索引进程解耦.
// ---------------------------------------------------------------------------

// settleDeposits 按块高序结算全部已越过安全线的 DETECTED 充值.
// 兜底: JOIN chain_blocks CANONICAL — 即使捕获侧混入过非主链哈希的
// DETECTED 行 (分叉视图), 结算也绝不推进, 只能等回滚将其 ORPHANED.
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

// finalizeDeposit 单笔 DETECTED -> FINALIZED (FOR UPDATE 复核后推进,
// 并发/回滚已处置时幂等跳过).
func (ix *Indexer) finalizeDeposit(ctx context.Context, depID uuid.UUID) error {
	return pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM onchain_deposits WHERE id=$1 FOR UPDATE`, depID).
			Scan(&status); err != nil {
			return err
		}
		if status != "DETECTED" {
			return nil
		}
		_, err := tx.Exec(ctx,
			`UPDATE onchain_deposits SET status='FINALIZED' WHERE id=$1`, depID)
		return err
	})
}

// ---------------------------------------------------------------------------
// 出站两段式: INCLUDED_UNSAFE -> FINALIZED (block_number <= safe_head)
//   成功路径联动 sweep_batches/items 与 user_deposit_addresses 部署/隔离
//   标记; 业务余额语义 (CREDITED vault 充值 -> SWEPT) 经 store 提交后调用.
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
	blockNumber   uint64
}

func (ix *Indexer) loadAttemptIntent(ctx context.Context, attemptID uuid.UUID) (*attFull, error) {
	var a attFull
	var w, th, bh string
	err := ix.db.QueryRow(ctx,
		`SELECT a.id, a.intent_id, i.intent_type, i.reference_id, i.wallet_address,
		        i.nonce, a.attempt_kind, a.tx_hash, a.receipt_status, a.gas_used,
		        a.block_number, a.block_hash
		   FROM outbound_tx_attempts a JOIN outbound_tx_intents i ON i.id=a.intent_id
		  WHERE a.id=$1`, attemptID).
		Scan(&a.attemptID, &a.intentID, &a.intentType, &a.referenceID, &w,
			&a.nonce, &a.kind, &th, &a.receiptStatus, &a.gasUsed,
			&a.blockNumber, &bh)
	if err != nil {
		return nil, err
	}
	a.wallet = types.HexToAddress(w)
	a.txHash = types.HexToHash(th)
	return &a, nil
}

// finalizeAttempt 出站候选两段式结算 (纯链侧): attempt FINALIZED +
// 意图 CONFIRMED/FAILED_REVERTED/CANCELLED_BY_FILLER + SWEEP_BATCH 业务联动.
func (ix *Indexer) finalizeAttempt(ctx context.Context, attemptID uuid.UUID) error {
	a, err := ix.loadAttemptIntent(ctx, attemptID)
	if err != nil {
		return err
	}

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

	var after []func() // 提交后 store 副作用 (部署/隔离标记 + SWEPT 迁移)
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

		newIntentStatus := types.IntentConfirmed
		if a.receiptStatus != 1 {
			newIntentStatus = types.IntentFailedReverted
		}
		if a.kind == types.AttemptKindGapFiller && a.receiptStatus == 1 {
			newIntentStatus = types.IntentCancelledByFiller
		}

		// 业务联动 (receipt==1 成功路径): 归集批次逐条结算.
		if a.receiptStatus == 1 && a.kind == types.AttemptKindPrimary &&
			a.intentType == types.IntentTypeSweepBatch {
			if err := ix.settleSweepBatch(ctx, tx, a, events, &after); err != nil {
				return err
			}
		}
		// 失败联动: 归集批次 revert -> FAILED (items 由 sweeper reconcile 释放).
		if a.receiptStatus == 0 && a.intentType == types.IntentTypeSweepBatch {
			if _, err := tx.Exec(ctx,
				`UPDATE sweep_batches SET status='FAILED' WHERE id=$1 AND status='IN_FLIGHT'`,
				a.referenceID); err != nil {
				return err
			}
		}

		// 状态跃迁落库.
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

// settleSweepBatch 解析 VaultSwept/VaultQuarantined 事件并逐条联动:
//   - VaultSwept      -> item SWEPT + actual_swept_amount + vault is_deployed;
//     提交后 store.MarkDepositsSweptByVault 把该 vault 上 CREDITED 充值迁 SWEPT;
//   - VaultQuarantined -> item QUARANTINED_REVERT + vault is_quarantined;
//   - 批次 -> COMPLETED / PARTIAL_QUARANTINED + total_swept_amount.
//
// 部署/隔离标记与 CREDITED->SWEPT 迁移走 store 方法并在本事务 COMMIT 后执行
// (收集进 after) — 批次结算回滚时绝不残留半旗标.
func (ix *Indexer) settleSweepBatch(ctx context.Context, tx pgx.Tx, a *attFull,
	events []chain.SweepEvent, after *[]func()) error {

	// FOR UPDATE 锁定批次行 (与 reaper/回滚路径串行).
	var asset string
	err := tx.QueryRow(ctx,
		`SELECT asset_symbol FROM sweep_batches WHERE id=$1 FOR UPDATE`,
		a.referenceID).Scan(&asset)
	if err != nil {
		return err
	}
	var quarantined bool

	for _, ev := range events {
		var itemID, daID uuid.UUID
		var exact string
		err := tx.QueryRow(ctx,
			`SELECT i.id, i.deposit_address_id, i.exact_sweep_amount::text
			   FROM sweep_batch_items i JOIN user_deposit_addresses uda
			     ON uda.id=i.deposit_address_id
			  WHERE i.batch_id=$1 AND uda.address=$2 AND i.item_status='PENDING'`,
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
			id := daID
			*after = append(*after, func() {
				if _, err := ix.st.MarkUserDepositAddressQuarantined(ctx, id); err != nil {
					ix.log.Warn("mark deposit address quarantined failed",
						zap.String("uda", id.String()), zap.Error(err))
				}
			})
			quarantined = true
			continue
		}
		// A4: VaultSwept(..,0) 防御 — 合约侧 0 额已改走 VaultQuarantined(..,0),
		// 链上仍可能残留/异常产生 0 额 VaultSwept. item 保持 PENDING
		// (余额在途占用不释放, 可重新归集), 其余项正常结算.
		if ev.Amount == nil || ev.Amount.Sign() <= 0 {
			ix.raise(Alert{Severity: "WARNING", Code: "SWEEP_EVENT_ZERO_AMOUNT",
				Message: "VaultSwept event with zero amount — item left PENDING",
				Detail: fmt.Sprintf("batch=%s item=%s vault=%s tx=%s salt=%s",
					a.referenceID, itemID, ev.Vault.Hex(), a.txHash.Hex(), ev.Salt.Hex())})
			continue
		}
		// VaultSwept — item SWEPT + 精确额 (账本语义由 user_balances 承担,
		// CREDITED -> SWEPT 迁移在提交后由 store 完成).
		if _, err := tx.Exec(ctx,
			`UPDATE sweep_batch_items SET item_status='SWEPT', actual_swept_amount=$2::numeric
			 WHERE id=$1`, itemID, ev.Amount.String()); err != nil {
			return err
		}
		id := daID
		*after = append(*after, func() {
			if _, err := ix.st.MarkUserDepositAddressDeployed(ctx, id); err != nil {
				ix.log.Warn("mark deposit address deployed failed",
					zap.String("uda", id.String()), zap.Error(err))
			}
			if n, err := ix.st.MarkDepositsSweptByVault(ctx, ix.cfg.ChainID, id); err != nil {
				ix.log.Warn("mark deposits swept by vault failed",
					zap.String("uda", id.String()), zap.Error(err))
			} else if n > 0 {
				ix.log.Info("vault deposits marked SWEPT",
					zap.String("uda", id.String()), zap.Int64("deposits", n))
			}
		})
	}
	// 累加 total_swept_amount (text->numeric, 防御 NULL).
	var totalSum string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(actual_swept_amount),0)::text FROM sweep_batch_items
		  WHERE batch_id=$1 AND item_status='SWEPT'`, a.referenceID).Scan(&totalSum); err != nil {
		return err
	}
	batchStatus := "COMPLETED"
	if quarantined {
		batchStatus = "PARTIAL_QUARANTINED"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sweep_batches SET status=$2, total_swept_amount=$3::numeric
		 WHERE id=$1`, a.referenceID, batchStatus, totalSum); err != nil {
		return err
	}
	return nil
}

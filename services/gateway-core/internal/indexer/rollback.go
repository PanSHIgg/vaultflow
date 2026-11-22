package indexer

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 统一 LIFO 逆序回滚: 父哈希断裂 -> 分叉点 -> latest_synced 逐块逆退
//
// 交易所托管模型的回滚面:
//   - 出站: 候选重置 BROADCASTED + 兄弟 SUPERSEDED 复活 + 意图退回 IN_FLIGHT
//     (重广播由 txmanager 巡检自愈 — 本进程不再持有 Rebroadcaster);
//   - 入站: DETECTED->ORPHANED_SHALLOW, FINALIZED->ORPHANED_DEEP,
//     CREDITED->store.DebitUserBalanceOnOrphan (余额原子冲正),
//     SWEPT->CRITICAL 人工对账;
//   - 已结归集批次 (COMPLETED/PARTIAL_QUARANTINED) -> items PENDING +
//     vault 隔离标记还原 + 批次 IN_FLIGHT + 该批次 vault 的 SWEPT 充值
//     退回 CREDITED (链上资金回到 vault, 可重新聚合归集).
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

// rollbackToFork 将 (fork, latest] 区间逐块 LIFO 逆退, 游标回退至分叉点.
// 重置态意图的候选已回 BROADCASTED — 重广播由 txmanager 巡检路径自愈.
func (ix *Indexer) rollbackToFork(ctx context.Context, cur types.ChainSyncCursor, fork uint64) error {
	ix.raise(Alert{
		Severity: "WARNING", Code: "REORG_DETECTED",
		Message: fmt.Sprintf("reorg: rolling back heights (%d, %d] LIFO",
			fork, cur.LatestSyncedHeight),
		Detail: fmt.Sprintf("chain=%d", ix.cfg.ChainID),
	})

	for h := cur.LatestSyncedHeight; h > fork; h-- {
		if err := ix.rollbackBlock(ctx, h); err != nil {
			return fmt.Errorf("rollback block %d: %w", h, err)
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
	_, err := ix.db.Exec(ctx,
		`UPDATE chain_sync_cursors SET latest_synced_height=$2, latest_synced_hash=$3, updated_at=now()
		 WHERE chain_id=$1`, int64(ix.cfg.ChainID), int64(fork), forkHash)
	return err
}

// rollbackBlock 单块 LIFO 逆退 (块 ORPHANED + 出站重置 + 入站孤块化/冲正).
func (ix *Indexer) rollbackBlock(ctx context.Context, h uint64) error {
	var blkHash string
	err := ix.db.QueryRow(ctx,
		`SELECT block_hash FROM chain_blocks
		  WHERE chain_id=$1 AND block_number=$2 AND status='CANONICAL'`,
		int64(ix.cfg.ChainID), int64(h)).Scan(&blkHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // 高度块未索引 (防御)
	}
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE chain_blocks SET status='ORPHANED'
			 WHERE chain_id=$1 AND block_hash=$2 AND status='CANONICAL'`,
			int64(ix.cfg.ChainID), blkHash); err != nil {
			return err
		}
		if err := ix.rollbackOutbound(ctx, tx, blkHash); err != nil {
			return err
		}
		return ix.rollbackInbound(ctx, tx, blkHash)
	})
}

// ---------------------------------------------------------------------------
// 出站回滚: 浅退清块字段 / 深退还原归集批次; SUPERSEDED 兄弟一并复活;
// 意图退回 IN_FLIGHT 交由 txmanager 巡检重广播 — 同 tx_hash 可平滑重入.
// ---------------------------------------------------------------------------

func (ix *Indexer) rollbackOutbound(ctx context.Context, tx pgx.Tx, blkHash string) error {
	rows, err := tx.Query(ctx,
		`SELECT a.id, a.intent_id, a.status, a.attempt_kind, a.receipt_status,
		        i.intent_type, i.reference_id
		   FROM outbound_tx_attempts a JOIN outbound_tx_intents i ON i.id=a.intent_id
		  WHERE a.chain_id=$1 AND a.block_hash=$2
		    AND a.status IN ('INCLUDED_UNSAFE','FINALIZED')`,
		int64(ix.cfg.ChainID), blkHash)
	if err != nil {
		return err
	}
	type row struct {
		attID, intentID uuid.UUID
		status          string
		kind            string
		receipt         *int32
		intentType      string
		referenceID     uuid.UUID
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.attID, &r.intentID, &r.status, &r.kind, &r.receipt,
			&r.intentType, &r.referenceID); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()

	for _, r := range list {
		if r.status == string(types.AttemptFinalized) {
			if err := ix.reverseAttempt(ctx, tx, r.attID,
				types.IntentType(r.intentType), r.referenceID); err != nil {
				return err
			}
		}
		// 本候选重置 BROADCASTED (终态 FINALIZED 亦放行 — 深回滚特例).
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts
			   SET status='BROADCASTED', block_number=NULL, block_hash=NULL,
			       receipt_status=NULL, gas_used=NULL, effective_gas_price=NULL
			 WHERE id=$1 AND status IN ('INCLUDED_UNSAFE','FINALIZED')`, r.attID); err != nil {
			return err
		}
		// 同 Nonce SUPERSEDED 兄弟复活 (DROPPED 永不复活).
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts SET status='BROADCASTED',
			       block_number=NULL, block_hash=NULL, receipt_status=NULL,
			       gas_used=NULL, effective_gas_price=NULL
			 WHERE intent_id=$1 AND status='SUPERSEDED'`, r.intentID); err != nil {
			return err
		}
		// 意图退回 IN_FLIGHT (全部已结算态均可退回).
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_intents SET status='IN_FLIGHT'
			 WHERE id=$1 AND status IN
			   ('IN_FLIGHT','INCLUDED_PENDING_SAFE','CONFIRMED','FAILED_REVERTED','CANCELLED_BY_FILLER')`,
			r.intentID); err != nil {
			return err
		}
	}
	return nil
}

// reverseAttempt 已结算出站候选的业务还原 (交易所模型仅归集一条路径):
// SWEEP_BATCH 且成功结算 -> reverseSweepBatch; 其余 (GAP_FILLER 取消 /
// Revert) 无业务侧效需要还原.
func (ix *Indexer) reverseAttempt(ctx context.Context, tx pgx.Tx,
	attID uuid.UUID, intentType types.IntentType, referenceID uuid.UUID) error {

	if intentType != types.IntentTypeSweepBatch {
		return nil
	}
	return ix.reverseSweepBatch(ctx, tx, attID, referenceID)
}

// reverseSweepBatch 归集成功结算的深回滚:
//   - 批次内已 SWEPT/QUARANTINED_REVERT 条目逐条还原 PENDING;
//   - 隔离 vault 的 is_quarantined 还原;
//   - 批次 vault 上已被迁 SWEPT 的充值退回 CREDITED (链上资金仍在 vault,
//     聚合侧可重新归集 — 否则余额净额被已清 item 抵消成负值永不再归集);
//   - 批次恢复 IN_FLIGHT 待重广播重新归集.
func (ix *Indexer) reverseSweepBatch(ctx context.Context, tx pgx.Tx,
	attID, batchID uuid.UUID) error {

	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sweep_batches WHERE id=$1 FOR UPDATE`, batchID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT i.id, i.deposit_address_id, i.item_status
		   FROM sweep_batch_items i
		  WHERE i.batch_id=$1 AND i.item_status IN ('SWEPT','QUARANTINED_REVERT')`,
		batchID)
	if err != nil {
		return err
	}
	type item struct {
		id, daID uuid.UUID
		status   string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.daID, &it.status); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()

	for _, it := range items {
		if it.status == "QUARANTINED_REVERT" {
			// 隔离标记随块回滚一并还原.
			if _, err := tx.Exec(ctx,
				`UPDATE user_deposit_addresses SET is_quarantined=FALSE WHERE id=$1`, it.daID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sweep_batch_items SET item_status='PENDING', actual_swept_amount=NULL
			 WHERE id=$1`, it.id); err != nil {
			return err
		}
	}
	// 本批次 vault 的 SWEPT 充值退回 CREDITED (链上归集交易被孤块化,
	// 资金物理上仍在 vault — 余额语义与链上重新一致).
	if _, err := tx.Exec(ctx,
		`UPDATE onchain_deposits SET status='CREDITED'
		  WHERE chain_id=$1 AND status='SWEPT'
		    AND to_address IN (SELECT uda.address FROM user_deposit_addresses uda
		                       JOIN sweep_batch_items i ON i.deposit_address_id=uda.id
		                       WHERE i.batch_id=$2)`,
		int64(ix.cfg.ChainID), batchID); err != nil {
		return err
	}
	// 批次恢复在途, 待重广播重新归集.
	_, err = tx.Exec(ctx,
		`UPDATE sweep_batches SET status='IN_FLIGHT', total_swept_amount=0
		 WHERE id=$1 AND status IN ('COMPLETED','PARTIAL_QUARANTINED','FAILED')`, batchID)
	return err
}

// ---------------------------------------------------------------------------
// 入站回滚: DETECTED->ORPHANED_SHALLOW / FINALIZED->ORPHANED_DEEP /
// CREDITED->余额原子冲正 / SWEPT->CRITICAL 人工对账.
// ---------------------------------------------------------------------------

func (ix *Indexer) rollbackInbound(ctx context.Context, tx pgx.Tx, blkHash string) error {
	rows, err := tx.Query(ctx,
		`SELECT id, status, deposit_category FROM onchain_deposits
		  WHERE chain_id=$1 AND block_hash=$2
		    AND status IN ('DETECTED','FINALIZED','CREDITED','SWEPT')`,
		int64(ix.cfg.ChainID), blkHash)
	if err != nil {
		return err
	}
	type dr struct {
		id       uuid.UUID
		status   string
		category string
	}
	var deps []dr
	for rows.Next() {
		var d dr
		if err := rows.Scan(&d.id, &d.status, &d.category); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, d)
	}
	rows.Close()

	for _, d := range deps {
		switch d.status {
		case "DETECTED":
			if _, err := tx.Exec(ctx,
				`UPDATE onchain_deposits SET status='ORPHANED_SHALLOW' WHERE id=$1`, d.id); err != nil {
				return err
			}
		case "CREDITED":
			// 已贷记用户余额 — 独立事务原子冲正 + ORPHANED_DEEP
			// (devnet 级一致性: 冲正先于本回滚事务提交, 本事务失败时
			// 可能出现余额已扣而块仍 CANONICAL 的短窗 — 交付报告记录).
			if err := ix.st.DebitUserBalanceOnOrphan(ctx, d.id); err != nil {
				return fmt.Errorf("debit user balance on orphan %s: %w", d.id, err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE onchain_deposits SET status='ORPHANED_DEEP' WHERE id=$1`, d.id); err != nil {
				return err
			}
		case "SWEPT":
			// 已归集充值被深回滚: 冷金库调拨在孤块链上, 用户余额已贷 —
			// 自动冲正不再安全 (sweep 侧链状态未知), 人工对账.
			ix.raise(Alert{
				Severity: "CRITICAL", Code: "SWEPT_DEPOSIT_REORGED",
				Message: "swept+credited deposit orphaned by deep reorg — manual reconciliation required",
				Detail: fmt.Sprintf("deposit=%s category=%s chain=%d",
					d.id, d.category, ix.cfg.ChainID),
			})
			if _, err := tx.Exec(ctx,
				`UPDATE onchain_deposits SET status='ORPHANED_DEEP' WHERE id=$1`, d.id); err != nil {
				return err
			}
		default: // FINALIZED — 未贷记, 直接孤块化
			if _, err := tx.Exec(ctx,
				`UPDATE onchain_deposits SET status='ORPHANED_DEEP' WHERE id=$1`, d.id); err != nil {
				return err
			}
		}
	}
	return nil
}

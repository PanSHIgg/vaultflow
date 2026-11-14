package txmanager

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// ARCHITECTURE §4.2 Nonce 巡检矩阵.
//
// 对每个 IN_FLIGHT 意图 (nonce=K, wallet=W):
//   ④ K < latest_nonce: 该 Nonce 已被链上消费 — 若本系统全部候选均无回执,
//      说明私钥在系统外被消耗 → NONCE_STOLEN_HALT 熔断停队列 + 紧急告警;
//      若某候选有回执 → 正常入块, 交给 indexer 两段式结算.
//   ② K >= latest_nonce 且全部候选 getTransactionByHash==nil 且 pending_nonce<=K
//      且候选已存活 >= DroppedMinAge → 候选标 DROPPED, 命中幂等槽重签补发
//      (新 attempt_no 行; DROPPED 行本身不可复活);
//   ① K >= latest_nonce 且仍有候选可见但超过 InFlightTimeout 未入块 →
//      同 Nonce 提价 RBF (attempt_no+1);
//   ③ 在途取消走 RequestCancel (pipeline.go).
// ---------------------------------------------------------------------------

// WatchdogTick 执行一轮 Nonce 巡检; 供后台 tick 与测试直接驱动.
func (m *Manager) WatchdogTick(ctx context.Context) error {
	rows, err := m.db.Query(ctx,
		`SELECT id, intent_type, reference_id, chain_id, wallet_address, nonce, status, created_at
		   FROM outbound_tx_intents WHERE status='IN_FLIGHT' ORDER BY created_at`)
	if err != nil {
		return err
	}
	var intents []types.OutboundTxIntent
	for rows.Next() {
		var it types.OutboundTxIntent
		var wa string
		if err := rows.Scan(&it.ID, &it.IntentType, &it.ReferenceID, &it.ChainID,
			&wa, &it.Nonce, &it.Status, &it.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		it.WalletAddress = types.HexToAddress(wa)
		intents = append(intents, it)
	}
	rows.Close()

	for _, it := range intents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := m.inspectIntent(ctx, it); err != nil {
			m.log.Warn("nonce inspect failed",
				zap.String("intent", it.ID.String()), zap.Error(err))
		}
	}
	return nil
}

func (m *Manager) inspectIntent(ctx context.Context, it types.OutboundTxIntent) error {
	k := it.Nonce
	latestNonce, err := m.chain.Nonce(ctx, it.WalletAddress, false)
	if err != nil {
		return fmt.Errorf("latest nonce: %w", err)
	}
	attempts, err := m.ListAttempts(ctx, it.ID)
	if err != nil {
		return err
	}

	if k < latestNonce {
		// Nonce K 已被链上消费 — 核查是否本系统候选被打包.
		// B4: 必须区分 "查询成功且确定无回执" 与 "查询出错证据不足" — RPC
		// 故障期把错误等同无回执会误触发 NONCE_STOLEN_HALT 熔断.
		// 仅当 ≥1 个候选确定性空回执且无任何查询失败时才允许熔断判定.
		provedEmpty, inconclusive := false, false
		for _, a := range attempts {
			if a.TxHash == (types.Hash{}) {
				continue
			}
			rcpt, err := m.chain.Receipt(ctx, a.TxHash)
			if err != nil {
				inconclusive = true
				continue
			}
			if rcpt != nil {
				// 已有候选入块 — 走正常两段式确认, 非带外占用.
				return nil
			}
			provedEmpty = true
		}
		if !provedEmpty || inconclusive {
			if inconclusive {
				m.raise(Alert{
					Severity: "WARNING",
					Code:     "NONCE_STOLEN_INCONCLUSIVE",
					Message: fmt.Sprintf("wallet %s nonce %d consumed out-of-band suspected "+
						"but receipt probes failed — deferring halt to next tick",
						it.WalletAddress.Hex(), k),
					Detail: fmt.Sprintf("intent=%s proved_empty=%v", it.ID, provedEmpty),
				})
			}
			return nil
		}
		return m.haltIntent(ctx, it)
	}

	// K >= latest_nonce — Mempool 可见性矩阵.
	var live []types.OutboundTxAttempt
	for _, a := range attempts {
		if a.Status == types.AttemptBroadcasted {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		// 无在播候选 (极端: 全部 DROPPED 后补发尚未落库) — 补发一笔.
		return m.resendDropped(ctx, it)
	}
	anyPresent := false
	for _, a := range live {
		found, _, err := m.chain.TxPresent(ctx, a.TxHash)
		if err != nil {
			return fmt.Errorf("tx presence: %w", err)
		}
		if found {
			anyPresent = true
			break
		}
	}
	now := m.now()
	if !anyPresent {
		pendingNonce, err := m.chain.Nonce(ctx, it.WalletAddress, true)
		if err != nil {
			return fmt.Errorf("pending nonce: %w", err)
		}
		oldest := oldestBroadcast(live)
		if pendingNonce <= k && now.Sub(oldest) >= m.cfg.DroppedMinAge {
			return m.markDroppedResend(ctx, it, live)
		}
		return nil // 传播窗口期, 下轮再判
	}
	// 仍有候选在 mempool — 超时未入块 → RBF 提价.
	oldest := oldestBroadcast(live)
	if now.Sub(oldest) >= m.cfg.InFlightTimeout {
		return m.rbfBump(ctx, it)
	}
	return nil
}

// oldestBroadcast 最早仍在播候选的广播时间 (RBF/Dropped 计时基准).
func oldestBroadcast(live []types.OutboundTxAttempt) time.Time {
	t := live[0].BroadcastedAt
	for _, a := range live[1:] {
		if a.BroadcastedAt.Before(t) {
			t = a.BroadcastedAt
		}
	}
	return t
}

// haltIntent ④ 私钥带外占用: 意图置 NONCE_STOLEN_HALT, 在播候选标 DROPPED,
// 该钱包出站队列熔断 (QueueIntent 检查), 触发最高优先级告警.
// 两个入口:
//   - 巡检 inspectIntent: IN_FLIGHT 且 K<latest_nonce 且全部候选确定性无回执;
//   - 首播 haltIfQueuedNonceConsumed: QUEUED 意图的已分配 Nonce 首播前被
//     带外消耗 (零 attempt 行即"非本系统消耗"等价证明) — 故守卫放宽为
//     (QUEUED, IN_FLIGHT); QUEUED 行无 BROADCASTED 候选, attempts→DROPPED
//     更新恒为零行 no-op.
func (m *Manager) haltIntent(ctx context.Context, it types.OutboundTxIntent) error {
	err := pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx,
			`UPDATE outbound_tx_intents SET status='NONCE_STOLEN_HALT'
			  WHERE id=$1 AND status IN ('QUEUED','IN_FLIGHT')`, it.ID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return nil // 已被并发处理
		}
		_, err = tx.Exec(ctx,
			`UPDATE outbound_tx_attempts SET status='DROPPED'
			  WHERE intent_id=$1 AND status='BROADCASTED'`, it.ID)
		return err
	})
	if err != nil {
		return err
	}
	m.raise(Alert{
		Severity: "CRITICAL",
		Code:     AlertCodeNonceStolen,
		Message: fmt.Sprintf("hot wallet %s nonce %d consumed out-of-band — outbound queue HALTED",
			it.WalletAddress.Hex(), it.Nonce),
		Detail: fmt.Sprintf("intent=%s chain=%d wallet=%s nonce=%d",
			it.ID, it.ChainID, it.WalletAddress.Hex(), it.Nonce),
	})
	return nil
}

// markDroppedResend ② Mempool 驱逐: 全部在播候选标 DROPPED, 命中幂等槽重签补发.
func (m *Manager) markDroppedResend(ctx context.Context, it types.OutboundTxIntent,
	live []types.OutboundTxAttempt) error {
	if _, err := m.db.Exec(ctx,
		`UPDATE outbound_tx_attempts SET status='DROPPED'
		  WHERE intent_id=$1 AND status='BROADCASTED'`, it.ID); err != nil {
		return err
	}
	m.raise(Alert{
		Severity: "WARNING",
		Code:     "MEMPOOL_DROPPED",
		Message: fmt.Sprintf("tx dropped from mempool (wallet %s nonce %d) — re-signing via idempotent slot",
			it.WalletAddress.Hex(), it.Nonce),
		Detail: fmt.Sprintf("intent=%s dropped_attempts=%d", it.ID, len(live)),
	})
	return m.resendDropped(ctx, it)
}

// resendDropped 以幂等槽重签补发同 Nonce 新 attempt (费率沿用最近一次并提价,
// 保证 tx_hash 与旧 DROPPED 候选不同 — 确定性签名下同参数字节必得同哈希,
// 而 (chain_id, tx_hash) 有全局唯一约束).
func (m *Manager) resendDropped(ctx context.Context, it types.OutboundTxIntent) error {
	p, err := m.rebuildTxParams(ctx, it, types.AttemptKindPrimary)
	if err != nil {
		return fmt.Errorf("rebuild for dropped resend: %w", err)
	}
	if err := m.bumpParams(ctx, it.ID, &p); err != nil {
		return err
	}
	return m.signBroadcastPersist(ctx, it, p, types.AttemptKindPrimary)
}

// rbfBump ① 超时未入块 RBF 提价 attempt_no+1.
func (m *Manager) rbfBump(ctx context.Context, it types.OutboundTxIntent) error {
	p, err := m.rebuildTxParams(ctx, it, types.AttemptKindPrimary)
	if err != nil {
		return fmt.Errorf("rebuild for rbf: %w", err)
	}
	if err := m.bumpParams(ctx, it.ID, &p); err != nil {
		return err
	}
	m.raise(Alert{
		Severity: "INFO",
		Code:     "RBF_BUMP",
		Message: fmt.Sprintf("in-flight timeout, RBF fee bump +%d bps (wallet %s nonce %d)",
			m.cfg.FeeBumpBps, it.WalletAddress.Hex(), it.Nonce),
		Detail: fmt.Sprintf("intent=%s", it.ID),
	})
	return m.signBroadcastPersist(ctx, it, p, types.AttemptKindPrimary)
}

// bumpParams 以最近一次 attempt 费率为基准按 FeeBumpBps 提价
// (保证新候选 tx_hash 与 mempool 中旧候选不同且满足节点替换费率阈值).
func (m *Manager) bumpParams(ctx context.Context, intentID uuid.UUID, p *txParams) error {
	maxFee, maxTip, err := m.topFees(ctx, intentID)
	if err != nil {
		return err
	}
	mul := big.NewInt(10_000 + m.cfg.FeeBumpBps)
	p.maxFee = new(big.Int).Div(new(big.Int).Mul(maxFee, mul), big.NewInt(10_000))
	p.maxTip = new(big.Int).Div(new(big.Int).Mul(maxTip, mul), big.NewInt(10_000))
	return nil
}

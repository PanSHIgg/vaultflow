package txmanager

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// txParams 一笔候选交易的全部可签名参数 (链 ID 与 Nonce 取 intent).
type txParams struct {
	to           types.Address
	value        *big.Int
	calldata     []byte
	gasLimit     uint64
	maxFee       *big.Int
	maxTip       *big.Int
	policy       ports.TxPolicy
	payoutUUID   uuid.UUID // PAYOUT 幂等槽绑定 (其余策略零值)
	retrySeq     uint32
	refundTicket *ports.RefundTicket
}

// BuildUnsignedTx 组装 EIP-2718 Type-2 (EIP-1559) 未签名原始字节:
//
//	0x02 || rlp([chainId, nonce, maxPriorityFeePerGas, maxFeePerGas,
//	             gasLimit, to, value, data, accessList])
//
// 与 docs/GOLDEN_VECTORS.json -> eip2718_unsigned_tx_example 字节级对齐.
func BuildUnsignedTx(chainID uint64, nonce uint64, maxTip, maxFee *big.Int,
	gasLimit uint64, to types.Address, value *big.Int, data []byte) ([]byte, error) {
	if maxTip == nil || maxFee == nil || value == nil {
		return nil, fmt.Errorf("txmanager: unsigned tx requires non-nil fee/value params "+
			"(tip=%v fee=%v value=%v)", maxTip, maxFee, value)
	}
	// 注意: ethtypes.DynamicFeeTx.MarshalBinary 会把 V/R/S 一起编码
	// (尾部多出 0x80*3), 不符合冻结的 9 字段 payload 定义 — 必须手工
	// rlp 仅编码签名字段.
	toAddr := common.Address(to)
	payload := struct {
		ChainID    *big.Int
		Nonce      uint64
		GasTipCap  *big.Int
		GasFeeCap  *big.Int
		Gas        uint64
		To         *common.Address `rlp:"nil"`
		Value      *big.Int
		Data       []byte
		AccessList ethtypes.AccessList
	}{
		ChainID:   new(big.Int).SetUint64(chainID),
		Nonce:     nonce,
		GasTipCap: maxTip,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &toAddr,
		Value:     value,
		Data:      data,
	}
	enc, err := rlp.EncodeToBytes(payload)
	if err != nil {
		return nil, fmt.Errorf("txmanager: marshal unsigned tx: %w", err)
	}
	return append([]byte{0x02}, enc...), nil
}

// policyFor 意图类型 -> Signer 策略 hint.
func policyFor(t types.IntentType) ports.TxPolicy {
	switch t {
	case types.IntentTypeSweepBatch:
		return ports.TxPolicySweep
	case types.IntentTypeEIP3009Relay:
		return ports.TxPolicyEIP3009Relay
	case types.IntentTypeMerchantPayout:
		return ports.TxPolicyPayout
	case types.IntentTypeNonceGapFiller:
		return ports.TxPolicyGapFiller
	}
	return ports.TxPolicyUnspecified
}

// paramsFromRequest 首播参数 = QueueIntentRequest 原样 (calldata 由调用方 ABI 编码).
func paramsFromRequest(req ports.QueueIntentRequest) txParams {
	gas := req.GasLimit
	return txParams{
		to:           req.To,
		value:        req.Value.Big(),
		calldata:     req.Calldata,
		gasLimit:     gas,
		maxFee:       req.MaxFeePerGas.Big(),
		maxTip:       req.MaxPriorityFeePerGas.Big(),
		policy:       policyFor(req.IntentType),
		payoutUUID:   payoutUUIDFor(req),
		retrySeq:     req.RetrySeq,
		refundTicket: req.RefundTicket,
	}
}

func payoutUUIDFor(req ports.QueueIntentRequest) uuid.UUID {
	if req.IntentType == types.IntentTypeMerchantPayout {
		return req.ReferenceID
	}
	return uuid.Nil
}

// fillerParams 同 Nonce GAP_FILLER 取消候选: to==from, value=0, data=∅, gas=21000.
func fillerParams(wallet types.Address, maxFee, maxTip *big.Int) txParams {
	return txParams{
		to:       wallet,
		value:    new(big.Int),
		gasLimit: 21_000,
		maxFee:   maxFee,
		maxTip:   maxTip,
		policy:   ports.TxPolicyGapFiller,
	}
}

// ---------------------------------------------------------------------------
// 首播流水线: QUEUED -> 签名 -> 广播 -> attempt(BROADCASTED) + intent(IN_FLIGHT)
// ---------------------------------------------------------------------------

// BroadcastQueued 扫描 QUEUED 意图执行首播流水线; 返回本轮处理数.
// 导出供测试驱动与 Stop/Start 语义解耦; 后台 drain 亦经此入口.
// 单意图失败不中断批次 — 失败原因经 recordError 落 outbound_tx_intents.
// last_error / last_error_at (B6 诊断透出).
func (m *Manager) BroadcastQueued(ctx context.Context) (int, error) {
	rows, err := m.db.Query(ctx,
		`SELECT id FROM outbound_tx_intents WHERE status='QUEUED'
		  ORDER BY created_at, id LIMIT $1`, m.cfg.MaxDispatchBatch)
	if err != nil {
		return 0, err
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

	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if err := m.broadcastIntent(ctx, id); err != nil {
			m.log.Warn("broadcast intent failed", zap.String("intent", id.String()), zap.Error(err))
		}
		n++
	}
	return n, nil
}

// lastErrorPrefixSignerRejected last_error 前缀 — 标记业务载荷曾被
// vault-signer 明确 RejectCode 拒签, 后续 tick 直接走 GAP_FILLER 取消路径,
// 不再重试注定被拒的原载荷 (B6: 拒签为终态语义, 不静默重试).
const lastErrorPrefixSignerRejected = "SIGNER_REJECTED"

// lastErrorPrefixCancelRequested last_error 前缀 — RequestCancel 对 QUEUED
// 意图的持久化取消标记 (FIX-E/E2). recordErrorText 对 QUEUED 行不再覆盖
// 此前缀 (sticky 护栏), 保证下轮首播一定检出标记并走 GAP_FILLER 填洞,
// 业务载荷永不出签名器.
const lastErrorPrefixCancelRequested = "CANCEL_REQUESTED"

func (m *Manager) broadcastIntent(ctx context.Context, intentID uuid.UUID) error {
	intent, err := m.GetIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if intent.Status != types.IntentQueued {
		return nil // 已被并发推进, 幂等返回
	}
	if err := m.checkHalted(ctx, intent.ChainID, intent.WalletAddress); err != nil {
		return err // 熔断期间 QUEUED 保持悬挂, 不消费 Nonce
	}

	var p txParams
	if v, ok := m.pendingParams.Load(intentID); ok {
		p = paramsFromRequest(v.(ports.QueueIntentRequest))
		if p.gasLimit == 0 {
			p.gasLimit = m.defaultGas(intent.IntentType, 1)
		}
	} else {
		p, err = m.rebuildTxParams(ctx, intent, types.AttemptKindPrimary)
		if err != nil {
			werr := fmt.Errorf("rebuild params for queued intent %s: %w", intentID, err)
			m.recordError(ctx, intentID, werr)
			return werr
		}
	}

	// FIX-E/E2 广播前复核: 取消标记 (RequestCancel 对 QUEUED 受理) 或
	// 关联 merchant_payouts 已 CANCELLED_UNLOCKED (QueueIntent→首播窗口
	// 内 CancelPayout 竞速获胜) → 同 Nonce GAP_FILLER 填洞, 业务载荷
	// 绝不出签名器. 注意: 该复核与 indexer settlePayoutSuccess 的
	// 迁移守卫互为纵深 — 即使此检查与 CancelPayout 提交恰好交错, 结算侧
	// 仍兜底跳过执行腿, 绝无 PAYOUT_LOCKED 双扣.
	if reason := m.cancelBroadcastReason(ctx, intent); reason != "" {
		return m.broadcastCancelFiller(ctx, intent, p, reason)
	}
	// 业务载荷已被 signer 明确拒签 → 跳过原载荷, 直接重试 GAP_FILLER 取消
	// (B6: 首轮 cancelRejectedIntent 已告警, 这里不再重复告警).
	if m.wasSignerRejected(ctx, intentID) {
		return m.cancelRejectedIntent(ctx, intent, p, nil)
	}
	return m.signBroadcastPersist(ctx, intent, p, types.AttemptKindPrimary)
}

// cancelBroadcastReason 首播前业务/运维取消复核 (E2), 返回非空原因串即须
// filler 化:
//
//	(a) last_error 带 CANCEL_REQUESTED 标记 — RequestCancel QUEUED 受理;
//	(b) MERCHANT_PAYOUT 关联 merchant_payouts 已 CANCELLED_UNLOCKED —
//	    CancelPayout 在 QueueIntent→首播窗口竞速获胜 (快照读: 本检查与
//	    CancelPayout 提交的微秒级交错由 indexer 结算侧迁移守卫兜底).
func (m *Manager) cancelBroadcastReason(ctx context.Context, intent types.OutboundTxIntent) string {
	var le *string
	if err := m.db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, intent.ID).Scan(&le); err == nil &&
		le != nil && strings.HasPrefix(*le, lastErrorPrefixCancelRequested) {
		return *le
	}
	if intent.IntentType == types.IntentTypeMerchantPayout {
		var st string
		if err := m.db.QueryRow(ctx,
			`SELECT status FROM merchant_payouts WHERE id=$1`, intent.ReferenceID).Scan(&st); err == nil &&
			types.PayoutStatus(st) == types.PayoutCancelledUnlocked {
			return fmt.Sprintf("%s: payout %s is CANCELLED_UNLOCKED at broadcast",
				lastErrorPrefixCancelRequested, intent.ReferenceID)
		}
	}
	return ""
}

// broadcastCancelFiller 首播路径取消候选广播: QUEUED 意图直接以同 Nonce
// GAP_FILLER 填洞 (费率沿用原业务参数), 意图进入正常在途生命周期
// (IN_FLIGHT → CANCELLED_BY_FILLER), nonce 洞不留, 关联业务行经
// indexer 结算联动归位.
func (m *Manager) broadcastCancelFiller(ctx context.Context, intent types.OutboundTxIntent,
	p txParams, reason string) error {
	m.recordErrorText(ctx, intent.ID, reason)
	m.raise(Alert{
		Severity: "WARNING",
		Code:     "INTENT_CANCELLED_PRE_BROADCAST",
		Message: "queued intent cancelled before first broadcast — " +
			"emitting same-nonce GAP_FILLER instead of business payload",
		Detail: fmt.Sprintf("intent=%s type=%s wallet=%s nonce=%d reason=%s",
			intent.ID, intent.IntentType, intent.WalletAddress.Hex(), intent.Nonce, reason),
	})
	fp := fillerParams(intent.WalletAddress, p.maxFee, p.maxTip)
	if err := m.bumpParams(ctx, intent.ID, &fp); err != nil {
		fp.maxFee, fp.maxTip = p.maxFee, p.maxTip // 无历史 attempt — 沿用首播费率
	}
	return m.signBroadcastPersist(ctx, intent, fp, types.AttemptKindGapFiller)
}

// wasSignerRejected 该意图上一轮是否已被 signer 明确拒签 (last_error 前缀判定).
func (m *Manager) wasSignerRejected(ctx context.Context, intentID uuid.UUID) bool {
	var le *string
	if err := m.db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, intentID).Scan(&le); err != nil {
		return false
	}
	return le != nil && strings.HasPrefix(*le, lastErrorPrefixSignerRejected)
}

// recordError B6 — 把流水线失败原因持久化到 intent.last_error (监控/排障透出).
func (m *Manager) recordError(ctx context.Context, intentID uuid.UUID, err error) {
	m.recordErrorText(ctx, intentID, err.Error())
}

func (m *Manager) recordErrorText(ctx context.Context, intentID uuid.UUID, msg string) {
	// E2 sticky 护栏: QUEUED 行的 CANCEL_REQUESTED 标记是"业务载荷绝不出
	// 签名器"的持久承诺 — 后续瞬时失败若覆写 last_error, 下轮 tick 检出
	// 不到标记即会误发原业务载荷, 故标记在 QUEUED 期间不可被覆盖
	// (意图一旦离开 QUEUED 即恢复常规诊断写入).
	if _, err := m.db.Exec(ctx,
		`UPDATE outbound_tx_intents SET last_error=$2, last_error_at=now()
		  WHERE id=$1
		    AND NOT (status='QUEUED' AND COALESCE(last_error,'') LIKE 'CANCEL_REQUESTED:%')`,
		intentID, msg); err != nil {
		m.log.Warn("persist last_error failed",
			zap.String("intent", intentID.String()), zap.Error(err))
	}
}

// signBroadcastPersist 签名 -> 广播 -> 同事务写 attempt 与意图 IN_FLIGHT.
// 重发路径共用 (RBF 提价 / DROPPED 补发 / GAP_FILLER / 回滚重广播):
// attempt_no 恒取 max(attempt_no)+1 — 新候选永不复用旧行 (DROPPED 不可复活).
//
// B6 错误透出: 每类失败均落 intent.last_error; vault-signer 明确 RejectCode
// 拒签属终态语义 — PRIMARY 载荷改发同 Nonce GAP_FILLER 取消候选填洞,
// GAP_FILLER 自身再被拒则意图落 REJECTED_BY_SIGNER + CRITICAL 告警.
func (m *Manager) signBroadcastPersist(ctx context.Context, intent types.OutboundTxIntent,
	p txParams, kind types.AttemptKind) error {

	unsigned, err := BuildUnsignedTx(uint64(intent.ChainID), intent.Nonce,
		p.maxTip, p.maxFee, p.gasLimit, p.to, p.value, p.calldata)
	if err != nil {
		m.recordError(ctx, intent.ID, err)
		return err
	}
	resp, err := m.signer.SignTx(ctx, ports.SignTxRequest{
		ChainID:      uint64(intent.ChainID),
		UnsignedTx:   unsigned,
		PolicyHint:   p.policy,
		PayoutUUID:   p.payoutUUID,
		RetrySeq:     p.retrySeq,
		RefundTicket: p.refundTicket,
	})
	if err != nil {
		m.recordError(ctx, intent.ID, err) // signer RPC 瞬时错误 — 可重试
		return err                         // ErrSignerUnavailable 已含上下文
	}
	if resp.Rejected != nil {
		if kind == types.AttemptKindPrimary {
			return m.cancelRejectedIntent(ctx, intent, p, resp.Rejected)
		}
		return m.markSignerRejected(ctx, intent, kind, resp.Rejected)
	}
	if resp.Signed == nil {
		err := errors.New("txmanager: empty signer result")
		m.recordError(ctx, intent.ID, err)
		return err
	}
	raw := resp.Signed.RawTx
	txHash := types.Hash(crypto.Keccak256Hash(raw))
	if resp.Signed.TxHash != (types.Hash{}) && resp.Signed.TxHash != txHash {
		m.log.Warn("signer tx_hash mismatch, trusting local keccak256",
			zap.String("signer", resp.Signed.TxHash.Hex()), zap.String("local", txHash.Hex()))
	}
	if _, err := m.chain.SendRawTx(ctx, raw); err != nil && !chain.IsAlreadyKnownError(err) {
		werr := fmt.Errorf("txmanager: broadcast %s: %w", txHash.Hex(), err)
		m.recordError(ctx, intent.ID, werr)
		m.haltIfQueuedNonceConsumed(ctx, intent)
		return werr
	}

	// 落库: 新 attempt(BROADCASTED) + 意图 IN_FLIGHT (首次) — 单事务原子.
	err = pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		var maxNo int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(attempt_no),0) FROM outbound_tx_attempts WHERE intent_id=$1`,
			intent.ID).Scan(&maxNo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbound_tx_attempts
			   (intent_id, attempt_no, attempt_kind, chain_id, tx_hash,
			    max_fee_per_gas, max_priority_fee_per_gas, status)
			 VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,'BROADCASTED')`,
			intent.ID, maxNo+1, string(kind), int64(intent.ChainID), txHashHex(txHash),
			p.maxFee.String(), p.maxTip.String()); err != nil {
			return err
		}
		res, err := tx.Exec(ctx,
			`UPDATE outbound_tx_intents SET status='IN_FLIGHT'
			  WHERE id=$1 AND status IN ('QUEUED','IN_FLIGHT')`, intent.ID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return fmt.Errorf("txmanager: intent %s left dispatchable state", intent.ID)
		}
		return nil
	})
	if err != nil {
		werr := fmt.Errorf("txmanager: persist attempt: %w", err)
		m.recordError(ctx, intent.ID, werr)
		return werr
	}
	return nil
}

// haltIfQueuedNonceConsumed QUEUED 首播失败后的 §4.2④ 等价判定: SendRawTx 已
// 失败, 探测链上 latest(mined) nonce — 若 latestNonce > intent.Nonce 则该
// Nonce 已被 canonical 链消费. QUEUED 意图此刻 outbound_tx_attempts 恒为零行
// (首行 attempt 仅由本函数下方持久化步骤插入), "消耗非本系统候选"不证自明,
// 等价于巡检④的 provedEmpty 证据, 直接 haltIntent 熔断停队列.
// (修复: 此前 QUEUED 意图 nonce 在分配至首播间被带外消耗时, SendRawTx
// 持续撞 "nonce too low" 无限重试 — WatchdogTick 只扫 IN_FLIGHT, 熔断永不触发.)
//
// 严格限定 QUEUED: IN_FLIGHT 重发路径 (RBF 提价 / DROPPED 补发 / GAP_FILLER /
// §4.3 回滚重广播) 已有自有候选, 必须由 inspectIntent 的 Receipt provedEmpty
// 核查区分"自有候选入块"与"带外消耗" — 否则自有入块会误触熔断, 巡检仍是唯一
// 裁定方. 探测本身失败则 fail-open 不熔断 (与 watchdog inconclusive 哲学一致),
// 意图留 QUEUED 待下轮重试.
func (m *Manager) haltIfQueuedNonceConsumed(ctx context.Context, intent types.OutboundTxIntent) {
	if intent.Status != types.IntentQueued {
		return
	}
	latestNonce, err := m.chain.Nonce(ctx, intent.WalletAddress, false)
	if err != nil {
		m.log.Warn("queued nonce-consumption probe failed; fail-open retry",
			zap.String("intent", intent.ID.String()), zap.Error(err))
		return
	}
	if latestNonce <= intent.Nonce {
		return // Nonce 未被链上消费 — 普通瞬时失败, 常规重试
	}
	if err := m.haltIntent(ctx, intent); err != nil {
		m.log.Warn("halt queued intent failed",
			zap.String("intent", intent.ID.String()), zap.Error(err))
	}
}

// cancelRejectedIntent B6 — 业务载荷被 signer 明确拒签: 该 Nonce 已分配不可
// 悬空 (否则 nonce 空洞级联死锁, 见 B1), 改发同 Nonce GAP_FILLER 取消候选
// (0 ETH 自转账, GAP_FILLER_POLICY 属 signer 必放行策略) 把洞填上; 意图随后
// 正常走 IN_FLIGHT -> CANCELLED_BY_FILLER 终态, 关联业务行经既有结算联动
// (payout -> FAILED_RETRYABLE / sweep batch -> reconcile 释放).
// rej == nil 表示"此前已拒签"的重试 tick — 仅落 last_error 不重复告警.
func (m *Manager) cancelRejectedIntent(ctx context.Context, intent types.OutboundTxIntent,
	p txParams, rej *ports.Rejection) error {

	reason := "SIGNER_REJECTED previously"
	if rej != nil {
		reason = fmt.Sprintf("%s: code=%d reason=%s", lastErrorPrefixSignerRejected,
			rej.Code, rej.Reason)
		m.raise(Alert{
			Severity: "WARNING",
			Code:     "SIGNER_REJECTED",
			Message: "vault-signer rejected outbound tx payload — " +
				"replacing with same-nonce GAP_FILLER cancel candidate",
			Detail: fmt.Sprintf("intent=%s type=%s wallet=%s nonce=%d %s",
				intent.ID, intent.IntentType, intent.WalletAddress.Hex(), intent.Nonce, reason),
		})
	}
	m.recordErrorText(ctx, intent.ID, reason)

	fp := fillerParams(intent.WalletAddress, p.maxFee, p.maxTip)
	if err := m.bumpParams(ctx, intent.ID, &fp); err != nil {
		// 无历史 attempt (QUEUED 首播被拒) — 沿用首播费率; filler 只为填洞.
		fp.maxFee, fp.maxTip = p.maxFee, p.maxTip
	}
	return m.signBroadcastPersist(ctx, intent, fp, types.AttemptKindGapFiller)
}

// markSignerRejected B6 — signer 连 GAP_FILLER 兜底也拒签 (策略配置错误或
// 签名服务异常): 意图落显式终态 REJECTED_BY_SIGNER + last_error + CRITICAL
// 告警; MERCHANT_PAYOUT 关联单同事务回 FAILED_RETRYABLE 防资金悬挂.
// 残余风险 (报告记录): 该 Nonce 洞仍需运维处置 (修配置后人工 filler 或钱包
// 级停队列), 后继意图广播将排队等该 Nonce.
func (m *Manager) markSignerRejected(ctx context.Context, intent types.OutboundTxIntent,
	kind types.AttemptKind, rej *ports.Rejection) error {

	msg := fmt.Sprintf("%s: kind=%s code=%d reason=%s",
		lastErrorPrefixSignerRejected, kind, rej.Code, rej.Reason)
	err := pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_intents
			    SET status='REJECTED_BY_SIGNER', last_error=$2, last_error_at=now()
			  WHERE id=$1 AND status IN ('QUEUED','IN_FLIGHT')`, intent.ID, msg); err != nil {
			return err
		}
		if intent.IntentType == types.IntentTypeMerchantPayout {
			// 业务行兜底: payout 可重试 (与 indexer failPayoutRetryable 同语义).
			if _, err := tx.Exec(ctx,
				`UPDATE merchant_payouts SET status='FAILED_RETRYABLE'
				  WHERE id=$1 AND status='IN_FLIGHT'`, intent.ReferenceID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mark signer-rejected: %w", err)
	}
	m.raise(Alert{
		Severity: "CRITICAL",
		Code:     "SIGNER_REJECTED_TERMINAL",
		Message: "vault-signer rejected even GAP_FILLER fallback — intent parked " +
			"REJECTED_BY_SIGNER; nonce hole requires operator intervention",
		Detail: fmt.Sprintf("intent=%s type=%s wallet=%s nonce=%d %s",
			intent.ID, intent.IntentType, intent.WalletAddress.Hex(), intent.Nonce, msg),
	})
	return fmt.Errorf("%w: %s", ports.ErrSignerRejected, msg)
}

// RequestCancel 对意图请求同 Nonce GAP_FILLER 取消 (§4.2 ③ + FIX-E/E2).
//   - IN_FLIGHT: 立即追加签名广播同 Nonce GAP_FILLER 候选 (命中 Signer
//     双字段幂等槽置 filler_signed, 不覆盖业务授权字段);
//   - QUEUED (E2): 尚未首播, 写持久取消标记 — 下轮 BroadcastQueued
//     检出后以同 Nonce GAP_FILLER 填洞, 业务载荷永不出签名器;
//   - 其余状态返回 ErrInvalidTransition.
func (m *Manager) RequestCancel(ctx context.Context, intentID uuid.UUID) error {
	intent, err := m.GetIntent(ctx, intentID)
	if err != nil {
		return err
	}
	switch intent.Status {
	case types.IntentQueued:
		m.recordErrorText(ctx, intentID, fmt.Sprintf(
			"%s: cancel accepted pre-broadcast; same-nonce GAP_FILLER pending",
			lastErrorPrefixCancelRequested))
		m.kick()
		return nil
	case types.IntentInFlight:
		// 既有路径: 立即签名广播 filler 候选.
	default:
		return fmt.Errorf("%w: intent %s is %s", ports.ErrInvalidTransition, intentID, intent.Status)
	}
	maxFee, maxTip, err := m.topFees(ctx, intentID)
	if err != nil {
		return err
	}
	bump := func(v *big.Int) *big.Int {
		out := new(big.Int).Mul(v, big.NewInt(10_000+m.cfg.FeeBumpBps))
		return out.Div(out, big.NewInt(10_000))
	}
	return m.signBroadcastPersist(ctx, intent,
		fillerParams(intent.WalletAddress, bump(maxFee), bump(maxTip)),
		types.AttemptKindGapFiller)
}

// topFees 当前意图最高一档费率 (RBF/取消的提价基准).
func (m *Manager) topFees(ctx context.Context, intentID uuid.UUID) (maxFee, maxTip *big.Int, err error) {
	var f, t string
	err = m.db.QueryRow(ctx,
		`SELECT max_fee_per_gas::text, max_priority_fee_per_gas::text
		   FROM outbound_tx_attempts WHERE intent_id=$1 ORDER BY attempt_no DESC LIMIT 1`,
		intentID).Scan(&f, &t)
	if err != nil {
		return nil, nil, err
	}
	return types.MustParseU256(f).Big(), types.MustParseU256(t).Big(), nil
}

// RebroadcastIntent §4.3 LIFO 深/浅回滚后重广播: 对意图下全部 BROADCASTED
// 候选按各自费率参数重签重发 (k256 确定性签名重放出同 tx_hash, 不落新 attempt).
// indexer 在 Attempt 重置 BROADCASTED / SUPERSEDED→BROADCASTED 后调用.
func (m *Manager) RebroadcastIntent(ctx context.Context, intentID uuid.UUID) error {
	intent, err := m.GetIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if intent.Status != types.IntentInFlight {
		return nil
	}
	attempts, err := m.ListAttempts(ctx, intentID)
	if err != nil {
		return err
	}
	var reb int
	for _, a := range attempts {
		if a.Status != types.AttemptBroadcasted {
			continue
		}
		p, err := m.rebuildTxParams(ctx, intent, a.Kind)
		if err != nil {
			m.log.Warn("rebroadcast rebuild failed",
				zap.String("intent", intentID.String()), zap.Error(err))
			continue
		}
		p.maxFee = a.MaxFeePerGas.Big()
		p.maxTip = a.MaxPriorityFeePerGas.Big()
		unsigned, err := BuildUnsignedTx(uint64(intent.ChainID), intent.Nonce,
			p.maxTip, p.maxFee, p.gasLimit, p.to, p.value, p.calldata)
		if err != nil {
			return err
		}
		resp, err := m.signer.SignTx(ctx, ports.SignTxRequest{
			ChainID:      uint64(intent.ChainID),
			UnsignedTx:   unsigned,
			PolicyHint:   p.policy,
			PayoutUUID:   p.payoutUUID,
			RetrySeq:     p.retrySeq,
			RefundTicket: p.refundTicket,
		})
		if err != nil || resp.Signed == nil {
			m.log.Warn("rebroadcast resign failed",
				zap.String("intent", intentID.String()), zap.Error(err))
			continue
		}
		got := types.Hash(crypto.Keccak256Hash(resp.Signed.RawTx))
		if got != a.TxHash {
			// 签名参数或密钥漂移 — 记告警但继续 (tx_hash 不一致不影响安全, 仅观测).
			m.raise(Alert{Severity: "WARNING", Code: "REBROADCAST_HASH_DRIFT",
				Message: "rebroadcast produced different tx_hash",
				Detail:  fmt.Sprintf("intent=%s attempt=%d want=%s got=%s", intentID, a.AttemptNo, a.TxHash.Hex(), got.Hex())})
		}
		if _, err := m.chain.SendRawTx(ctx, resp.Signed.RawTx); err != nil && !chain.IsAlreadyKnownError(err) {
			m.log.Warn("rebroadcast send failed", zap.Error(err))
			continue
		}
		reb++
	}
	m.log.Info("rebroadcast after rollback", zap.String("intent", intentID.String()), zap.Int("attempts", reb))
	return nil
}

// defaultGas 各意图类型默认 gas 上限 (QueueIntentRequest.GasLimit==0 时启用).
func (m *Manager) defaultGas(t types.IntentType, itemCount int) uint64 {
	switch t {
	case types.IntentTypeSweepBatch:
		return m.cfg.GasLimitSweepBase + uint64(itemCount)*m.cfg.GasLimitSweepItem
	case types.IntentTypeMerchantPayout:
		return m.cfg.GasLimitPayout
	case types.IntentTypeEIP3009Relay:
		return m.cfg.GasLimitRelay
	case types.IntentTypeNonceGapFiller:
		return 21_000
	}
	return 100_000
}

func txHashHex(h types.Hash) string {
	s := h.Hex()
	out := make([]byte, 0, len(s))
	out = append(out, s[:2]...)
	for i := 2; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

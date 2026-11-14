package types

// 四层正交状态机 (ARCHITECTURE.md §5.1 ~ §5.4) + 出站 Attempt 状态机.
// 全部状态名与 transitions 严格对齐文档 mermaid 图, 作为 W3/W4/W5 共享的
// 显式白名单迁移表. 未列入白名单的迁移一律 CanTransition == false.

// canTransit 通用白名单迁移表查询.
func canTransit[S ~string](table map[S]map[S]bool, from, to S) bool {
	return table[from][to]
}

// ---------------------------------------------------------------------------
// §5.1 链上入站记录状态机 onchain_deposits.status (4 态)
// ---------------------------------------------------------------------------

// DepositStatus 链上入站充值记录状态.
type DepositStatus string

const (
	// DepositDetected 已捕获链上 Transfer 日志, 等待越过 safe_head.
	DepositDetected DepositStatus = "DETECTED"
	// DepositOrphanedShallow 浅分叉孤块作废 (未入账); 若新链重打包以新 block_hash 另行入库.
	DepositOrphanedShallow DepositStatus = "ORPHANED_SHALLOW"
	// DepositFinalized 已过 safe_head 完成入账.
	DepositFinalized DepositStatus = "FINALIZED"
	// DepositOrphanedDeep 深分叉孤块: 已完成 LIFO 红冲.
	DepositOrphanedDeep DepositStatus = "ORPHANED_DEEP"
)

var depositTransitions = map[DepositStatus]map[DepositStatus]bool{
	DepositDetected: {
		DepositFinalized:       true, // block_number <= safe_head_height
		DepositOrphanedShallow: true, // LIFO 回退未确认孤块
	},
	DepositFinalized: {
		DepositOrphanedDeep: true, // LIFO 深度回滚并写入红冲分录
	},
}

// CanTransition 校验 from -> to 是否在 §5.1 白名单内.
func (s DepositStatus) CanTransition(to DepositStatus) bool {
	return canTransit(depositTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s DepositStatus) String() string { return string(s) }

// IsTerminal 是否为终态 (孤块作废两态均为终态; DETECTED/FINALIZED 可被回滚).
func (s DepositStatus) IsTerminal() bool {
	return s == DepositOrphanedShallow || s == DepositOrphanedDeep
}

// ---------------------------------------------------------------------------
// §5.2 业务支付订单状态机 payment_orders.status (11 态)
// ---------------------------------------------------------------------------

// OrderStatus 支付订单状态.
type OrderStatus string

const (
	// OrderAwaitingPayment 等待同币种入账 (1:1 绑定专属 DepositAddr).
	OrderAwaitingPayment OrderStatus = "AWAITING_PAYMENT"
	// OrderPartiallyPaid 部分确认: 暂挂 ORDER_ESCROW_LIAB.
	OrderPartiallyPaid OrderStatus = "PARTIALLY_PAID"
	// OrderPaid 足额完结: 业务终态, 后续误充不改此状态.
	OrderPaid OrderStatus = "PAID"
	// OrderOverpaid 超额完结: 应付已结算, 溢缴挂 SUSPENSE_OVERPAY_LIAB.
	OrderOverpaid OrderStatus = "OVERPAID"
	// OrderExpired 超时零有效入账.
	OrderExpired OrderStatus = "EXPIRED"
	// OrderUnderpaidExpiredReview 少付超时挂起: 转挂 SUSPENSE_REFUNDABLE_LIAB.
	OrderUnderpaidExpiredReview OrderStatus = "UNDERPAID_EXPIRED_REVIEW"
	// OrderLatePaidReview 过期后入账挂起: 转挂 SUSPENSE_REFUNDABLE_LIAB.
	OrderLatePaidReview OrderStatus = "LATE_PAID_REVIEW"
	// OrderRefundInProgress 全额异常单退款单执行中.
	OrderRefundInProgress OrderStatus = "REFUND_IN_PROGRESS"
	// OrderRefunded 异常单全额退回完结: 终态.
	OrderRefunded OrderStatus = "REFUNDED"
	// OrderSettledReversed 深回滚红冲追偿中.
	OrderSettledReversed OrderStatus = "SETTLED_REVERSED"
	// OrderReversalResolved 红冲缺口补齐/核销完结: 终态.
	OrderReversalResolved OrderStatus = "REVERSAL_RESOLVED"
)

var orderTransitions = map[OrderStatus]map[OrderStatus]bool{
	OrderAwaitingPayment: {
		OrderPartiallyPaid:  true, // 未过期, 同币种累计 FINALIZED < 应付下限
		OrderPaid:           true, // 未过期, 命中应付容差
		OrderOverpaid:       true, // 未过期, 超出应付上限
		OrderExpired:        true, // 周期重评: 过 expires_at 且无同币种在途/已确认充值
		OrderLatePaidReview: true, // 过 expires_at 后同币种充值 FINALIZED
	},
	OrderPartiallyPaid: {
		OrderPartiallyPaid:          true, // 未过期追加仍不足 (或深回滚后有效累计仍 > 0)
		OrderAwaitingPayment:        true, // 关联充值全部 ORPHANED_DEEP 归零且未过期
		OrderPaid:                   true, // 未过期追加后达标
		OrderOverpaid:               true, // 未过期追加后超上限
		OrderUnderpaidExpiredReview: true, // 过 expires_at 且无在途充值仍未补齐
		OrderLatePaidReview:         true, // 过 expires_at 后在途追加 FINALIZED
	},
	OrderExpired: {
		OrderLatePaidReview: true, // 过期单收到迟到同币种 FINALIZED 转账
	},
	OrderUnderpaidExpiredReview: {
		OrderLatePaidReview:   true, // 挂起期间收到追加同币种 FINALIZED 转账
		OrderExpired:          true, // 关联充值 ORPHANED_DEEP 全部冲销归零
		OrderPaid:             true, // 商户确认折让接受实收额 (模板 4)
		OrderRefundInProgress: true, // 创建关联 CUSTOMER_REFUND 退款单
	},
	OrderLatePaidReview: {
		OrderExpired:                true, // 关联充值 ORPHANED_DEEP 全部冲销归零
		OrderUnderpaidExpiredReview: true, // 部分迟到充值 ORPHANED_DEEP 仍有残余
		OrderPaid:                   true, // 商户确认接受迟到款履约 (模板 4)
		OrderRefundInProgress:       true, // 创建关联 CUSTOMER_REFUND 退款单
	},
	OrderRefundInProgress: {
		OrderRefunded:       true, // 关联退款单 COMPLETED
		OrderLatePaidReview: true, // 管理员取消未成行退款单退回人工决策
	},
	OrderOverpaid: {
		OrderPaid:            true, // 溢缴差额对应独立 CUSTOMER_REFUND 退款单 COMPLETED
		OrderSettledReversed: true, // 关联充值 ORPHANED_DEEP 红冲
	},
	OrderPaid: {
		OrderSettledReversed: true, // 关联充值 ORPHANED_DEEP 红冲
	},
	OrderSettledReversed: {
		OrderReversalResolved: true, // 客户补款或运营核销清零追偿应收款
	},
}

// CanTransition 校验 from -> to 是否在 §5.2 白名单内.
func (s OrderStatus) CanTransition(to OrderStatus) bool {
	return canTransit(orderTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s OrderStatus) String() string { return string(s) }

// OrderTerminalStatuses §4.1 分支 C 判定的"已完结终态"集合:
// 处于此集合中的订单再收到同币种转账, 一律记 SUSPENSE_REFUNDABLE_LIAB
// 且不改变原订单状态. 注意 PAID/OVERPAID 在状态图上仍有深回滚出边,
// "终态"此处指业务完结语义而非图终态; EXPIRED 不在此列 (迟到款转 LATE_PAID_REVIEW).
var OrderTerminalStatuses = map[OrderStatus]bool{
	OrderPaid:             true,
	OrderOverpaid:         true,
	OrderRefunded:         true,
	OrderReversalResolved: true,
}

// IsBusinessTerminal 是否属于分支 C 判定的业务终态.
func (s OrderStatus) IsBusinessTerminal() bool { return OrderTerminalStatuses[s] }

// ---------------------------------------------------------------------------
// §5.3 出站交易意图状态机 outbound_tx_intents.status (8 态)
// ---------------------------------------------------------------------------

// TxIntentStatus 出站交易意图状态.
type TxIntentStatus string

const (
	// IntentQueued 已锁定 Nonce 待首播 (同事务分配连续 Nonce 落盘).
	IntentQueued TxIntentStatus = "QUEUED"
	// IntentInFlight 在途: 支持 RBF / Dropped 补发 / GAP_FILLER 替换.
	IntentInFlight TxIntentStatus = "IN_FLIGHT"
	// IntentIncludedPendingSafe 已入块, 等待越过 safe_head.
	IntentIncludedPendingSafe TxIntentStatus = "INCLUDED_PENDING_SAFE"
	// IntentConfirmed 达 safe_head, 主业务 Attempt receipt_status == 1.
	IntentConfirmed TxIntentStatus = "CONFIRMED"
	// IntentFailedReverted 达 safe_head, Attempt receipt_status == 0, 已记 Gas.
	IntentFailedReverted TxIntentStatus = "FAILED_REVERTED"
	// IntentCancelledByFiller 达 safe_head, GAP_FILLER Attempt receipt_status == 1.
	IntentCancelledByFiller TxIntentStatus = "CANCELLED_BY_FILLER"
	// IntentNonceStolenHalt 检测到私钥带外消耗该 Nonce: 熔断停机告警.
	IntentNonceStolenHalt TxIntentStatus = "NONCE_STOLEN_HALT"
	// IntentRejectedBySigner 业务载荷被 vault-signer 明确拒签且 GAP_FILLER
	// 兜底亦失败 — 显式终态 (002_intent_payload.sql 已入库; FIX-E/E4 Go 侧
	// 对齐, 白名单无出边即终态, 该 Nonce 洞需人工处置).
	IntentRejectedBySigner TxIntentStatus = "REJECTED_BY_SIGNER"
)

var txIntentTransitions = map[TxIntentStatus]map[TxIntentStatus]bool{
	IntentQueued: {
		IntentInFlight:        true, // 首笔签名广播
		IntentNonceStolenHalt: true, // 已分配 Nonce 首播前被带外消耗
	},
	IntentInFlight: {
		IntentInFlight:            true, // RBF 提价 / Dropped 补发 / 追加同 Nonce GAP_FILLER
		IntentNonceStolenHalt:     true, // K < latest_nonce 且本系统全部候选均无回执
		IntentIncludedPendingSafe: true, // 任一候选被打包入块
	},
	IntentIncludedPendingSafe: {
		IntentInFlight:          true, // 浅分叉回滚该块, Attempt 重置 BROADCASTED 重播
		IntentConfirmed:         true, // 达 safe_head, PRIMARY 且 status==1
		IntentCancelledByFiller: true, // 达 safe_head, GAP_FILLER 且 status==1
		IntentFailedReverted:    true, // 达 safe_head 且 status==0
	},
	IntentConfirmed: {
		IntentInFlight: true, // 极端深分叉 LIFO 红冲后重置回在途重播
	},
	IntentFailedReverted: {
		IntentInFlight: true, // 极端深分叉 LIFO 红冲后重置回在途重播
	},
	IntentCancelledByFiller: {
		IntentInFlight: true, // 极端深分叉 LIFO 红冲后重置回在途重播
	},
}

// CanTransition 校验 from -> to 是否在 §5.3 白名单内.
func (s TxIntentStatus) CanTransition(to TxIntentStatus) bool {
	return canTransit(txIntentTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s TxIntentStatus) String() string { return string(s) }

// ---------------------------------------------------------------------------
// outbound_tx_attempts.status (5 态, §4.2/§4.3 迁移规则 + §6.3 CHECK)
// ---------------------------------------------------------------------------

// TxAttemptStatus 同 Nonce 单候选交易状态.
type TxAttemptStatus string

const (
	// AttemptBroadcasted 已签名广播, 等待入块.
	AttemptBroadcasted TxAttemptStatus = "BROADCASTED"
	// AttemptIncludedUnsafe 已入块但未过 safe_head (暂不记账).
	AttemptIncludedUnsafe TxAttemptStatus = "INCLUDED_UNSAFE"
	// AttemptFinalized 已过 safe_head (已记 GAS_SETTLE 等业务分录).
	AttemptFinalized TxAttemptStatus = "FINALIZED"
	// AttemptSuperseded 同 Nonce 兄弟候选先入块, 本候选被取代.
	AttemptSuperseded TxAttemptStatus = "SUPERSEDED"
	// AttemptDropped Mempool 驱逐 (全部候选无链上记录且 pending_nonce <= K).
	AttemptDropped TxAttemptStatus = "DROPPED"
)

var txAttemptTransitions = map[TxAttemptStatus]map[TxAttemptStatus]bool{
	AttemptBroadcasted: {
		AttemptIncludedUnsafe: true, // 被打包入块
		AttemptSuperseded:     true, // 同 Nonce 兄弟候选先入块
		AttemptDropped:        true, // Mempool 驱逐检测命中
	},
	AttemptIncludedUnsafe: {
		AttemptFinalized:   true, // 越过 safe_head, 两段式确认记账
		AttemptBroadcasted: true, // 浅分叉回滚: 清空块信息重置回广播态
	},
	AttemptFinalized: {
		AttemptBroadcasted: true, // 极端深分叉: LIFO 红冲后重置回广播态重播
	},
	AttemptSuperseded: {
		AttemptBroadcasted: true, // 深回滚时被取代的兄弟候选一并恢复重播
	},
}

// CanTransition 校验 from -> to 是否在 Attempt 白名单内.
func (s TxAttemptStatus) CanTransition(to TxAttemptStatus) bool {
	return canTransit(txAttemptTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s TxAttemptStatus) String() string { return string(s) }

// ---------------------------------------------------------------------------
// §5.4 商户提现与客户退款单状态机 merchant_payouts.status (5 态)
// ---------------------------------------------------------------------------

// PayoutStatus 提现/退款单状态.
type PayoutStatus string

const (
	// PayoutLockedQueued 已冻结来源科目, 待分配出站 Nonce.
	PayoutLockedQueued PayoutStatus = "LOCKED_QUEUED"
	// PayoutInFlight 已关联出站 Intent 广播在途.
	PayoutInFlight PayoutStatus = "IN_FLIGHT"
	// PayoutCompleted 出站 Intent 达 safe_head CONFIRMED.
	PayoutCompleted PayoutStatus = "COMPLETED"
	// PayoutFailedRetryable 出站 Intent FAILED_REVERTED 或 Filler 取消, 余额仍锁定待重排.
	PayoutFailedRetryable PayoutStatus = "FAILED_RETRYABLE"
	// PayoutCancelledUnlocked 取消提现/退款, 资金原路解冻回来源负债科目.
	PayoutCancelledUnlocked PayoutStatus = "CANCELLED_UNLOCKED"
)

var payoutTransitions = map[PayoutStatus]map[PayoutStatus]bool{
	PayoutLockedQueued: {
		PayoutInFlight:          true, // 分配新 Nonce 创建 outbound_tx_intent
		PayoutCancelledUnlocked: true, // 广播前取消并解冻余额
	},
	PayoutInFlight: {
		PayoutCompleted:       true, // 出站 Intent CONFIRMED (扣减 CUSTODY_HOT_PAYOUT)
		PayoutFailedRetryable: true, // 出站 Intent FAILED_REVERTED 或 CANCELLED_BY_FILLER
	},
	PayoutFailedRetryable: {
		PayoutInFlight:          true, // 递增 retry_seq (退款换新票据) 分配新 Nonce 重发
		PayoutCancelledUnlocked: true, // 放弃重试, 解冻回退至原负债科目
	},
	PayoutCompleted: {
		PayoutInFlight: true, // 极端深分叉 LIFO 红冲后恢复在途重播
	},
}

// CanTransition 校验 from -> to 是否在 §5.4 白名单内.
func (s PayoutStatus) CanTransition(to PayoutStatus) bool {
	return canTransit(payoutTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s PayoutStatus) String() string { return string(s) }

// IsTerminal 是否为终态.
func (s PayoutStatus) IsTerminal() bool {
	return s == PayoutCancelledUnlocked
}

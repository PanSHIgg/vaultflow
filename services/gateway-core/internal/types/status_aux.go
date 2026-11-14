package types

// §6.3 其余 CHECK 枚举状态机. 文档未给出 mermaid 迁移图的状态域,
// 迁移白名单按 §4/§6 文字规则推导 (标注 AMBIGUITY 的条目已回报冻结方).

// ---------------------------------------------------------------------------
// sweep_batches.status (5 态, §6.3)
// ---------------------------------------------------------------------------

// SweepBatchStatus 定额归集批次状态.
type SweepBatchStatus string

const (
	// SweepBatchPending 已组装待提交 TxManager.
	SweepBatchPending SweepBatchStatus = "PENDING"
	// SweepBatchInFlight 已关联出站 Intent 在途.
	SweepBatchInFlight SweepBatchStatus = "IN_FLIGHT"
	// SweepBatchCompleted 全部 item 归集成功.
	SweepBatchCompleted SweepBatchStatus = "COMPLETED"
	// SweepBatchPartialQuarantined 部分 item 被隔离 (VaultQuarantined), 其余成功.
	SweepBatchPartialQuarantined SweepBatchStatus = "PARTIAL_QUARANTINED"
	// SweepBatchFailed 批次交易 Revert/取消, 未发生任何归集.
	SweepBatchFailed SweepBatchStatus = "FAILED"
)

var sweepBatchTransitions = map[SweepBatchStatus]map[SweepBatchStatus]bool{
	SweepBatchPending: {
		SweepBatchInFlight: true, // 分配 Nonce 广播
	},
	SweepBatchInFlight: {
		SweepBatchCompleted:          true, // 回执成功且无隔离项
		SweepBatchPartialQuarantined: true, // 回执成功但含 VaultQuarantined 项
		SweepBatchFailed:             true, // 回执 Revert / Filler 取消
	},
}

// CanTransition 校验 from -> to 是否在白名单内.
func (s SweepBatchStatus) CanTransition(to SweepBatchStatus) bool {
	return canTransit(sweepBatchTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s SweepBatchStatus) String() string { return string(s) }

// ---------------------------------------------------------------------------
// sweep_batch_items.item_status (4 态, §6.3 + 004_sweep_release.sql)
// ---------------------------------------------------------------------------

// SweepItemStatus 归集批次单条目状态.
type SweepItemStatus string

const (
	// SweepItemPending 待归集.
	SweepItemPending SweepItemStatus = "PENDING"
	// SweepItemSwept 已归集 (VaultSwept 事件, actual_swept_amount 落账).
	SweepItemSwept SweepItemStatus = "SWEPT"
	// SweepItemQuarantinedRevert 划转 Revert 被外层捕获 (VaultQuarantined, is_quarantined).
	SweepItemQuarantinedRevert SweepItemStatus = "QUARANTINED_REVERT"
	// SweepItemReleased 批次终态失败/派发超时后未结条目作废, 解除对 vault
	// 精确余额的在途占用 (004_sweep_release.sql; FIX-E/E4 Go 侧对齐, 终态).
	SweepItemReleased SweepItemStatus = "RELEASED"
)

var sweepItemTransitions = map[SweepItemStatus]map[SweepItemStatus]bool{
	SweepItemPending: {
		SweepItemSwept:             true,
		SweepItemQuarantinedRevert: true,
		SweepItemReleased:          true, // 批次 FAILED/过期 -> 未结条目释放 (FIX-B/B2)
	},
}

// CanTransition 校验 from -> to 是否在白名单内.
func (s SweepItemStatus) CanTransition(to SweepItemStatus) bool {
	return canTransit(sweepItemTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s SweepItemStatus) String() string { return string(s) }

// ---------------------------------------------------------------------------
// webhook_outbox.status (3 态, §6.3 + W4-4)
// ---------------------------------------------------------------------------

// WebhookStatus Transactional Outbox 投递状态.
type WebhookStatus string

const (
	// WebhookPending 待投递/指数退避重试中.
	WebhookPending WebhookStatus = "PENDING"
	// WebhookDelivered 已送达.
	WebhookDelivered WebhookStatus = "DELIVERED"
	// WebhookDeadLetter 重试超限转入死信.
	WebhookDeadLetter WebhookStatus = "DEAD_LETTER"
)

var webhookTransitions = map[WebhookStatus]map[WebhookStatus]bool{
	WebhookPending: {
		WebhookPending:    true, // 原地重试 (retry_count++/next_retry_at 推后)
		WebhookDelivered:  true,
		WebhookDeadLetter: true,
	},
}

// CanTransition 校验 from -> to 是否在白名单内.
func (s WebhookStatus) CanTransition(to WebhookStatus) bool {
	return canTransit(webhookTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s WebhookStatus) String() string { return string(s) }

// ---------------------------------------------------------------------------
// chain_blocks.status (2 态, §4.3/§6.3)
// ---------------------------------------------------------------------------

// ChainBlockStatus 区块在本地主链视图中的状态.
type ChainBlockStatus string

const (
	// ChainBlockCanonical 处于当前主链上 (部分唯一索引 chain_id+block_number 仅约束此态).
	ChainBlockCanonical ChainBlockStatus = "CANONICAL"
	// ChainBlockOrphaned 已被 LIFO 回滚判为孤块.
	ChainBlockOrphaned ChainBlockStatus = "ORPHANED"
)

var chainBlockTransitions = map[ChainBlockStatus]map[ChainBlockStatus]bool{
	ChainBlockCanonical: {
		ChainBlockOrphaned: true, // 分叉回滚置孤
	},
}

// CanTransition 校验 from -> to 是否在白名单内.
func (s ChainBlockStatus) CanTransition(to ChainBlockStatus) bool {
	return canTransit(chainBlockTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s ChainBlockStatus) String() string { return string(s) }

// ---------------------------------------------------------------------------
// merchant_withdrawal_addresses.status (4 态, §6.3 + Signer 租期白名单规则)
// ---------------------------------------------------------------------------

// WithdrawalAddressStatus 商户提现白名单地址状态.
type WithdrawalAddressStatus string

const (
	// WithdrawalPendingCooldown 已注册未满冷却期 (now < first_seen_at + 24h).
	WithdrawalPendingCooldown WithdrawalAddressStatus = "PENDING_COOLDOWN"
	// WithdrawalActive 冷却期满且租期有效, 可用于 PAYOUT_POLICY.
	WithdrawalActive WithdrawalAddressStatus = "ACTIVE"
	// WithdrawalExpired 超过 valid_until 租期上限.
	WithdrawalExpired WithdrawalAddressStatus = "EXPIRED"
	// WithdrawalRevoked 已被管理员紧急吊销.
	WithdrawalRevoked WithdrawalAddressStatus = "REVOKED"
)

var withdrawalAddrTransitions = map[WithdrawalAddressStatus]map[WithdrawalAddressStatus]bool{
	WithdrawalPendingCooldown: {
		WithdrawalActive:  true, // 冷却期满
		WithdrawalExpired: true, // 租期先于冷却期结束 (边界情形)
		WithdrawalRevoked: true, // 管理员吊销
	},
	WithdrawalActive: {
		WithdrawalExpired: true, // now >= valid_until
		WithdrawalRevoked: true, // 管理员吊销
	},
}

// CanTransition 校验 from -> to 是否在白名单内.
func (s WithdrawalAddressStatus) CanTransition(to WithdrawalAddressStatus) bool {
	return canTransit(withdrawalAddrTransitions, s, to)
}

// String 返回 DB 存储字符串.
func (s WithdrawalAddressStatus) String() string { return string(s) }

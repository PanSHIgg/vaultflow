package types

// 账本 reference_id 前缀契约 (FIX-E/E1).
//
// ledger_transactions 以 (reference_type, reference_id) 为幂等键:
// reference_type = EntryTemplateCode, reference_id = <前缀>+<业务主键>.
// 同一条业务记账只允许一个规范前缀 — 写入方 (链上 finalize 路径与人工
// 审核路径) 与反查方 (LIFO 深回滚 unwind / 存活腿重建) 必须共享同一常量;
// 前缀漂移会使深回滚误判"已结单无结算腿"而永久阻塞回滚, 且静默掩盖
// 双套前缀并存产生的重复结算面.
const (
	// RefPrefixOrderSettle 订单结算 (TemplateOrderSettle) 的规范前缀.
	//   写入: indexer/finalize.go settleOrder (链上) + order/review.go
	//   settleSuspendedToPaid (人工 ACCEPT_UNDERPAID / ACCEPT_LATE_PAID);
	//   反查: indexer/rollback.go unwindSettledOrder.
	RefPrefixOrderSettle = "ordsettle:"
)

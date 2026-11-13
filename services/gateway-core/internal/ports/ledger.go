package ports

import "context"

// LedgerService NUMERIC(78,0) 全值域整数复式记账引擎 (W3 实现).
// 消费方: W3 order (结算/转挂), W4 indexer (入站四分支/出站两段式/LIFO 红冲),
// W5 api (14 科目对账大盘), I1 装配.
type LedgerService interface {
	// PostEntry 按 §6.2 封闭模板记账. 幂等: (Template, ReferenceID) 已存在且
	// 分录一致时直接返回已存事务 (AlreadyExisted=true); 不一致返回
	// ErrDuplicateReference. 实现端在同一 PG 事务内完成建户、加锁、校验、
	// 写分录与 outbox 事件.
	PostEntry(ctx context.Context, req PostEntryRequest) (PostedEntry, error)

	// GetBalances 查询账户余额 (按 owner+chain+asset 维度).
	GetBalances(ctx context.Context, q BalanceQuery) ([]AccountBalance, error)

	// RunInvariantCheck 双层账务巡检: 账本内试算平衡 + (可选) safe_head 链上
	// 链下四项核对 (§8.3). 快照数据由调用方 (W4 chain 查询器) 供给.
	RunInvariantCheck(ctx context.Context, req InvariantCheckRequest) (InvariantReport, error)
}

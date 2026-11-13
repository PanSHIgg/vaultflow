package ports

import "errors"

// 跨窗口共享哨兵错误. 实现端应以 errors.Is 可判的方式包装底层错误,
// W3/W4/W5 据此统一映射到 HTTP 状态码 / 告警级别.
var (
	// ErrNotFound 目标实体不存在.
	ErrNotFound = errors.New("ports: resource not found")
	// ErrIdempotencyConflict 同 Idempotency-Key 但 request_hash 不同 (HTTP 409).
	ErrIdempotencyConflict = errors.New("ports: idempotency key reused with different request hash")
	// ErrDuplicateReference 账本 (reference_type, reference_id) 幂等键已存在
	// 且本次分录内容与已存不一致; 一致则按幂等成功返回.
	ErrDuplicateReference = errors.New("ports: duplicate ledger reference with mismatched payload")
	// ErrInvalidTransition 状态迁移不在 §5 白名单内.
	ErrInvalidTransition = errors.New("ports: state transition not allowed")
	// ErrTerminalState 实体已处终态, 拒绝再变更.
	ErrTerminalState = errors.New("ports: entity already in terminal state")
	// ErrInvalidTemplateLeg 分录腿不在 §6.2 模板借贷白名单内.
	ErrInvalidTemplateLeg = errors.New("ports: entry leg not allowed by template")
	// ErrUnbalancedEntry 单笔分录 SUM(DEBIT) != SUM(CREDIT).
	ErrUnbalancedEntry = errors.New("ports: entry debits do not equal credits")
	// ErrNonPositiveAmount 分录金额 <= 0 (CHECK amount > 0).
	ErrNonPositiveAmount = errors.New("ports: entry amount must be positive")
	// ErrNegativeBalance 分录将导致账户余额为负 (CHECK balance >= 0).
	ErrNegativeBalance = errors.New("ports: entry would drive account balance negative")
	// ErrInvalidAccountCode 非法的 14 科目之外的科目码.
	ErrInvalidAccountCode = errors.New("ports: invalid ledger account code")
	// ErrMerchantFrozen 商户 payout_frozen = true (MERCHANT_REORG_RECEIVABLE 未清零).
	ErrMerchantFrozen = errors.New("ports: merchant payout is frozen")
	// ErrSignerRejected vault-signer 防火墙拒签 (明细见 SignTxResponse.Rejected).
	ErrSignerRejected = errors.New("ports: vault-signer rejected the request")
	// ErrSignerUnavailable vault-signer mTLS 通道不可用.
	ErrSignerUnavailable = errors.New("ports: vault-signer unavailable")
	// ErrNonceHalted 该热钱包出站队列已触发 NONCE_STOLEN_HALT 熔断.
	ErrNonceHalted = errors.New("ports: wallet outbound queue halted by NONCE_STOLEN_HALT")
	// ErrUnavailable 外部依赖 (PG/Redis/RPC) 暂不可用.
	ErrUnavailable = errors.New("ports: dependency unavailable")
)

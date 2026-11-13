package ports

import (
	"context"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// OrderService 收单、审核与提现退款编排 (W3 order 实现).
// 消费方: W5 api (下单/查询/审核), W4 indexer (状态联动可经内部直接调用或
// 同事务编排, 本接口供跨模块查询与指令通道).
type OrderService interface {
	// CreateOrder 幂等下单: 同事务创建 1:1 deposit_addresses + payment_orders
	// (AWAITING_PAYMENT). 同 (MerchantID, IdempotencyKey) 同 RequestHash 返回
	// 首个订单; 异 RequestHash 返回 ErrIdempotencyConflict (HTTP 409).
	CreateOrder(ctx context.Context, req CreateOrderRequest) (types.PaymentOrder, error)

	// GetOrder 查询订单 (收银台/监控台).
	GetOrder(ctx context.Context, orderID uuid.UUID) (types.PaymentOrder, error)

	// ReviewOrder 人工审核编排: 少付折让/迟付履约接受 -> PAID; 创建/取消
	// CUSTOMER_REFUND; 深回滚追偿核销 -> REVERSAL_RESOLVED. 迁移不在
	// §5.2 白名单内返回 ErrInvalidTransition.
	ReviewOrder(ctx context.Context, req ReviewOrderRequest) (types.PaymentOrder, error)

	// RequestPayout 发起商户提现或客户退款 (幂等). 锁定来源负债科目入
	// PAYOUT_LOCKED_LIAB (PAYOUT_FLOW 冻结腿) 并落 LOCKED_QUEUED 单.
	// 商户 payout_frozen 时返回 ErrMerchantFrozen.
	RequestPayout(ctx context.Context, req PayoutRequest) (types.MerchantPayout, error)

	// CancelPayout 取消未广播或 FAILED_RETRYABLE 的提现/退款, 资金原路解冻
	// 回来源负债科目 -> CANCELLED_UNLOCKED.
	CancelPayout(ctx context.Context, payoutID uuid.UUID) (types.MerchantPayout, error)

	// GetPayout 查询提现/退款单.
	GetPayout(ctx context.Context, payoutID uuid.UUID) (types.MerchantPayout, error)

	// UnfreezeMerchant 管理员复核解除 merchants.payout_frozen
	// (前置: MERCHANT_REORG_RECEIVABLE 已清零).
	UnfreezeMerchant(ctx context.Context, merchantID uuid.UUID) error
}

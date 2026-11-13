package ports

import (
	"context"

	"github.com/google/uuid"
)

// SSEBroker 收银台订单事件实时广播 (W5 sse 实现: Redis Pub/Sub 多实例分发,
// Redis 不可用时无缝降级进程内通知 + 客户端短轮询). 生产方: W3 order /
// W4 indexer (结算与状态跃迁事件); 消费方: W5 api SSE handler.
type SSEBroker interface {
	// PublishOrderEvent 发布订单事件. Redis 降级期间仅投递进程内订阅者.
	PublishOrderEvent(ctx context.Context, ev OrderEvent) error

	// SubscribeOrder 订阅单订单事件流. 返回只读通道与取消函数;
	// 调用方必须在退出时执行 cancel 释放资源 (Redis 订阅或进程内注册项).
	SubscribeOrder(ctx context.Context, orderID uuid.UUID) (events <-chan OrderEvent, cancel func(), err error)
}

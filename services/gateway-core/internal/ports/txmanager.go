package ports

import (
	"context"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// TxManager 交易生命周期引擎 (W4 txmanager 实现): PG 持久化 Nonce 分配、
// EIP-1559 组装、Signer 策略签名、广播、RBF/Dropped/带外占用检测与
// GAP_FILLER 在途取消. 消费方: W4 sweeper/indexer, W3 order (经 payout 编排),
// W5 监控台.
type TxManager interface {
	// QueueIntent 同事务分配连续 Nonce 落 QUEUED 意图并触发首播流水线.
	// 该钱包队列已 NONCE_STOLEN_HALT 熔断时返回 ErrNonceHalted.
	QueueIntent(ctx context.Context, req QueueIntentRequest) (types.OutboundTxIntent, error)

	// GetIntent 查询意图当前生命周期状态.
	GetIntent(ctx context.Context, intentID uuid.UUID) (types.OutboundTxIntent, error)

	// ListAttempts 列出意图下同 Nonce 全部候选 (含 BROADCASTED / SUPERSEDED /
	// DROPPED / INCLUDED_UNSAFE / FINALIZED), 供监控台回放 RBF 历史.
	ListAttempts(ctx context.Context, intentID uuid.UUID) ([]types.OutboundTxAttempt, error)

	// RequestCancel 对在途意图追加同 Nonce GAP_FILLER 取消候选
	// (命中 Signer 双字段幂等槽, 不覆盖原业务授权字段). 仅 IN_FLIGHT 可取消,
	// 其余状态返回 ErrInvalidTransition.
	RequestCancel(ctx context.Context, intentID uuid.UUID) error

	// NonceCursorStatus 查询热钱包 Nonce 游标与熔断状态 (监控台观测).
	NonceCursorStatus(ctx context.Context, chainID types.ChainID, wallet types.Address) (NonceCursorStatus, error)
}

package ports

import (
	"context"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// TxManager 交易生命周期引擎 (txmanager 实现): PG 持久化 Nonce 分配、
// EIP-1559 组装、Signer 策略签名、广播、RBF/Dropped/带外占用检测与
// GAP_FILLER 在途取消. 消费方: sweeper (SWEEP_BATCH 入队), cmd/backend
// 装配面.
type TxManager interface {
	// QueueIntent 同事务分配连续 Nonce 落 QUEUED 意图并触发首播流水线.
	// 该钱包队列已 NONCE_STOLEN_HALT 熔断时返回 ErrNonceHalted.
	QueueIntent(ctx context.Context, req QueueIntentRequest) (types.OutboundTxIntent, error)

	// GetIntent 查询意图当前生命周期状态.
	GetIntent(ctx context.Context, intentID uuid.UUID) (types.OutboundTxIntent, error)

	// ListAttempts 列出意图下同 Nonce 全部候选 (含 BROADCASTED / SUPERSEDED /
	// DROPPED / INCLUDED_UNSAFE / FINALIZED), 供监控台回放 RBF 历史.
	ListAttempts(ctx context.Context, intentID uuid.UUID) ([]types.OutboundTxAttempt, error)

	// RequestCancel 对意图请求同 Nonce GAP_FILLER 取消
	// (命中 Signer 双字段幂等槽, 不覆盖原业务授权字段).
	//   - IN_FLIGHT: 立即追加同 Nonce GAP_FILLER 候选;
	//   - QUEUED (FIX-E/E2): 首播前打持久标记, 下一轮派发以同 Nonce
	//     GAP_FILLER 填洞;
	//   - 其余状态返回 ErrInvalidTransition.
	RequestCancel(ctx context.Context, intentID uuid.UUID) error

	// NonceCursorStatus 查询热钱包 Nonce 游标与熔断状态 (监控台观测).
	NonceCursorStatus(ctx context.Context, chainID types.ChainID, wallet types.Address) (NonceCursorStatus, error)
}

// ===========================================================================
// TxManager DTO
// ===========================================================================

// QueueIntentRequest 出站意图入队.
// 实现端 (txmanager) 在 PG 事务内对 wallet_nonce_cursors SELECT FOR UPDATE
// 分配连续 Nonce 并落 QUEUED 意图; 随后组装 EIP-1559 未签名交易请求
// vault-signer 策略签名并广播. Calldata 由调用方 ABI 编码
// (sweeper 编 deployAndSweepBatch).
// 交易所托管模型下 intent_type 仅 SWEEP_BATCH / NONCE_GAP_FILLER.
type QueueIntentRequest struct {
	IntentType           types.IntentType
	ReferenceID          uuid.UUID // sweep_batch_id (SWEEP_BATCH)
	ChainID              types.ChainID
	WalletAddress        types.Address // 出站热钱包 (Nonce 归属)
	To                   types.Address
	Value                types.U256 // 合约调用恒为 0
	Calldata             []byte
	GasLimit             uint64
	MaxFeePerGas         types.U256
	MaxPriorityFeePerGas types.U256
	// RetrySeq/RefundTicket 为 signer 幂等槽保留字段 (交易所模型当前未用).
	RetrySeq     uint32
	RefundTicket *RefundTicket
}

// NonceCursorStatus 热钱包 Nonce 游标与熔断状态.
type NonceCursorStatus struct {
	ChainID         types.ChainID
	WalletAddress   types.Address
	NextNonce       uint64
	InFlightIntents int  // 在途意图数
	Halted          bool // 存在 NONCE_STOLEN_HALT 意图 = true (暂停调度)
}

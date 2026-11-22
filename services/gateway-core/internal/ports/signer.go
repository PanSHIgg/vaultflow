package ports

// ports.SignerClient 与其 DTO — 交易所托管模型版.
//
// DTO 与 proto/signer/v1/signer.proto 字段级对齐, 但本接口保持 proto-agnostic:
// 实现端 (signerclient) 负责原生结构 <-> protobuf 消息转换.
// 消费方: txmanager (SignTx + VerifyAuditChain 巡检), cmd/backend 与
// cmd/gateway 装配面 (GenerateWallet / UpdateColdWallets 冷热钱包管理).

import (
	"context"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// SignerClient vault-signer 双向 mTLS gRPC 客户端 (signerclient 实现).
type SignerClient interface {
	// SignTx 请求签名一笔出站 EIP-1559 交易. signer 独立解码 unsigned_tx 推导
	// 策略并执行四模防火墙校验; 返回二选一结果 — Signed 或 Rejected.
	// 传输层错误 (连接断开/mTLS 失败) 返回 ErrSignerUnavailable.
	SignTx(ctx context.Context, req SignTxRequest) (SignTxResponse, error)

	// RegisterWhitelist 注册商户提现白名单地址 (附 69B 租期载荷 + Ed25519 签名).
	// proto 契约保留 — 当前业务面未消费, 仍属签名机能力面.
	RegisterWhitelist(ctx context.Context, req RegisterWhitelistRequest) (WhitelistOpResponse, error)

	// RevokeWhitelist 紧急吊销提现白名单地址 (管理员可经 mTLS 直连 signer,
	// 本接口为 gateway 转发通道; 全程落审计链).
	RevokeWhitelist(ctx context.Context, req RevokeWhitelistRequest) (WhitelistOpResponse, error)

	// GenerateWallet 由 signer KMS 生成新平台钱包密钥对 (私钥落
	// encrypted_keys, API 仅返回地址). role ∈ {RELAYER, PAYOUT, ...} —
	// 平台钱包角色标识透传给 signer 策略面.
	GenerateWallet(ctx context.Context, chainID types.ChainID, role string) (types.Address, error)

	// UpdateColdWallets 增量更新 signer 侧 allowed_cold_wallets 动态白名单
	// 并持久化 (sweep 策略 ColdWalletNotAllowed 校验的事实源).
	// 返回更新后的全量 ACTIVE 冷钱包地址集.
	// Accepted=false 时返回的错误须携带 RejectCode+Reason (见
	// signerclient.ColdWalletsRejectedError).
	UpdateColdWallets(ctx context.Context, chainID types.ChainID, add, remove []types.Address) ([]types.Address, error)

	// VerifyAuditChain 从指定区间重算 SHA-256 审计哈希链
	// (signerclient 巡检协程每 5 分钟调用, 校验失败触发最高优先级安全告警).
	VerifyAuditChain(ctx context.Context, req VerifyAuditChainRequest) (VerifyAuditChainResponse, error)

	// Health 健康度与运行态巡检 (版本/时钟/白名单规模/审计链 tip/已加载热钱包).
	Health(ctx context.Context) (SignerHealth, error)
}

// ===========================================================================
// SignerClient DTO — 与 proto/signer/v1 字段级对齐的原生 Go 结构
// ===========================================================================

// RejectCode 拒签原因码 (镜像 signer.v1.RejectCode 数值).
type RejectCode int32

const (
	RejectCodeUnspecified           RejectCode = 0
	RejectCodeMalformedTx           RejectCode = 1  // EIP-2718 Type-2 RLP 解码失败
	RejectCodeUnknownPolicy         RejectCode = 2  // 不匹配四类策略
	RejectCodePolicyViolation       RejectCode = 3  // 命中策略但字段校验失败
	RejectCodeColdWalletNotAllowed  RejectCode = 4  // 目标冷金库不在只读白名单
	RejectCodeAuthNonceMismatch     RejectCode = 5  // authNonce != keccak256(chain_id, salt)
	RejectCodeWhitelistNotFound     RejectCode = 6  // 提现收款人未注册
	RejectCodeWhitelistCooldown     RejectCode = 7  // 未满 first_seen_at + cooldown
	RejectCodeWhitelistExpired      RejectCode = 8  // now >= valid_until
	RejectCodeWhitelistRevoked      RejectCode = 9  // 已吊销
	RejectCodeInvalidAdminSignature RejectCode = 10 // Ed25519 载荷验签失败
	RejectCodeInvalidRefundTicket   RejectCode = 11 // RefundTicket 不匹配/过期/验签失败
	RejectCodeNonceSlotConflict     RejectCode = 12 // 同 Nonce 槽位业务字段被篡改
	RejectCodeDailyLimitExceeded    RejectCode = 13 // 触发 24h 滑动限额熔断器
	RejectCodeInternal              RejectCode = 14
)

// TxPolicy 四类交易策略 (镜像 signer.v1.TxPolicy 数值; 仅作 hint,
// signer 始终以自行解码 unsigned_tx 的结论为准).
type TxPolicy int32

const (
	TxPolicyUnspecified  TxPolicy = 0
	TxPolicySweep        TxPolicy = 1 // relayer -> BatchSweeper.deployAndSweepBatch
	TxPolicyEIP3009Relay TxPolicy = 2 // relayer -> EIP3009PayRouter.payOrderWithAuthorization
	TxPolicyPayout       TxPolicy = 3 // payout wallet -> 白名单稳定币 transfer
	TxPolicyGapFiller    TxPolicy = 4 // 热钱包 0 ETH 自转账
)

// RefundTicket 客户异常退款票据 (镜像 signer.v1.RefundTicket).
type RefundTicket struct {
	SignedPayload []byte // 127B: "VAULTFLOW_REFUND_V1"(19) || chain_id(8) || token(20)
	// || to(20) || exact_amount_u256(32B BE) || payout_uuid(16) || retry_seq(4) || expiry(8)
	AdminSignature []byte // 64B Ed25519
}

// SignTxRequest 签名请求 (镜像 signer.v1.SignTransactionRequest).
type SignTxRequest struct {
	ChainID      uint64
	UnsignedTx   []byte // EIP-2718 Type-2 未签名编码: 0x02 || rlp([...])
	PolicyHint   TxPolicy
	PayoutUUID   uuid.UUID // PAYOUT 策略幂等槽绑定 payout_id (其余策略置零)
	RetrySeq     uint32
	RefundTicket *RefundTicket
}

// SignedTx 签名成功结果 (镜像 signer.v1.SignedTx).
type SignedTx struct {
	RawTx    []byte // 可直接广播的已签名 EIP-2718 Type-2 字节
	TxHash   types.Hash
	AuditSeq uint64
}

// Rejection 拒签详情 (镜像 signer.v1.Rejection).
type Rejection struct {
	Code     RejectCode
	Reason   string
	AuditSeq uint64
}

// SignTxResponse 二选一结果 (镜像 proto oneof result).
type SignTxResponse struct {
	Signed   *SignedTx
	Rejected *Rejection
}

// RegisterWhitelistRequest 注册提现白名单 (镜像 RegisterWithdrawalAddressRequest).
type RegisterWhitelistRequest struct {
	SignedPayload []byte // 69B: "VAULTFLOW_ADDR_V1"(17) || chain_id(8) || merchant_uuid(16)
	// || recipient_address(20) || valid_until_unix(8)
	AdminSignature []byte // 64B Ed25519
}

// RevokeWhitelistRequest 紧急吊销白名单 (镜像 RevokeWithdrawalAddressRequest).
type RevokeWhitelistRequest struct {
	ChainID          uint64
	MerchantUUID     uuid.UUID
	RecipientAddress types.Address
	AdminNote        string
}

// WhitelistOpResponse 注册/吊销响应 (镜像 *WithdrawalAddressResponse 公共字段).
type WhitelistOpResponse struct {
	Accepted   bool
	RejectCode RejectCode
	Reason     string
	AuditSeq   uint64
}

// VerifyAuditChainRequest 审计链校验请求 (镜像 VerifyAuditChainRequest).
type VerifyAuditChainRequest struct {
	FromSeq uint64 // 0 = 从创世记录开始
	ToSeq   uint64 // 0 = 校验到最新
}

// VerifyAuditChainResponse 审计链校验结果 (镜像 VerifyAuditChainResponse).
type VerifyAuditChainResponse struct {
	Valid          bool
	EntriesChecked uint64
	FirstBadSeq    uint64 // Valid=false 时第一条哈希不连续记录
	TipHash        types.Hash
}

// SignerHealth 健康度与运行态 (镜像 GetSignerHealthResponse).
type SignerHealth struct {
	Version          string
	UptimeSeconds    uint64
	SignerClock      time.Time // signer 本地时钟 (白名单冷却判定基准)
	WhitelistEntries uint32
	AuditEntries     uint64
	AuditTipHash     types.Hash
	LoadedWallets    []types.Address
}

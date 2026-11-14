package ports

// W3/W4/W5 共享的请求/响应 DTO. 金额一律 types.U256 (JSON 十进制字符串),
// 地址/哈希一律 types.Address / types.Hash, ID 一律 uuid.UUID.
// SignerClient 相关 DTO 与 proto/signer/v1/signer.proto 字段级对齐,
// 但保持 proto-agnostic — ports 包禁止 import internal/genproto.

import (
	"math/big"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// ===========================================================================
// LedgerService DTO
// ===========================================================================

// PostEntryRequest 模板化复式记账请求.
// 调用方给出具体借贷腿; 实现端 (W3 ledger) 负责:
//  1. 逐腿校验 (debit -> credit) 命中 types.EntryTemplateCode.Allows 白名单;
//  2. SUM(DEBIT) == SUM(CREDIT) 且每腿 amount > 0;
//  3. 按需幂等建户, 按 account_id 升序 SELECT ... FOR UPDATE 防死锁;
//  4. 变动后 balance >= 0; 同事务写 ledger_transactions + ledger_entries
//     (带 balance_after 快照) + webhook_outbox.
type PostEntryRequest struct {
	Template    types.EntryTemplateCode
	ReferenceID string // 业务幂等键: UNIQUE(reference_type, reference_id)
	Description string
	Legs        []PostEntryLeg
}

// PostEntryLeg 单条记账腿.
type PostEntryLeg struct {
	OwnerMerchantID uuid.UUID // 系统科目传 types.SystemMerchantID
	ChainID         types.ChainID
	AssetSymbol     string
	Account         types.LedgerAccountCode
	Direction       types.Direction
	Amount          types.U256 // 必须 > 0
}

// PostedEntry 记账结果. AlreadyExisted = true 表示幂等命中
// (reference_type, reference_id) 已存在且分录一致, 未重复入账.
type PostedEntry struct {
	Transaction    types.LedgerTransaction
	Entries        []types.LedgerEntry
	AlreadyExisted bool
}

// BalanceQuery 账户余额查询 (按 owner+chain+asset 聚合维度).
type BalanceQuery struct {
	OwnerMerchantID uuid.UUID // 系统科目传 types.SystemMerchantID
	ChainID         types.ChainID
	AssetSymbol     string
	Accounts        []types.LedgerAccountCode // 空 = 该维度全部科目
}

// AccountBalance 单科目余额.
type AccountBalance struct {
	Account types.LedgerAccountCode
	Balance types.U256
}

// InvariantCheckRequest 双层账务巡检请求 (W3-2 InvariantChecker).
// Snapshot 为 nil 时仅执行账本内试算平衡 (第一层);
// 非 nil 时追加链上链下 safe_head 快照四项核对 (第二层, 快照由 W4 采集).
type InvariantCheckRequest struct {
	ChainID        types.ChainID
	AssetSymbol    string
	SafeHeadHeight uint64
	Snapshot       *ChainSnapshot
}

// ChainSnapshot safe_head_height 处链上托管资产快照 (由 W4 链查询器供给).
type ChainSnapshot struct {
	UnsweptVaultTotal     types.U256 // 各 DepositVault 已确认未归集额之和, 应 == CUSTODY_UNSWEPT
	ColdWalletBalance     types.U256 // 冷金库余额, 应 == CUSTODY_COLD
	HotPayoutTokenBalance types.U256 // payout 热钱包稳定币余额, 应 == CUSTODY_HOT_PAYOUT
	HotWalletNativeTotal  types.U256 // 双热钱包原生 ETH 之和, 应 == HOT_WALLET_NATIVE
}

// AccountDiff 链上链下单科目核对差异 (Delta 可为负, 故用 *big.Int).
type AccountDiff struct {
	Account types.LedgerAccountCode
	Ledger  types.U256
	OnChain types.U256
	// Delta = OnChain - Ledger; 精确到 1 Wei, 零差异时 Sign()==0.
	Delta *big.Int
}

// InvariantReport 巡检报告. 全部通过时 Violations 为空.
type InvariantReport struct {
	ChainID          types.ChainID
	AssetSymbol      string
	SafeHeadHeight   uint64
	TrialBalanced    bool // 第一层: SUM(DEBIT) == SUM(CREDIT)
	TotalDebits      types.U256
	TotalCredits     types.U256
	NegativeAccounts []types.LedgerAccountCode // balance < 0 账户, 应为空
	ChainDiffs       []AccountDiff             // 第二层四项核对, Snapshot==nil 时为空
	Violations       []string                  // 人类可读违例明细
	CheckedAt        time.Time
}

// ===========================================================================
// OrderService DTO
// ===========================================================================

// CreateOrderRequest 创建支付订单 (幂等).
// W5 中间件已校验 (merchant_id, idempotency_key) UNIQUE 与 request_hash 一致性;
// 实现端同事务创建 1:1 deposit_addresses + payment_orders(AWAITING_PAYMENT),
// salt = keccak256(abi.encode(chainId, merchantId, orderId)).
type CreateOrderRequest struct {
	MerchantID     uuid.UUID
	IdempotencyKey string
	RequestHash    string // 请求体 SHA-256 hex
	ChainID        types.ChainID
	AssetSymbol    string
	ExpectedAmount types.U256
	ExpiresAt      time.Time
}

// ReviewAction 人工审核动作 (§5.2 审核编排).
type ReviewAction string

const (
	// ReviewAcceptUnderpaid 少付折让接受实收额: UNDERPAID_EXPIRED_REVIEW -> PAID
	// (模板 4 借 SUSPENSE_REFUNDABLE_LIAB 结转).
	ReviewAcceptUnderpaid ReviewAction = "ACCEPT_UNDERPAID"
	// ReviewAcceptLatePaid 接受迟到款履约: LATE_PAID_REVIEW -> PAID.
	ReviewAcceptLatePaid ReviewAction = "ACCEPT_LATE_PAID"
	// ReviewCreateRefund 创建关联 CUSTOMER_REFUND 退款单:
	// UNDERPAID_EXPIRED_REVIEW / LATE_PAID_REVIEW -> REFUND_IN_PROGRESS.
	ReviewCreateRefund ReviewAction = "CREATE_REFUND"
	// ReviewCancelRefund 取消未成行退款单退回人工决策:
	// REFUND_IN_PROGRESS -> LATE_PAID_REVIEW.
	ReviewCancelRefund ReviewAction = "CANCEL_REFUND"
	// ReviewResolveReversal 客户补款/运营核销清零追偿应收款:
	// SETTLED_REVERSED -> REVERSAL_RESOLVED.
	ReviewResolveReversal ReviewAction = "RESOLVE_REVERSAL"
)

// ReviewOrderRequest 订单人工审核请求.
type ReviewOrderRequest struct {
	OrderID uuid.UUID
	Action  ReviewAction
	// RefundToAddress CREATE_REFUND 必填: 核实后的客户收款地址
	// (禁止自动退回 tx.from_address, §9.2).
	RefundToAddress *types.Address
	// RefundTicket CREATE_REFUND 必填: 离线管理员签署的 127B 退款票据
	// (与 PayoutRequest.RefundTicket 同构). 票据内嵌 payout_uuid(raw16,
	// 载荷 [99:115]) 即所建 merchant_payouts.id — signer 按
	// SignTransactionRequest.payout_uuid 逐字节比对, 故 PK 必须等于
	// 票据内嵌 uuid (FIX-D 修复: 此前恒 nil 致 CUSTOMER_REFUND 必拒签).
	RefundTicket *RefundTicket
	OperatorNote string
}

// PayoutRequest 发起提现/客户退款 (幂等; 走 §5.4 状态机, 申请冻结即记 PAYOUT_FLOW 第一腿).
type PayoutRequest struct {
	MerchantID     uuid.UUID
	IdempotencyKey string
	RequestHash    string
	Kind           types.PayoutKind
	SourceAccount  types.LedgerAccountCode // 须满足 types.IsPayoutSource()
	SourceOrderID  *uuid.UUID              // CUSTOMER_REFUND 关联订单
	ChainID        types.ChainID
	AssetSymbol    string
	ToAddress      types.Address
	Amount         types.U256
	// PayoutID 客户端幂等锚 (openapi payout_uuid), 入库即 merchant_payouts.id —
	// 亦即广播时 SignTransactionRequest.payout_uuid 与 outbound_tx_intents.
	// reference_id. CUSTOMER_REFUND 必须等于 RefundTicket 载荷内嵌 payout_uuid
	// (不一致即拒); 零值由服务端生成 (审核建单路径自动取票据内嵌值).
	PayoutID uuid.UUID
	// RefundTicket CUSTOMER_REFUND 必带 (WITHDRAWAL 置 nil):
	// 创建时暂存, 广播签名时透传给 vault-signer 自证. 新建单内嵌
	// retry_seq 必须为 0 (与 retry_seq 列初值对齐).
	RefundTicket *RefundTicket
}

// ===========================================================================
// TxManager DTO
// ===========================================================================

// QueueIntentRequest 出站意图入队.
// 实现端 (W4 txmanager) 在 PG 事务内对 wallet_nonce_cursors SELECT FOR UPDATE
// 分配连续 Nonce 并落 QUEUED 意图; 随后组装 EIP-1559 未签名交易请求
// vault-signer 策略签名并广播. Calldata 由调用方 ABI 编码
// (sweeper 编 deployAndSweepBatch / order 编 transfer / relay 编 payOrderWithAuthorization).
type QueueIntentRequest struct {
	IntentType  types.IntentType
	ReferenceID uuid.UUID // sweep_batch_id / merchant_payout_id / order_id;
	// IntentType==MERCHANT_PAYOUT 时即 Signer 幂等槽绑定的 payout_uuid.
	ChainID              types.ChainID
	WalletAddress        types.Address // 出站热钱包 (Nonce 归属)
	To                   types.Address
	Value                types.U256 // 合约调用恒为 0
	Calldata             []byte
	GasLimit             uint64
	MaxFeePerGas         types.U256
	MaxPriorityFeePerGas types.U256
	// RetrySeq PAYOUT 幂等槽绑定重试序号; RefundTicket 客户退款票据 (可为 nil).
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
	PayoutUUID   uuid.UUID // PAYOUT 策略必填: 幂等槽绑定 payout_id (其余策略置零)
	RetrySeq     uint32
	RefundTicket *RefundTicket // 客户异常退款携带, 商户常规提现置 nil
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

// ===========================================================================
// SSEBroker DTO
// ===========================================================================

// OrderEventType SSE 事件类型 (对应 openapi.yaml 事件流).
type OrderEventType string

const (
	// OrderEventDepositDetected 区块捕获到入站 Transfer.
	OrderEventDepositDetected OrderEventType = "DEPOSIT_DETECTED"
	// OrderEventConfirmationUpdate 确认数递增.
	OrderEventConfirmationUpdate OrderEventType = "CONFIRMATION_UPDATE"
	// OrderEventStatusChanged 订单状态跃迁 (结算完成/挂起/退款等).
	OrderEventStatusChanged OrderEventType = "STATUS_CHANGED"
	// OrderEventExpired 订单超时.
	OrderEventExpired OrderEventType = "EXPIRED"
)

// OrderEvent 收银台实时进度事件.
type OrderEvent struct {
	OrderID               uuid.UUID
	Type                  OrderEventType
	Status                types.OrderStatus
	ReceivedAmount        types.U256
	Confirmations         uint64 // 当前已确认区块数
	RequiredConfirmations uint64 // 安全确认阈值
	OccurredAt            time.Time
}

// ===========================================================================
// SimulatorService DTO
// ===========================================================================

// SimScenario 四大对抗演练场景 (ARCHITECTURE.md §8; 场景 1 拆为浅/深两触发器).
type SimScenario string

const (
	// SimScenarioReorgShallow §8.1 浅分叉回滚重挖.
	SimScenarioReorgShallow SimScenario = "REORG_SHALLOW"
	// SimScenarioReorgDeep §8.1 跨归集与提现后的极端深分叉 LIFO 回滚.
	SimScenarioReorgDeep SimScenario = "REORG_DEEP"
	// SimScenarioQuarantineRevertGas §8.2 黑名单/ReturnBomb 归集隔离 + Revert Gas 1-Wei 账平.
	SimScenarioQuarantineRevertGas SimScenario = "QUARANTINE_REVERT_GAS"
	// SimScenarioInvariantCheck §8.3 双层账务实时对账巡检.
	SimScenarioInvariantCheck SimScenario = "INVARIANT_CHECK"
	// SimScenarioSignerPenetration §8.4 被攻破网关四类越权盲签 + 双字段幂等槽.
	SimScenarioSignerPenetration SimScenario = "SIGNER_PENETRATION"
)

// SimRunStatus 演练运行状态.
type SimRunStatus string

const (
	SimRunRunning   SimRunStatus = "RUNNING"
	SimRunSucceeded SimRunStatus = "SUCCEEDED"
	SimRunFailed    SimRunStatus = "FAILED"
)

// SimTriggerRequest 演练触发请求.
type SimTriggerRequest struct {
	Scenario SimScenario
	ChainID  types.ChainID
	// Params 场景参数 (如回滚深度 depth / 目标批次 / 商户号), 键值契约由 W5 sim 实现定义.
	Params map[string]string
}

// SimRun 演练运行视图 (Detail 为 JSON 摘要: 分叉树/红冲分录流水/审计拦截明细).
type SimRun struct {
	ID         uuid.UUID
	Scenario   SimScenario
	Status     SimRunStatus
	Detail     string
	StartedAt  time.Time
	FinishedAt *time.Time
}

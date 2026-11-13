package types

import (
	"time"

	"github.com/google/uuid"
)

// §6.3 十八张 PostgreSQL 表的共享行投影. 字段类型统一为:
//   - uuid.UUID        (uuid 列)
//   - ChainID          (bigint chain_id)
//   - Address / Hash   (varchar 链上地址与哈希, 规范化后赋值)
//   - U256             (NUMERIC(78,0) 金额/Gas, JSON 十进制字符串)
//   - time.Time        (timestamptz)
//   - *T 指针          (可空列)
// 结构体为契约层投影; 各窗口可按需附加 db/json tag 或自有扫描行类型.

// ChainSyncCursor 单链单写者同步游标 (chain_sync_cursors).
type ChainSyncCursor struct {
	ChainID            ChainID
	LatestSyncedHeight uint64
	LatestSyncedHash   Hash
	SafeHeadHeight     uint64
	UpdatedAt          time.Time
}

// SupportedAsset 白名单资产 (supported_assets, PK (chain_id, asset_symbol)).
type SupportedAsset struct {
	ChainID         ChainID
	AssetSymbol     string // USDT / USDC / ETH
	TokenContract   Address
	Decimals        int32 // ETH=18, USDT/USDC=6
	SupportsEIP3009 bool
}

// Merchant 商户 (merchants).
type Merchant struct {
	ID               uuid.UUID
	Name             string
	APIKeyHash       string
	WebhookURL       string
	WebhookSecretEnc []byte
	FeeRateBps       int32
	// PayoutFrozen 深回滚致 MERCHANT_REORG_RECEIVABLE > 0 时自动置 true,
	// 归零后经管理员复核解除 (§4.3).
	PayoutFrozen bool
	CreatedAt    time.Time
}

// MerchantWithdrawalAddress 商户提现白名单地址 (merchant_withdrawal_addresses).
type MerchantWithdrawalAddress struct {
	ID               uuid.UUID
	MerchantID       uuid.UUID
	ChainID          ChainID
	Address          Address
	AdminApprovalSig []byte // 69B 载荷对应的 64B Ed25519 签名
	ValidUntil       time.Time
	Status           WithdrawalAddressStatus
}

// DepositAddress 1:1 订单专属 CREATE2 收款地址 (deposit_addresses).
type DepositAddress struct {
	ID            uuid.UUID
	ChainID       ChainID
	Address       Address
	Create2Salt   Hash // bytea 32B: keccak256(abi.encode(chainId, merchantId, orderId))
	IsDeployed    bool
	IsQuarantined bool // 归集 Revert (黑名单/异常) 被外层捕获后置 true
	CreatedAt     time.Time
}

// PaymentOrder 支付订单 (payment_orders).
type PaymentOrder struct {
	ID               uuid.UUID
	MerchantID       uuid.UUID
	IdempotencyKey   string
	RequestHash      string
	ChainID          ChainID
	AssetSymbol      string
	ExpectedAmount   U256
	ReceivedAmount   U256 // 仅累计同币种有效入账
	DepositAddressID uuid.UUID
	Status           OrderStatus
	ExpiresAt        time.Time
	SettledAt        *time.Time
}

// ChainBlock 已索引区块 (chain_blocks, PK (chain_id, block_hash)).
type ChainBlock struct {
	ChainID     ChainID
	BlockHash   Hash
	BlockNumber uint64
	ParentHash  Hash
	Status      ChainBlockStatus
	IndexedAt   time.Time
}

// OnchainDeposit 链上入站记录 (onchain_deposits,
// 四列唯一键 chain_id+block_hash+tx_hash+log_index — 新分叉块重打包可无冲突再入库).
type OnchainDeposit struct {
	ID             uuid.UUID
	ChainID        ChainID
	BlockHash      Hash
	TxHash         Hash
	LogIndex       int32
	BlockNumber    uint64
	TokenContract  Address
	FromAddress    Address
	ToAddress      Address
	Amount         U256
	Category       DepositCategory
	Status         DepositStatus
	MatchedOrderID *uuid.UUID // 可空: 分支 A/B/C 不关联活跃订单
}

// WalletNonceCursor 热钱包持久化 Nonce 游标 (wallet_nonce_cursors).
type WalletNonceCursor struct {
	ChainID       ChainID
	WalletAddress Address
	NextNonce     uint64
	UpdatedAt     time.Time
}

// OutboundTxIntent 出站交易意图 (outbound_tx_intents).
type OutboundTxIntent struct {
	ID            uuid.UUID
	IntentType    IntentType
	ReferenceID   uuid.UUID // sweep_batch_id / merchant_payout_id / order_id
	ChainID       ChainID
	WalletAddress Address
	Nonce         uint64
	Status        TxIntentStatus
	CreatedAt     time.Time
}

// OutboundTxAttempt 同 Nonce 候选交易 (outbound_tx_attempts).
type OutboundTxAttempt struct {
	ID                   uuid.UUID
	IntentID             uuid.UUID
	AttemptNo            int32 // RBF 提价/补发时 +1
	Kind                 AttemptKind
	ChainID              ChainID
	TxHash               Hash
	BlockNumber          *uint64
	BlockHash            *Hash // LIFO 回滚后清空, 支持新分叉链重打包
	ReceiptStatus        *int32
	MaxFeePerGas         U256
	MaxPriorityFeePerGas U256
	GasUsed              *uint64
	EffectiveGasPrice    *U256
	Status               TxAttemptStatus
	BroadcastedAt        time.Time
}

// SweepBatch 定额归集批次 (sweep_batches, 单批 <= 50 item).
type SweepBatch struct {
	ID               uuid.UUID
	ChainID          ChainID
	AssetSymbol      string
	TargetColdWallet Address
	TotalSweptAmount U256
	Status           SweepBatchStatus
	CreatedAt        time.Time
}

// SweepBatchItem 归集批次单条目 (sweep_batch_items).
type SweepBatchItem struct {
	ID                uuid.UUID
	BatchID           uuid.UUID
	DepositAddressID  uuid.UUID
	ExactSweepAmount  U256 // 账本已 FINALIZED 的精确额度
	ActualSweptAmount U256
	ItemStatus        SweepItemStatus
}

// MerchantPayout 商户提现/客户退款单 (merchant_payouts).
type MerchantPayout struct {
	ID                uuid.UUID
	MerchantID        uuid.UUID
	IdempotencyKey    string
	RequestHash       string
	PayoutKind        PayoutKind
	SourceAccount     LedgerAccountCode // 必须为 PayoutSourceAccounts 之一
	SourceOrderID     *uuid.UUID        // CUSTOMER_REFUND 关联订单 (可空)
	ChainID           ChainID
	AssetSymbol       string
	ToAddress         Address
	Amount            U256
	RetrySeq          int32  // Revert 换号重试时递增, 退款须重签票据
	AdminRefundTicket []byte // CUSTOMER_REFUND 的 127B 票据载荷+签名打包字节
	Status            PayoutStatus
	CreatedAt         time.Time
}

// WebhookMessage Transactional Outbox 消息 (webhook_outbox).
type WebhookMessage struct {
	ID          uuid.UUID
	MerchantID  uuid.UUID
	EventType   string
	PayloadJSON []byte
	RetryCount  int32
	NextRetryAt time.Time
	Status      WebhookStatus
	CreatedAt   time.Time
}

// LedgerAccount 账本账户 (ledger_accounts,
// 唯一键 owner_merchant_id+chain_id+asset_symbol+account_type; 系统科目 owner=SystemMerchantID).
type LedgerAccount struct {
	ID              uuid.UUID
	OwnerMerchantID uuid.UUID
	ChainID         ChainID
	AssetSymbol     string
	Account         LedgerAccountCode
	NormalSide      Direction
	Balance         U256 // CHECK balance >= 0
}

// LedgerTransaction 复式记账事务头 (ledger_transactions,
// 幂等唯一键 reference_type+reference_id).
type LedgerTransaction struct {
	ID            uuid.UUID
	ReferenceType EntryTemplateCode
	ReferenceID   string
	Description   string
	CreatedAt     time.Time
}

// LedgerEntry 记账分录腿 (ledger_entries, CHECK amount > 0).
type LedgerEntry struct {
	ID            uuid.UUID
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	Direction     Direction
	Amount        U256
	BalanceAfter  U256 // 按 account_id 升序加锁后的余额快照
	CreatedAt     time.Time
}

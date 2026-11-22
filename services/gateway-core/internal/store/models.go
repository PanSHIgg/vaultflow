package store

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 错误哨兵
// ---------------------------------------------------------------------------

var (
	// ErrDepositUnassigned onchain_deposits.user_id 为 NULL (UNMATCHED/国库类
	// 充值或 indexer 尚未回填归属), 无法贷记用户余额.
	ErrDepositUnassigned = errors.New("store: deposit has no resolved user")
	// ErrUnknownAsset deposit 的 (chain_id, token_contract) 不在 supported_assets
	// 白名单内, 无法确定 asset_symbol.
	ErrUnknownAsset = errors.New("store: deposit token contract not whitelisted")
	// ErrInvalidCursor 分页游标无法解码.
	ErrInvalidCursor = errors.New("store: invalid pagination cursor")
)

// ---------------------------------------------------------------------------
// 领域枚举 (008_exchange.sql CHECK 白名单, 字符串即数据库原值)
// ---------------------------------------------------------------------------

// onchain_deposits.status — 用户充值状态机.
const (
	DepositStatusDetected        = "DETECTED"
	DepositStatusFinalized       = "FINALIZED"
	DepositStatusCredited        = "CREDITED"
	DepositStatusSwept           = "SWEPT"
	DepositStatusOrphanedShallow = "ORPHANED_SHALLOW"
	DepositStatusOrphanedDeep    = "ORPHANED_DEEP"
)

// onchain_deposits.deposit_category — 充值分类.
const (
	DepositCategoryUserDeposit       = "USER_DEPOSIT"
	DepositCategoryTreasuryRebalance = "TREASURY_REBALANCE"
	DepositCategoryTopup             = "TOPUP"
	DepositCategoryUnmatched         = "UNMATCHED"
)

// platform_wallets.role.
const (
	WalletRoleRelayer = "RELAYER"
	WalletRolePayout  = "PAYOUT"
	WalletRoleCold    = "COLD"
)

// platform_wallets.status.
const (
	WalletStatusActive   = "ACTIVE"
	WalletStatusDisabled = "DISABLED"
)

// ---------------------------------------------------------------------------
// 行类型
// ---------------------------------------------------------------------------

// User users 表行.
type User struct {
	ID           uuid.UUID
	Email        string // 应用侧已规范化小写
	PasswordHash string // bcrypt
	CreatedAt    time.Time
}

// UserDepositAddress user_deposit_addresses 表行 — 每用户每链一个常驻
// CREATE2 充值地址 (salt = keccak256(abi.encode(chainId, bytes16(userId)))).
type UserDepositAddress struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	ChainID       types.ChainID
	Address       types.Address
	Create2Salt   types.Hash // 32B
	IsDeployed    bool       // EIP-1167 proxy 已被归集器部署
	IsQuarantined bool       // VaultQuarantined 黑名单/revert
	CreatedAt     time.Time
}

// UserBalance user_balances 表行 (+LEFT JOIN supported_assets 的展示字段).
type UserBalance struct {
	UserID      uuid.UUID
	AssetSymbol string
	Available   types.U256
	Pending     types.U256
	Decimals    int // LEFT JOIN supported_assets; 白名单缺失时 -1
	UpdatedAt   time.Time
}

// DepositRecord onchain_deposits 读投影 (+LEFT JOIN supported_assets 的展示字段).
type DepositRecord struct {
	ID            uuid.UUID
	ChainID       types.ChainID
	BlockHash     types.Hash
	TxHash        types.Hash
	LogIndex      int
	BlockNumber   int64
	TokenContract types.Address
	FromAddress   types.Address
	ToAddress     types.Address
	Amount        types.U256
	Category      string // DepositCategory*
	Status        string // DepositStatus*
	UserID        uuid.NullUUID
	AssetSymbol   string // LEFT JOIN; UNMATCHED/未白名单为 ""
	Decimals      int    // LEFT JOIN; 未白名单为 -1
	CreatedAt     time.Time
}

// DepositPage ListUserDeposits 返回页. NextCursor 不透明 (base64), HasMore
// 为 true 时回传下一页请求; 否则为空串.
type DepositPage struct {
	Data       []DepositRecord
	NextCursor string
	HasMore    bool
}

// PlatformWallet platform_wallets 表行.
type PlatformWallet struct {
	ID        uuid.UUID
	ChainID   types.ChainID
	Role      string // WalletRole*
	Address   types.Address
	Status    string // WalletStatus*
	CreatedAt time.Time
}

// AssetInfo supported_assets 表行 (含 007 min_deposit 与 008 链元数据).
type AssetInfo struct {
	ChainID               types.ChainID
	AssetSymbol           string
	TokenContract         types.Address
	Decimals              int
	SupportsEIP3009       bool
	MinDeposit            types.U256
	ChainName             string
	BlockTimeSeconds      int
	RequiredConfirmations int
	TokenExplorerURL      string
}

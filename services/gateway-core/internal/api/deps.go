package api

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/sse"
	"vaultflow/gateway-core/internal/types"
)

// ===========================================================================
// 配置 (Wave-2 I1 由 yaml/env 装配注入)
// ===========================================================================

// ChainConfig 单链部署参数 (合约地址 / 热钱包 / 确认阈值 / Relay Gas 策略).
type ChainConfig struct {
	ChainID               types.ChainID
	RequiredConfirmations uint64        // safe_head 滞后块数 (确认进度条阈值)
	EIP3009Router         types.Address // 链上 EIP3009PayRouter (USDC 链)
	RelayerHotWallet      types.Address // 归集与中继热钱包
	PayoutHotWallet       types.Address // 提现与退款热钱包
	RelayGasLimit         uint64        // EIP-3009 relay intent gas_limit
	MaxFeePerGas          types.U256    // EIP-1559 费率上界 (十进制原子单位)
	MaxPriorityFeePerGas  types.U256
}

// RateLimitRule 单个限流桶策略 (固定窗口计数: Limit 次 / Window).
type RateLimitRule struct {
	Limit  int64
	Window time.Duration
}

// Config 服务运行配置.
type Config struct {
	// Chains 链配置表 (chain_id -> 部署参数).
	Chains map[types.ChainID]ChainConfig
	// CashierURLFormat 收银台相对路径模板, 占位 %s = order_id.
	CashierURLFormat string

	RateMerchant RateLimitRule // 商户端按 merchant_id 限流
	RatePublic   RateLimitRule // 公开端按 client IP 限流
	RateAdmin    RateLimitRule // 管理端按 JWT sub 限流

	// IdempotencyTTL 幂等记录保留期.
	IdempotencyTTL time.Duration
	// MaxBodyBytes 请求体读取上限 (幂等中间件与解码共用).
	MaxBodyBytes int64
	// SSEHeartbeatInterval SSE ": heartbeat" 注释行周期 (openapi: 15s).
	SSEHeartbeatInterval time.Duration
	// SSERetryMillis SSE "retry:" 字段 (openapi: 3000).
	SSERetryMillis int
	// ReconIntervalSec 对外展示的审计链巡检周期 (秒).
	ReconIntervalSec int
	// SimEnabled 等价 ENABLE_DEV_SIMULATOR=true: 挂载 /v1/sim/** 演练面.
	SimEnabled bool
}

// DefaultConfig 提供可用的开发默认 (I1 可整体覆盖).
func DefaultConfig() Config {
	return Config{
		Chains:               map[types.ChainID]ChainConfig{},
		CashierURLFormat:     "/pay/%s",
		RateMerchant:         RateLimitRule{Limit: 600, Window: time.Minute},
		RatePublic:           RateLimitRule{Limit: 120, Window: time.Minute},
		RateAdmin:            RateLimitRule{Limit: 120, Window: time.Minute},
		IdempotencyTTL:       24 * time.Hour,
		MaxBodyBytes:         1 << 20,
		SSEHeartbeatInterval: 15 * time.Second,
		SSERetryMillis:       3000,
		ReconIntervalSec:     300,
	}
}

// ===========================================================================
// Deps — Server 依赖注入面. 业务调用一律走冻结 ports 接口;
// ports 未覆盖的读侧投影由 ReadStore 补齐 (Wave-2 I1 PG 实现).
// ===========================================================================

// EventStreamer SSE 富订阅接口 — internal/sse.Broker 的超集
// (ports.SSEBroker 的 SubscribeOrder 不带 seq, 无法支撑 Last-Event-ID 回放;
// 该接口由本窗口 sse.Broker.Subscribe 满足).
type EventStreamer interface {
	// Subscribe 订阅单订单事件流; 先回放 seq > afterSeq 的缓冲事件再推实时事件.
	Subscribe(ctx context.Context, orderID uuid.UUID, afterSeq uint64) (events <-chan sse.StreamEvent, cancel func(), err error)
}

// AnvilPassthrough Anvil 裸调用透传面 (internal/sim.AnvilRouter 实现).
type AnvilPassthrough interface {
	// Invoke 执行 action 对应的 evm_*/anvil_* JSON-RPC, 原样返回 result.
	Invoke(ctx context.Context, chainID uint64, action string, params map[string]any) (any, error)
}

// Deps Server 全部注入依赖 (nil 安全: 未注入的依赖对应路由返回 503).
type Deps struct {
	Config Config
	Logger *zap.Logger

	// ports 冻结业务接口 (W3/W4 实现, Wave-2 装配).
	Orders    ports.OrderService
	Ledger    ports.LedgerService
	TxManager ports.TxManager
	Signer    ports.SignerClient
	Sim       ports.SimulatorService

	// Streamer SSE 事件源 (internal/sse.Broker).
	Streamer EventStreamer
	// Store ports 之外的读侧/写侧投影 (I1 PG 实现).
	Store ReadStore
	// IdemStore 幂等记录存储 (Redis 优先, 进程内降级).
	IdemStore IdempotencyStore
	// Limiter 频控 (Redis 断连降级放行).
	Limiter *RateLimiter
	// AdminJWTKey 管理端 HS256 JWT 校验密钥.
	AdminJWTKey []byte
	// Anvil Anvil 透传控制器 (仅 SimEnabled 时有意义).
	Anvil AnvilPassthrough

	// nowFn 可注入时钟 (测试用).
	nowFn func() time.Time
}

func (d *Deps) now() time.Time {
	if d.nowFn != nil {
		return d.nowFn()
	}
	return time.Now().UTC()
}

// ===========================================================================
// ReadStore — 冻结 ports 未覆盖的读侧投影契约.
//
// ⚠ 契约说明 (Wave-2 合并清单): openapi.yaml 的 27 个操作中大量 list/详情
// 投影在 ports.* 接口上不存在对应方法 (端口只冻结了写路径与单查), 因此 W5
// 自定义本读侧接口, 由 I1 窗口以 pgx 实现. 单测以 fake 驱动.
// ===========================================================================

// Keyset 通用 keyset 分页游标 (created_at DESC, id DESC 序列).
type Keyset struct {
	Sort time.Time // 排序键 (多数表 = created_at)
	ID   uuid.UUID // 末行主键, 同键决胜
}

// OrderListQuery 商户订单列表查询.
type OrderListQuery struct {
	MerchantID  uuid.UUID
	Statuses    []types.OrderStatus
	ChainID     *types.ChainID
	AssetSymbol string
	MerchantRef string // ⚠ payment_orders 当前 DDL 无 merchant_order_ref 列 (契约缺陷, 已上报)
	After       *Keyset
	Limit       int
}

// OrderPage 订单分页.
type OrderPage struct {
	Data    []types.PaymentOrder
	HasMore bool
}

// PayoutListQuery 提现/退款单列表查询.
type PayoutListQuery struct {
	MerchantID    uuid.UUID
	Statuses      []types.PayoutStatus
	Kind          *types.PayoutKind
	SourceAccount *types.LedgerAccountCode
	ChainID       *types.ChainID
	After         *Keyset
	Limit         int
}

// PayoutPage 提现/退款分页.
type PayoutPage struct {
	Data    []types.MerchantPayout
	HasMore bool
}

// WhitelistQuery 提现白名单列表查询.
// ⚠ merchant_withdrawal_addresses 无 created_at 列, 用 id 游标稳定分页.
type WhitelistQuery struct {
	MerchantID uuid.UUID
	Statuses   []types.WithdrawalAddressStatus
	ChainID    *types.ChainID
	After      *uuid.UUID
	Limit      int
}

// WhitelistPage 白名单分页.
type WhitelistPage struct {
	Data    []types.MerchantWithdrawalAddress
	HasMore bool
}

// NewWithdrawalAddress 白名单注册入库投影.
type NewWithdrawalAddress struct {
	MerchantID uuid.UUID
	ChainID    types.ChainID
	Address    types.Address
	AdminSig   []byte
	ValidUntil time.Time
}

// LedgerAccountQuery 14 科目余额查询.
type LedgerAccountQuery struct {
	Owners      []uuid.UUID // 调用方商户 + SystemMerchantID (或显式单个 owner)
	ChainID     *types.ChainID
	AssetSymbol string
	Accounts    []types.LedgerAccountCode
}

// LedgerTxQuery 复式记账事务头查询.
type LedgerTxQuery struct {
	RefTypes []types.EntryTemplateCode
	RefID    string
	After    *Keyset
	Limit    int
}

// LedgerTxPage 事务头分页.
type LedgerTxPage struct {
	Data    []types.LedgerTransaction
	HasMore bool
}

// LedgerEntryView 分录腿视图 (join ledger_accounts 补 account_type/owner).
type LedgerEntryView struct {
	Entry           types.LedgerEntry
	AccountCode     types.LedgerAccountCode
	OwnerMerchantID uuid.UUID
}

// LedgerEntryQuery 分录腿查询.
type LedgerEntryQuery struct {
	TransactionID *uuid.UUID
	AccountID     *uuid.UUID
	Direction     *types.Direction
	After         *Keyset
	Limit         int
}

// LedgerEntryPage 分录分页.
type LedgerEntryPage struct {
	Data    []LedgerEntryView
	HasMore bool
}

// NonceCursorView 热钱包 Nonce 游标 + 熔断标记 (供监控台).
type NonceCursorView struct {
	Cursor types.WalletNonceCursor
	Halted bool // 存在 NONCE_STOLEN_HALT 意图
}

// OutboundListQuery 出站意图列表查询.
type OutboundListQuery struct {
	Statuses   []types.TxIntentStatus
	IntentType *types.IntentType
	ChainID    *types.ChainID
	Wallet     *types.Address
	After      *Keyset
	Limit      int
}

// OutboundPage 出站意图分页.
type OutboundPage struct {
	Data    []types.OutboundTxIntent
	HasMore bool
}

// QuarantinedVaultView 隔离金库视图 (deposit_addresses join sweep_batch_items +
// payment_orders 尽力而为; DDL 无 reason/quarantined_at 列 — 契约缺陷已上报).
type QuarantinedVaultView struct {
	DepositAddressID uuid.UUID
	ChainID          types.ChainID
	Address          types.Address
	IsQuarantined    bool
	AssetSymbol      string
	Reason           string
	SweepBatchID     *uuid.UUID
	SweepBatchItemID *uuid.UUID
	ItemStatus       string
	RelatedOrderID   *uuid.UUID
	QuarantinedAt    *time.Time
}

// QuarantineQuery 隔离金库查询.
type QuarantineQuery struct {
	ChainID *types.ChainID
	After   *Keyset
	Limit   int
}

// QuarantinePage 隔离金库分页.
type QuarantinePage struct {
	Data    []QuarantinedVaultView
	HasMore bool
}

// ReadStore 读侧投影接口 (Wave-2 I1 PG 实现).
type ReadStore interface {
	// ---- 鉴权与商户 ----
	// ResolveAPIKey 按 SHA-256(api_key) hex 反查商户; 未命中 ErrNotFound.
	ResolveAPIKey(ctx context.Context, apiKeyHash string) (types.Merchant, error)
	GetMerchant(ctx context.Context, id uuid.UUID) (types.Merchant, error)
	// MerchantReceivableBalance 返回商户 MERCHANT_REORG_RECEIVABLE 合计余额.
	MerchantReceivableBalance(ctx context.Context, merchantID uuid.UUID) (types.U256, error)

	// ---- 订单 / 充值 / 资产 ----
	GetDepositAddress(ctx context.Context, id uuid.UUID) (types.DepositAddress, error)
	GetSupportedAsset(ctx context.Context, chainID types.ChainID, symbol string) (types.SupportedAsset, error)
	ListSupportedAssets(ctx context.Context) ([]types.SupportedAsset, error)
	ListOrders(ctx context.Context, q OrderListQuery) (OrderPage, error)
	// ListOrderDeposits 按 block_number DESC 返回订单关联入账记录 (SSE 明细用).
	ListOrderDeposits(ctx context.Context, orderID uuid.UUID, limit int) ([]types.OnchainDeposit, error)

	// ---- 提现 / 退款 ----
	ListPayouts(ctx context.Context, q PayoutListQuery) (PayoutPage, error)
	// FindOrderRefundPayout 找订单关联的 CUSTOMER_REFUND 单 (review APPROVE_REFUND 回显).
	FindOrderRefundPayout(ctx context.Context, orderID uuid.UUID) (types.MerchantPayout, error)
	// FindIntentByReference 按 outbound_tx_intents.reference_id 反查意图.
	FindIntentByReference(ctx context.Context, referenceID uuid.UUID) (types.OutboundTxIntent, error)
	// LatestFinalizedAttempt 取意图最新 FINALIZED 候选 (tx_hash 回显).
	LatestFinalizedAttempt(ctx context.Context, intentID uuid.UUID) (types.OutboundTxAttempt, error)

	// ---- 提现白名单 ----
	CreateWithdrawalAddress(ctx context.Context, rec NewWithdrawalAddress) (types.MerchantWithdrawalAddress, error)
	GetWithdrawalAddress(ctx context.Context, id uuid.UUID) (types.MerchantWithdrawalAddress, error)
	ListWithdrawalAddresses(ctx context.Context, q WhitelistQuery) (WhitelistPage, error)
	MarkWithdrawalAddressRevoked(ctx context.Context, id uuid.UUID) (types.MerchantWithdrawalAddress, error)

	// ---- 账本 ----
	ListLedgerAccounts(ctx context.Context, q LedgerAccountQuery) ([]types.LedgerAccount, error)
	ListLedgerTransactions(ctx context.Context, q LedgerTxQuery) (LedgerTxPage, error)
	ListLedgerEntries(ctx context.Context, q LedgerEntryQuery) (LedgerEntryPage, error)

	// ---- 运维 ----
	GetChainSyncCursor(ctx context.Context, chainID types.ChainID) (types.ChainSyncCursor, error)
	ListNonceCursors(ctx context.Context, chainID *types.ChainID) ([]NonceCursorView, error)
	ListOutboundIntents(ctx context.Context, q OutboundListQuery) (OutboundPage, error)
	ListQuarantinedVaults(ctx context.Context, q QuarantineQuery) (QuarantinePage, error)
}

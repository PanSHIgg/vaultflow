package api

import (
	"context"
	"time"

	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/exchange"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// ===========================================================================
// 配置 (cmd/gateway 由 env/CHAIN_CONFIG_JSON 装配注入)
// ===========================================================================

// ChainConfig 单链 API 面配置 — gateway 进程不持有 RPC/钱包地址,
// 仅保留链存在性 (deposit-address 校验) 与展示用确认数阈值.
type ChainConfig struct {
	ChainID               types.ChainID
	RequiredConfirmations int // required_confirmations 兜底 (资产行缺失时使用)
}

// RateLimitRule 单个限流桶策略 (固定窗口计数: Limit 次 / Window).
type RateLimitRule struct {
	Limit  int64
	Window time.Duration
}

// Config 服务运行配置.
type Config struct {
	// Chains 已接入链表 (chain_id -> API 面配置); 决定 /v1/me/deposit-address 合法性.
	Chains map[types.ChainID]ChainConfig

	RateAuth   RateLimitRule // /v1/auth/* 按 client IP 限流 (防爆破)
	RatePublic RateLimitRule // 公开端按 client IP 限流
	RateUser   RateLimitRule // /v1/me/* 按 user_id 限流
	RateAdmin  RateLimitRule // 管理端按 JWT sub 限流

	// MaxBodyBytes 请求体读取上限.
	MaxBodyBytes int64
}

// DefaultConfig 提供可用的开发默认 (装配层可整体覆盖).
func DefaultConfig() Config {
	return Config{
		Chains:       map[types.ChainID]ChainConfig{},
		RateAuth:     RateLimitRule{Limit: 30, Window: time.Minute},
		RatePublic:   RateLimitRule{Limit: 120, Window: time.Minute},
		RateUser:     RateLimitRule{Limit: 300, Window: time.Minute},
		RateAdmin:    RateLimitRule{Limit: 120, Window: time.Minute},
		MaxBodyBytes: 1 << 20,
	}
}

// ===========================================================================
// Deps — Server 依赖注入面.
// ===========================================================================

// SignerAdmin admin 面所需的最小 vault-signer 接口
// (*signerclient.Client 满足; 接口收窄便于单测 fake 注入).
// GenerateWallet / UpdateColdWallets 为平台钱包 CRUD 的签名机联动通道
// (RELAYER/PAYOUT 由 signer KMS 生成密钥对; COLD 增删同步 allowed_cold_wallets).
type SignerAdmin interface {
	// Health 健康度与运行态巡检 (版本/时钟/白名单规模/审计链 tip/已加载钱包).
	Health(ctx context.Context) (ports.SignerHealth, error)
	// GenerateWallet 令 signer KMS 为 (chain, role) 生成新热钱包密钥对, 返回地址.
	GenerateWallet(ctx context.Context, chainID types.ChainID, role string) (types.Address, error)
	// UpdateColdWallets 增删 signer 侧 allowed_cold_wallets, 返回变更后全量列表.
	UpdateColdWallets(ctx context.Context, chainID types.ChainID, add, remove []types.Address) ([]types.Address, error)
}

// Deps Server 全部注入依赖 (nil 安全: Signer 未注入时 admin 写路径返回 502).
type Deps struct {
	Config Config
	Logger *zap.Logger

	// Exchange 用户域业务门面 (注册/登录/充值地址惰性分配/状态映射).
	Exchange *exchange.Service
	// Store 用户域读侧投影 (balances/deposits/assets/wallets/cursors).
	Store *store.Store
	// Signer admin 面签名机通道 (nil 时 signer 联动接口返回 502).
	Signer SignerAdmin
	// Limiter 频控 (Redis 断连降级放行; nil 时完全放行).
	Limiter *RateLimiter
	// UserJWTKey 终端用户 HS256 JWT 校验密钥 (env JWT_SECRET).
	UserJWTKey []byte
	// AdminJWTKey 管理端 HS256 JWT 校验密钥 (env ADMIN_JWT_SECRET).
	AdminJWTKey []byte

	// nowFn 可注入时钟 (测试用).
	nowFn func() time.Time
}

func (d *Deps) now() time.Time {
	if d.nowFn != nil {
		return d.nowFn()
	}
	return time.Now().UTC()
}

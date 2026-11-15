// config.go — Wave-2 I1 环境变量装配配置 (stdlib only, 无 Viper).
//
// 全部链级部署参数经单个结构化 JSON (CHAIN_CONFIG_JSON) 注入, 由
// scripts/deploy-and-seed.sh 从 forge broadcast 输出生成; 其余标量走散置 env.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"vaultflow/gateway-core/internal/types"
)

// chainRuntime 单链装配参数 (CHAIN_CONFIG_JSON.chains 元素).
type chainRuntime struct {
	ChainID               types.ChainID   `json:"chain_id"`
	RPCURLs               []string        `json:"rpc_urls"`
	RequiredConfirmations uint64          `json:"required_confirmations"`
	EIP3009Router         types.Address   `json:"eip3009_router"`
	BatchSweeper          types.Address   `json:"batch_sweeper"`
	RelayerHotWallet      types.Address   `json:"relayer_hot_wallet"`
	PayoutHotWallet       types.Address   `json:"payout_hot_wallet"`
	ColdWallets           []types.Address `json:"cold_wallets"`
	RelayGasLimit         uint64          `json:"relay_gas_limit"`
	MaxFeePerGas          string          `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas  string          `json:"max_priority_fee_per_gas"`
	PayoutGasLimit        uint64          `json:"payout_gas_limit"`
	StartHeight           *uint64         `json:"start_height"`
	// DevnetTags 显式 opt-in: 仅 devnet/anvil 链置真时才包 DevnetTagAdapter
	// (将 safe=0 与 latest-safe>requiredConfs 的陈旧标签转译 NotFound).
	// 生产链必须缺省 false — 主网 safe 天然滞后 ~32-64 块, 误接会把合法
	// 标签全部打成 NotFound 并把安全线降级为 latest-requiredConfs.
	DevnetTags bool `json:"devnet_tags"`
	// FundingAccount dev 演练注资源 (anvil_impersonateAccount 源; 仅 SimEnabled 用).
	FundingAccount types.Address `json:"funding_account"`
}

// deploymentConfig CHAIN_CONFIG_JSON 根对象 — 合约部署面 + 每链参数.
type deploymentConfig struct {
	Factory        types.Address            `json:"factory"`
	Implementation types.Address            `json:"implementation"`
	InitCodeHash   types.Hash               `json:"init_code_hash"`
	AnchorChainID  int64                    `json:"anchor_chain_id"`
	Chains         map[string]*chainRuntime `json:"chains"`
}

// devAdminJWTSecret 旧 dev 字面量 — 仅用于拒绝比对, 绝不作为密钥回退.
const devAdminJWTSecret = "vaultflow-dev-admin-secret"

// appConfig 全量装配配置.
type appConfig struct {
	Port           int
	DatabaseURL    string
	RedisURL       string // 可空 → 进程内降级
	MigrationsDir  string
	SignerEndpoint string
	SignerID       string // signer_audit_anchor 锚点标识 (空 → Endpoint)
	AnchorChainID  int64

	MTLSCertFile   string
	MTLSKeyFile    string
	MTLSCAFile     string
	MTLSServerName string // 证书 CN (compose 内 vault-signer)

	AdminJWTKey         []byte
	SimEnabled          bool
	WebhookInsecureHTTP bool
	Deploy              deploymentConfig
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt64(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// loadAppConfig 解析全部 env; 缺失必填项即 error (main fatal 拒绝启动).
func loadAppConfig() (*appConfig, error) {
	cfg := &appConfig{
		Port:                int(envInt64("PORT", 8080)),
		DatabaseURL:         strings.TrimSpace(os.Getenv("DATABASE_URL")),
		RedisURL:            strings.TrimSpace(os.Getenv("REDIS_URL")),
		MigrationsDir:       envStr("MIGRATIONS_DIR", "migrations"),
		SignerEndpoint:      envStr("SIGNER_ADDR", "vault-signer:50051"),
		SignerID:            envStr("SIGNER_ID", ""),
		AnchorChainID:       envInt64("ANCHOR_CHAIN_ID", 0),
		MTLSCertFile:        envStr("MTLS_CLIENT_CERT", "certs/clients/gateway-core.crt"),
		MTLSKeyFile:         envStr("MTLS_CLIENT_KEY", "certs/clients/gateway-core.key"),
		MTLSCAFile:          envStr("MTLS_CA_CERT", "certs/ca/ca.crt"),
		MTLSServerName:      envStr("MTLS_SERVER_NAME", "vault-signer"),
		AdminJWTKey:         []byte(strings.TrimSpace(os.Getenv("ADMIN_JWT_SECRET"))),
		SimEnabled:          envBool("ENABLE_DEV_SIMULATOR", false),
		WebhookInsecureHTTP: envBool("WEBHOOK_ALLOW_INSECURE_HTTP", false),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	// ADMIN_JWT_SECRET 为 /v1/admin/** + /v1/sim/** 唯一鉴权根 — 禁止回退到
	// 任何内置默认: 空即拒启动; 命中已知公开 dev 字面量需
	// ALLOW_DEV_ADMIN_SECRET=true 显式 opt-in (否则与全网可铸的管理端令牌
	// 等价); 其余值最小 32B 对齐 HS256 密钥强度惯例.
	if len(cfg.AdminJWTKey) == 0 {
		return nil, fmt.Errorf("config: ADMIN_JWT_SECRET is required")
	}
	if string(cfg.AdminJWTKey) == devAdminJWTSecret {
		if !envBool("ALLOW_DEV_ADMIN_SECRET", false) {
			return nil, fmt.Errorf("config: ADMIN_JWT_SECRET equals the public dev value; generate a real secret (deploy-and-seed.sh does) or set ALLOW_DEV_ADMIN_SECRET=true for local dev only")
		}
	} else if len(cfg.AdminJWTKey) < 32 {
		return nil, fmt.Errorf("config: ADMIN_JWT_SECRET must be at least 32 bytes")
	}
	raw := strings.TrimSpace(os.Getenv("CHAIN_CONFIG_JSON"))
	if raw == "" {
		return nil, fmt.Errorf("config: CHAIN_CONFIG_JSON is required (write by deploy-and-seed.sh)")
	}
	if err := json.Unmarshal([]byte(raw), &cfg.Deploy); err != nil {
		return nil, fmt.Errorf("config: parse CHAIN_CONFIG_JSON: %w", err)
	}
	if cfg.Deploy.Factory == (types.Address{}) {
		return nil, fmt.Errorf("config: CHAIN_CONFIG_JSON.factory is required")
	}
	if cfg.Deploy.Implementation == (types.Address{}) {
		return nil, fmt.Errorf("config: CHAIN_CONFIG_JSON.implementation is required")
	}
	if len(cfg.Deploy.Chains) == 0 {
		return nil, fmt.Errorf("config: CHAIN_CONFIG_JSON.chains must declare at least one chain")
	}
	for key, cc := range cfg.Deploy.Chains {
		if cc == nil || cc.ChainID <= 0 {
			return nil, fmt.Errorf("config: chain %q missing chain_id", key)
		}
		if len(cc.RPCURLs) == 0 {
			return nil, fmt.Errorf("config: chain %d missing rpc_urls", cc.ChainID)
		}
	}
	if cfg.Deploy.AnchorChainID != 0 && cfg.AnchorChainID == 0 {
		cfg.AnchorChainID = cfg.Deploy.AnchorChainID
	}
	return cfg, nil
}

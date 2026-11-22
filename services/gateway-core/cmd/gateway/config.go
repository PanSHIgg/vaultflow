// config.go — gateway 进程环境变量装配配置 (stdlib only, 无 Viper).
//
// 交易所托管模型下 gateway 仅承担对外 REST API 面: 无链 worker、无 RPC 拨号.
// CHAIN_CONFIG_JSON 沿用 deploy-and-seed.sh 的同构输出, 但 gateway 只消费
// factory / implementation (CREATE2 推导器) 与 chains[].chain_id /
// required_confirmations (链合法性与展示阈值) — 其余链级字段 (rpc_urls /
// 热钱包 / 合约地址等) 宽松忽略, 不校验.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/types"
)

// chainRuntime 单链 API 面参数 (CHAIN_CONFIG_JSON.chains 元素的容错子集).
type chainRuntime struct {
	ChainID               types.ChainID `json:"chain_id"`
	RequiredConfirmations int           `json:"required_confirmations"`
	// Factory/Implementation 可选覆盖 — 公链部署地址与全局 deploy 面不
	// 一致时逐链指定 (devnet 同 deployer 部署可留空回落全局值).
	Factory        types.Address `json:"factory"`
	Implementation types.Address `json:"implementation"`
}

// deploymentConfig CHAIN_CONFIG_JSON 根对象 (仅 gateway 关心的字段).
type deploymentConfig struct {
	Factory        types.Address            `json:"factory"`
	Implementation types.Address            `json:"implementation"`
	InitCodeHash   types.Hash               `json:"init_code_hash"`
	AnchorChainID  int64                    `json:"anchor_chain_id"`
	Chains         map[string]*chainRuntime `json:"chains"`
}

// Deriver 解析某链的 CREATE2 推导器 — 逐链覆盖优先, 缺省回落全局部署面.
// Implementation 为零值时不可构造 (无法推导 init_code) — 返回 nil.
func (c *deploymentConfig) Deriver(chainID types.ChainID) *create2.Deriver {
	factory, impl := c.Factory, c.Implementation
	if cc, ok := c.Chains[strconv.FormatUint(uint64(chainID), 10)]; ok {
		if cc.Factory != (types.Address{}) {
			factory = cc.Factory
		}
		if cc.Implementation != (types.Address{}) {
			impl = cc.Implementation
		}
	}
	if impl == (types.Address{}) {
		return nil
	}
	return create2.NewDeriver(factory, impl)
}

// appConfig 全量装配配置.
type appConfig struct {
	Port           int
	DatabaseURL    string
	RedisURL       string // 可空 → 进程内降级限流
	MigrationsDir  string
	JWTSecret      []byte // 终端用户 HS256 JWT (必填, 无默认)
	AdminJWTKey    []byte // 管理端 HS256 JWT
	SignerEndpoint string
	SignerID       string // signer_audit_anchor 锚点标识 (空 → Endpoint)
	AnchorChainID  int64

	MTLSCertFile   string
	MTLSKeyFile    string
	MTLSCAFile     string
	MTLSServerName string // 证书 CN (compose 内 vault-signer)

	Deploy deploymentConfig
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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
		Port:           int(envInt64("PORT", 8080)),
		DatabaseURL:    strings.TrimSpace(os.Getenv("DATABASE_URL")),
		RedisURL:       strings.TrimSpace(os.Getenv("REDIS_URL")),
		MigrationsDir:  envStr("MIGRATIONS_DIR", "migrations"),
		JWTSecret:      []byte(strings.TrimSpace(os.Getenv("JWT_SECRET"))),
		AdminJWTKey:    []byte(envStr("ADMIN_JWT_SECRET", "vaultflow-dev-admin-secret")),
		SignerEndpoint: envStr("SIGNER_ADDR", "vault-signer:50051"),
		SignerID:       envStr("SIGNER_ID", ""),
		AnchorChainID:  envInt64("ANCHOR_CHAIN_ID", 0),
		MTLSCertFile:   envStr("MTLS_CLIENT_CERT", "certs/clients/gateway-core.crt"),
		MTLSKeyFile:    envStr("MTLS_CLIENT_KEY", "certs/clients/gateway-core.key"),
		MTLSCAFile:     envStr("MTLS_CA_CERT", "certs/ca/ca.crt"),
		MTLSServerName: envStr("MTLS_SERVER_NAME", "vault-signer"),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) == 0 {
		return nil, fmt.Errorf("config: JWT_SECRET is required (user token signing key)")
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
	}
	if cfg.Deploy.AnchorChainID != 0 && cfg.AnchorChainID == 0 {
		cfg.AnchorChainID = cfg.Deploy.AnchorChainID
	}
	return cfg, nil
}

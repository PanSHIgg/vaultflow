// config.go — cmd/indexer 环境变量装配配置 (stdlib only).
//
// 全部链级参数经单个结构化 JSON (CHAIN_CONFIG_JSON) 注入 — 与
// cmd/gateway/cmd/backend 同一 env schema (deploy-and-seed.sh 统一生成);
// 本进程只消费扫链所需字段, 其余字段宽容忽略 (json.Unmarshal 默认行为).
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

// chainRuntime 单链装配参数 (CHAIN_CONFIG_JSON.chains 元素).
// 未列出字段 (热钱包/费率/合约地址等) 由 encoding/json 自动忽略.
type chainRuntime struct {
	ChainID               types.ChainID `json:"chain_id"`
	RPCURLs               []string      `json:"rpc_urls"`
	RequiredConfirmations uint64        `json:"required_confirmations"`
	BatchSweeper          types.Address `json:"batch_sweeper"`
	StartHeight           *uint64       `json:"start_height"`
	// Factory/Implementation 可选覆盖 — 公链部署地址与全局 deploy 面不
	// 一致时逐链指定 (devnet 同 deployer 部署可留空回落全局值).
	Factory        types.Address `json:"factory"`
	Implementation types.Address `json:"implementation"`
	// DevnetTags 显式 opt-in: 仅 devnet/anvil 链置真时才包 DevnetTagAdapter
	// (将 safe=0 与 latest-safe>requiredConfs 的陈旧标签转译 NotFound).
	// 生产链必须缺省 false — 主网 safe 天然滞后, 误接会把合法标签全部
	// 打成 NotFound 并把安全线降级为 latest-requiredConfs.
	DevnetTags bool `json:"devnet_tags"`
}

// deploymentConfig CHAIN_CONFIG_JSON 根对象 — 合约部署面 + 每链参数.
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
	Port          int
	DatabaseURL   string
	MigrationsDir string // 保留 env 位 — migrations 由 gateway 进程执行, 本进程仅等 users 表就绪
	Deploy        deploymentConfig
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
		Port:          int(envInt64("PORT", 8082)),
		DatabaseURL:   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		MigrationsDir: envStr("MIGRATIONS_DIR", "migrations"),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	raw := strings.TrimSpace(os.Getenv("CHAIN_CONFIG_JSON"))
	if raw == "" {
		return nil, fmt.Errorf("config: CHAIN_CONFIG_JSON is required (written by deploy-and-seed.sh)")
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
	return cfg, nil
}

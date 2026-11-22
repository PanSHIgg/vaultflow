// config.go — cmd/backend 环境变量装配配置 (stdlib only).
//
// 业务核心 worker 进程: txmanager (nonce 分配/签名/广播/RBF/熔断) +
// 每链 sweeper (归集编排) + credit worker (FINALIZED -> CREDITED 用户入账).
// 链级参数经 CHAIN_CONFIG_JSON 注入 (与 gateway/indexer 同 schema,
// 未列出字段宽容忽略).
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
	ChainID               types.ChainID `json:"chain_id"`
	RPCURLs               []string      `json:"rpc_urls"`
	RequiredConfirmations uint64        `json:"required_confirmations"`
	BatchSweeper          types.Address `json:"batch_sweeper"`
	RelayerHotWallet      types.Address `json:"relayer_hot_wallet"`
	PayoutHotWallet       types.Address `json:"payout_hot_wallet"`
	MaxFeePerGas          string        `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas  string        `json:"max_priority_fee_per_gas"`
	SweepGasBase          uint64        `json:"sweep_gas_base"`
	SweepGasItem          uint64        `json:"sweep_gas_item"`
	PayoutGasLimit        uint64        `json:"payout_gas_limit"` // 缺省 150_000
	WithdrawalCooldownSecs int64        `json:"withdrawal_cooldown_secs"` // 缺省 60 (与 signer cooldown_secs 对齐)
	// DevnetTags 显式 opt-in: 仅 devnet/anvil 链置真时才包 DevnetTagAdapter.
	DevnetTags bool `json:"devnet_tags"`
}

// deploymentConfig CHAIN_CONFIG_JSON 根对象.
type deploymentConfig struct {
	Factory        types.Address            `json:"factory"` // 可选 — 本进程不推导 vault
	Implementation types.Address            `json:"implementation"`
	AnchorChainID  int64                    `json:"anchor_chain_id"`
	Chains         map[string]*chainRuntime `json:"chains"`
}

// appConfig 全量装配配置.
type appConfig struct {
	Port           int
	DatabaseURL    string
	RedisURL       string // 可空 — 当前未消费, 预留
	SignerEndpoint string
	SignerID       string // signer_audit_anchor 锚点标识 (空 → Endpoint)
	AnchorChainID  int64

	MTLSCertFile   string
	MTLSCAFile     string
	MTLSKeyFile    string
	MTLSServerName string // 证书 CN (compose 内 vault-signer)

	// AdminEd25519Priv 白名单租约离线签名私钥 (64B seed||pub hex).
	// 空 => 提现功能关闭 (充值/归集不受影响).
	AdminEd25519Priv string

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
		Port:           int(envInt64("PORT", 8081)),
		DatabaseURL:    strings.TrimSpace(os.Getenv("DATABASE_URL")),
		RedisURL:       strings.TrimSpace(os.Getenv("REDIS_URL")),
		SignerEndpoint: envStr("SIGNER_ADDR", "vault-signer:50051"),
		SignerID:       envStr("SIGNER_ID", ""),
		AnchorChainID:  envInt64("ANCHOR_CHAIN_ID", 0),
		MTLSCertFile:   envStr("MTLS_CLIENT_CERT", "certs/clients/gateway-core.crt"),
		MTLSKeyFile:    envStr("MTLS_CLIENT_KEY", "certs/clients/gateway-core.key"),
		MTLSCAFile:     envStr("MTLS_CA_CERT", "certs/ca/ca.crt"),
		MTLSServerName: envStr("MTLS_SERVER_NAME", "vault-signer"),
		AdminEd25519Priv: envStr("ADMIN_ED25519_PRIVATE_KEY", ""),
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

// mustU256 十进制字符串 -> types.U256; 空串取零值 (配置可选).
func mustU256(s string) types.U256 {
	if s == "" {
		return types.U256{}
	}
	return types.MustParseU256(s)
}

package ports

import "context"

// SignerClient vault-signer 双向 mTLS gRPC 客户端 (W4 signerclient 实现).
// DTO 与 proto/signer/v1/signer.proto 字段级对齐, 但本接口保持 proto-agnostic:
// 实现端负责原生结构 <-> protobuf 消息转换. 消费方: W4 txmanager (SignTx,
// 以及每 5 分钟 VerifyAuditChain 巡检协程), W5 api (白名单管理与健康展示).
type SignerClient interface {
	// SignTx 请求签名一笔出站 EIP-1559 交易. signer 独立解码 unsigned_tx 推导
	// 策略并执行四模防火墙校验; 返回二选一结果 — Signed 或 Rejected.
	// 传输层错误 (连接断开/mTLS 失败) 返回 ErrSignerUnavailable.
	SignTx(ctx context.Context, req SignTxRequest) (SignTxResponse, error)

	// RegisterWhitelist 注册商户提现白名单地址 (附 69B 租期载荷 + Ed25519 签名).
	RegisterWhitelist(ctx context.Context, req RegisterWhitelistRequest) (WhitelistOpResponse, error)

	// RevokeWhitelist 紧急吊销提现白名单地址 (管理员可经 mTLS 直连 signer,
	// 本接口为 gateway 转发通道; 全程落审计链).
	RevokeWhitelist(ctx context.Context, req RevokeWhitelistRequest) (WhitelistOpResponse, error)

	// VerifyAuditChain 从指定区间重算 SHA-256 审计哈希链
	// (TxManager 巡检协程每 5 分钟调用, 校验失败触发最高优先级安全告警).
	VerifyAuditChain(ctx context.Context, req VerifyAuditChainRequest) (VerifyAuditChainResponse, error)

	// Health 健康度与运行态巡检 (版本/时钟/白名单规模/审计链 tip/已加载热钱包).
	Health(ctx context.Context) (SignerHealth, error)
}

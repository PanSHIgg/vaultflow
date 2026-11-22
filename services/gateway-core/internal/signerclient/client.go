// Package signerclient implements W4-2 前半: vault-signer 双向 mTLS gRPC 客户端.
//
//   - 生产端经 NewClient 用离线根 CA 颁发的双向证书连接 (90d 轮换 → 每次 RPC
//     走标准 TLS 握手, 证书文件热更新由重启/重连策略承接);
//   - 每个 RPC 包装指数退避重试 (限 Unavailable/DeadlineExceeded 等瞬时错误码);
//   - 内置每 5 分钟 VerifyAuditChain 独立巡检协程 — 审计链校验失败触发
//     CRITICAL 级安全告警 (ARCHITECTURE §1.1: 监控进程定时校验哈希链完整性);
//   - 实现 ports.SignerClient, DTO 与 proto 字段级互转, ports 层保持 proto-agnostic.
package signerclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"vaultflow/gateway-core/internal/genproto/signerv1"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// Severity 告警级别; AlertCritical 触发最高优先级安全响应 (人工介入).
type Severity string

const (
	AlertInfo     Severity = "INFO"
	AlertWarning  Severity = "WARNING"
	AlertCritical Severity = "CRITICAL"
)

// Alert 结构化安全告警 (投递给运维通道/监控台).
type Alert struct {
	Severity Severity
	Code     string // 机器可读告警码
	Message  string
	Detail   string
	At       time.Time
}

// AlertFunc 告警投递回调 (注入: zap/webhook/metrics 适配由装配方实现).
type AlertFunc func(Alert)

// Config signerclient 连接与策略参数.
type Config struct {
	Endpoint   string // host:port
	CertFile   string // 客户端证书 (certs/clients/gateway-core.crt)
	KeyFile    string // 客户端私钥
	CAFile     string // 根 CA (certs/ca/ca.crt)
	ServerName string // 覆盖 TLS SNI (证书 CN 与 endpoint 不一致时使用, 可空)

	DialTimeout   time.Duration // 单 RPC 超时 (默认 10s)
	MaxAttempts   int           // 单 RPC 最大尝试次数 (默认 3)
	BackoffBase   time.Duration // 退避基数 (默认 200ms)
	BackoffMax    time.Duration // 退避上限 (默认 2s)
	AuditInterval time.Duration // 审计链巡检周期 (默认 5min, 测试可调小)

	// FIX-E/E7 巡检锚点持久化 (可空 — nil 时仅做无状态链校验,
	// 尾部截断/漂移检测能力随之关闭):
	AnchorDB       *pgxpool.Pool // signer_audit_anchor 表所在库 (005_audit_anchor.sql)
	AnchorSignerID string        // 锚点 signer 标识; 空时退化取 Endpoint
	AnchorChainID  int64         // 锚点链维度 (默认 0 = 未分链部署)
}

func (c *Config) withDefaults() {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 200 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 2 * time.Second
	}
	if c.AuditInterval <= 0 {
		c.AuditInterval = 5 * time.Minute
	}
}

// Client vault-signer mTLS gRPC 客户端, 实现 ports.SignerClient.
type Client struct {
	cfg    Config
	conn   *grpc.ClientConn
	api    signerv1.VaultSignerServiceClient
	log    *zap.Logger
	alertf AlertFunc

	mu      sync.RWMutex
	closed  bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	randSrc func() float64 // 测试注入抖动源; nil → 数学伪随机
}

// NewClient 建立 mTLS 连接并启动审计链巡检协程.
// 非阻塞拨号: 连接可用性由首个 RPC (WaitForReady 重试) 保证.
func NewClient(cfg Config, log *zap.Logger, alertf AlertFunc) (*Client, error) {
	cfg.withDefaults()
	creds, err := loadMTLS(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("signerclient: dial %s: %w", cfg.Endpoint, err)
	}
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{
		cfg:    cfg,
		conn:   conn,
		api:    signerv1.NewVaultSignerServiceClient(conn),
		log:    log,
		alertf: alertf,
	}
	c.startPatrol()
	return c, nil
}

// newWithConn 测试/装配注入: 以既有 ClientConnInterface 构造
// (bufconn/insecure 单测与 Wave-2 注入式装配复用).
func newWithConn(cc grpc.ClientConnInterface, cfg Config, log *zap.Logger, alertf AlertFunc) *Client {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{
		cfg:    cfg,
		api:    signerv1.NewVaultSignerServiceClient(cc),
		log:    log,
		alertf: alertf,
	}
	if conn, ok := cc.(*grpc.ClientConn); ok {
		c.conn = conn
	}
	c.startPatrol()
	return c
}

// NewInsecureForTesting 供跨包测试/装配用: 无 TLS 的本地 conn 注入
// (真实部署一律走 NewClient mTLS 路径).
func NewInsecureForTesting(cc grpc.ClientConnInterface, cfg Config, log *zap.Logger, alertf AlertFunc) *Client {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{
		cfg:    cfg,
		api:    signerv1.NewVaultSignerServiceClient(cc),
		log:    log,
		alertf: alertf,
	}
	if conn, ok := cc.(*grpc.ClientConn); ok {
		c.conn = conn
	}
	c.startPatrol()
	return c
}

// InsecureDialOption 导出供装配层选择 insecure 回退 (仅供测试环境显式开启).
func InsecureDialOption() grpc.DialOption {
	return grpc.WithTransportCredentials(insecure.NewCredentials())
}

func loadMTLS(cfg Config) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("signerclient: load client keypair: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("signerclient: read ca file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("signerclient: CA file contains no PEM certificates")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   cfg.ServerName,
		MinVersion:   tls.VersionTLS12,
	}), nil
}

// Close 停巡检并关闭连接.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// 通用重试: 仅对传输层/瞬时类 code 退避重试; 业务拒签不是 error 路径.
// ---------------------------------------------------------------------------

var retryableCodes = map[codes.Code]bool{
	codes.Unavailable:       true,
	codes.DeadlineExceeded:  true,
	codes.ResourceExhausted: true,
	codes.Aborted:           true,
	codes.Internal:          true,
}

func isRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return retryableCodes[status.Code(err)]
}

func (c *Client) backoff(attempt int) time.Duration {
	d := float64(c.cfg.BackoffBase) * math.Pow(2, float64(attempt))
	if d > float64(c.cfg.BackoffMax) {
		d = float64(c.cfg.BackoffMax)
	}
	jitter := 1.0
	if c.randSrc != nil {
		jitter = 0.75 + 0.5*c.randSrc()
	}
	return time.Duration(d * jitter)
}

// call 统一 RPC 执行器: 单次超时 + 指数退避重试.
// 传输层耗尽后返回 ErrSignerUnavailable (W4 错误契约).
func (c *Client) call(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %s: %v", ports.ErrSignerUnavailable, op, ctx.Err())
			case <-time.After(c.backoff(attempt - 1)):
			}
		}
		rctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
		lastErr = fn(rctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
		c.log.Warn("signer rpc transient failure, retrying",
			zap.String("op", op), zap.Int("attempt", attempt+1), zap.Error(lastErr))
	}
	return fmt.Errorf("%w: %s: %v", ports.ErrSignerUnavailable, op, lastErr)
}

// ---------------------------------------------------------------------------
// ports.SignerClient 实现
// ---------------------------------------------------------------------------

// SignTx 请求签名一笔 EIP-1559 出站交易.
func (c *Client) SignTx(ctx context.Context, req ports.SignTxRequest) (ports.SignTxResponse, error) {
	pb := &signerv1.SignTransactionRequest{
		ChainId:    req.ChainID,
		UnsignedTx: req.UnsignedTx,
		PolicyHint: signerv1.TxPolicy(req.PolicyHint),
		RetrySeq:   req.RetrySeq,
	}
	if req.PayoutUUID != uuid.Nil {
		pb.PayoutUuid = req.PayoutUUID[:]
	}
	if req.RefundTicket != nil {
		pb.RefundTicket = &signerv1.RefundTicket{
			SignedPayload:  req.RefundTicket.SignedPayload,
			AdminSignature: req.RefundTicket.AdminSignature,
		}
	}
	var resp *signerv1.SignTransactionResponse
	err := c.call(ctx, "SignTransaction", func(rctx context.Context) error {
		var e error
		resp, e = c.api.SignTransaction(rctx, pb)
		return e
	})
	if err != nil {
		return ports.SignTxResponse{}, err
	}
	out := ports.SignTxResponse{}
	switch r := resp.GetResult().(type) {
	case *signerv1.SignTransactionResponse_Signed:
		out.Signed = &ports.SignedTx{
			RawTx:    r.Signed.GetRawTx(),
			AuditSeq: r.Signed.GetAuditSeq(),
		}
		if len(r.Signed.GetTxHash()) == 32 {
			out.Signed.TxHash = types.HexToHash(hexencode(r.Signed.GetTxHash()))
		}
	case *signerv1.SignTransactionResponse_Rejected:
		out.Rejected = &ports.Rejection{
			Code:     ports.RejectCode(r.Rejected.GetCode()),
			Reason:   r.Rejected.GetReason(),
			AuditSeq: r.Rejected.GetAuditSeq(),
		}
	default:
		return ports.SignTxResponse{}, fmt.Errorf("signerclient: empty SignTransaction result")
	}
	return out, nil
}

// RegisterWhitelist 注册商户提现白名单地址 (69B 载荷 + Ed25519 签名).
func (c *Client) RegisterWhitelist(ctx context.Context, req ports.RegisterWhitelistRequest) (ports.WhitelistOpResponse, error) {
	var resp *signerv1.RegisterWithdrawalAddressResponse
	err := c.call(ctx, "RegisterWithdrawalAddress", func(rctx context.Context) error {
		var e error
		resp, e = c.api.RegisterWithdrawalAddress(rctx, &signerv1.RegisterWithdrawalAddressRequest{
			SignedPayload:  req.SignedPayload,
			AdminSignature: req.AdminSignature,
		})
		return e
	})
	if err != nil {
		return ports.WhitelistOpResponse{}, err
	}
	return ports.WhitelistOpResponse{
		Accepted:   resp.GetAccepted(),
		RejectCode: ports.RejectCode(resp.GetRejectCode()),
		Reason:     resp.GetReason(),
		AuditSeq:   resp.GetAuditSeq(),
	}, nil
}

// RevokeWhitelist 紧急吊销白名单地址.
func (c *Client) RevokeWhitelist(ctx context.Context, req ports.RevokeWhitelistRequest) (ports.WhitelistOpResponse, error) {
	var resp *signerv1.RevokeWithdrawalAddressResponse
	err := c.call(ctx, "RevokeWithdrawalAddress", func(rctx context.Context) error {
		var e error
		resp, e = c.api.RevokeWithdrawalAddress(rctx, &signerv1.RevokeWithdrawalAddressRequest{
			ChainId:          req.ChainID,
			MerchantUuid:     req.MerchantUUID[:],
			RecipientAddress: req.RecipientAddress.Bytes(),
			AdminNote:        req.AdminNote,
		})
		return e
	})
	if err != nil {
		return ports.WhitelistOpResponse{}, err
	}
	return ports.WhitelistOpResponse{
		Accepted:   resp.GetAccepted(),
		RejectCode: ports.RejectCode(resp.GetRejectCode()),
		Reason:     resp.GetReason(),
		AuditSeq:   resp.GetAuditSeq(),
	}, nil
}

// VerifyAuditChain 重算审计哈希链 (巡检协程/监控台).
func (c *Client) VerifyAuditChain(ctx context.Context, req ports.VerifyAuditChainRequest) (ports.VerifyAuditChainResponse, error) {
	var resp *signerv1.VerifyAuditChainResponse
	err := c.call(ctx, "VerifyAuditChain", func(rctx context.Context) error {
		var e error
		resp, e = c.api.VerifyAuditChain(rctx, &signerv1.VerifyAuditChainRequest{
			FromSeq: req.FromSeq,
			ToSeq:   req.ToSeq,
		})
		return e
	})
	if err != nil {
		return ports.VerifyAuditChainResponse{}, err
	}
	out := ports.VerifyAuditChainResponse{
		Valid:          resp.GetValid(),
		EntriesChecked: resp.GetEntriesChecked(),
		FirstBadSeq:    resp.GetFirstBadSeq(),
	}
	if th := resp.GetTipHash(); len(th) == 32 {
		out.TipHash = types.HexToHash(hexencode(th))
	}
	return out, nil
}

// GenerateWallet 请求 signer KMS 生成新平台钱包 (RELAYER/PAYOUT 等角色):
// 私钥落 signer 侧 encrypted_keys, 本端仅回收地址. gRPC 错误统一包装.
func (c *Client) GenerateWallet(ctx context.Context, chainID types.ChainID, role string) (types.Address, error) {
	var resp *signerv1.GenerateWalletResponse
	err := c.call(ctx, "GenerateWallet", func(rctx context.Context) error {
		var e error
		resp, e = c.api.GenerateWallet(rctx, &signerv1.GenerateWalletRequest{
			ChainId: uint64(chainID),
			Role:    role,
		})
		return e
	})
	if err != nil {
		return types.Address{}, fmt.Errorf("signerclient: GenerateWallet: %w", err)
	}
	addr := resp.GetAddress()
	if addr == "" {
		return types.Address{}, fmt.Errorf("signerclient: GenerateWallet returned empty address (audit_seq=%d)",
			resp.GetAuditSeq())
	}
	return types.HexToAddress(addr), nil
}

// ColdWalletsRejectedError UpdateColdWallets 被 signer 拒绝 (Accepted=false)
// 的强类型错误 — errors.As 可提取 RejectCode/Reason/AuditSeq 供运维决策.
type ColdWalletsRejectedError struct {
	Code     ports.RejectCode
	Reason   string
	AuditSeq uint64
}

func (e *ColdWalletsRejectedError) Error() string {
	return fmt.Sprintf("signerclient: UpdateColdWallets rejected: code=%d reason=%s audit_seq=%d",
		e.Code, e.Reason, e.AuditSeq)
}

// UpdateColdWallets 增量更新 signer 动态冷钱包白名单 (add/remove 为 raw 20B).
// Accepted=false 返回 *ColdWalletsRejectedError; 成功回传全量 ACTIVE 冷钱包.
func (c *Client) UpdateColdWallets(ctx context.Context, chainID types.ChainID,
	add, remove []types.Address) ([]types.Address, error) {

	req := &signerv1.UpdateColdWalletsRequest{ChainId: uint64(chainID)}
	for _, a := range add {
		req.Add = append(req.Add, a.Bytes())
	}
	for _, a := range remove {
		req.Remove = append(req.Remove, a.Bytes())
	}
	var resp *signerv1.UpdateColdWalletsResponse
	err := c.call(ctx, "UpdateColdWallets", func(rctx context.Context) error {
		var e error
		resp, e = c.api.UpdateColdWallets(rctx, req)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("signerclient: UpdateColdWallets: %w", err)
	}
	if !resp.GetAccepted() {
		return nil, &ColdWalletsRejectedError{
			Code:     ports.RejectCode(resp.GetRejectCode()),
			Reason:   resp.GetReason(),
			AuditSeq: resp.GetAuditSeq(),
		}
	}
	out := make([]types.Address, 0, len(resp.GetActiveColdWallets()))
	for _, w := range resp.GetActiveColdWallets() {
		out = append(out, types.HexToAddress(w))
	}
	return out, nil
}

// hexencode 0x 前缀小写 hex (与 §2.5.3 hex 编码规范一致).
func hexencode(b []byte) string {
	const hexdig = "0123456789abcdef"
	out := make([]byte, 2+len(b)*2)
	out[0], out[1] = '0', 'x'
	for i, c := range b {
		out[2+i*2] = hexdig[c>>4]
		out[3+i*2] = hexdig[c&0x0f]
	}
	return string(out)
}

// Health 运行态巡检 (版本/时钟/白名单规模/审计链 tip/已加载热钱包).
func (c *Client) Health(ctx context.Context) (ports.SignerHealth, error) {
	var resp *signerv1.GetSignerHealthResponse
	err := c.call(ctx, "GetSignerHealth", func(rctx context.Context) error {
		var e error
		resp, e = c.api.GetSignerHealth(rctx, &signerv1.GetSignerHealthRequest{})
		return e
	})
	if err != nil {
		return ports.SignerHealth{}, err
	}
	out := ports.SignerHealth{
		Version:          resp.GetVersion(),
		UptimeSeconds:    resp.GetUptimeSeconds(),
		SignerClock:      time.Unix(resp.GetSignerClockUnix(), 0).UTC(),
		WhitelistEntries: resp.GetWhitelistEntries(),
		AuditEntries:     resp.GetAuditEntries(),
	}
	if th := resp.GetAuditTipHash(); len(th) == 32 {
		out.AuditTipHash = types.HexToHash(hexencode(th))
	}
	for _, w := range resp.GetLoadedWallets() {
		out.LoadedWallets = append(out.LoadedWallets, types.HexToAddress(w))
	}
	return out, nil
}

var _ ports.SignerClient = (*Client)(nil)

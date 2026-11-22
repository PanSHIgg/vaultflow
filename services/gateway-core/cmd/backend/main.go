// main.go — cmd/backend: 交易所托管模型的业务核心 worker 进程.
//
// 装配序: env 解析 -> pgxpool -> 就绪等待 (pg + users 表) -> 每链 RPC 仲裁池
// + chainMux (多链 nonce/广播路由) -> mTLS vault-signer 客户端 (审计锚点
// 注入 AnchorDB/AnchorSignerID/AnchorChainID) -> txmanager (SetChainContracts
// + Start) -> 每链 sweeper -> credit worker (3s tick, FINALIZED -> CREDITED
// 原子入账) -> /healthz -> 优雅停机.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/signerclient"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/sweeper"
	"vaultflow/gateway-core/internal/txmanager"
	"vaultflow/gateway-core/internal/types"
	"vaultflow/gateway-core/internal/withdrawer"
)

// ---------------------------------------------------------------------------
// 多链路由 ChainProvider — txmanager.BroadcastQueued 不按 chain_id 过滤, 其
// 单 chain 依赖在生产端须把广播/回执/nonce 按链路由到对应仲裁池:
//   - SendRawTx: 解签名交易内嵌 EIP-155 chainId 选池 (逐字节可信 — 签名字段);
//   - Receipt/TxPresent: 轮询全部池 (同 txHash 只属于签名时锁定的那条链);
//   - Nonce: 热钱包地址 -> chain_id 映射 (装配期要求各链热钱包地址互异).
// ---------------------------------------------------------------------------
type chainMux struct {
	pools  map[types.ChainID]*chain.Pool
	wallet map[types.Address]types.ChainID
}

func (m *chainMux) SendRawTx(ctx context.Context, raw []byte) (types.Hash, error) {
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return types.Hash{}, fmt.Errorf("chainMux: decode signed tx: %w", err)
	}
	p, ok := m.pools[types.ChainID(tx.ChainId().Uint64())]
	if !ok {
		return types.Hash{}, fmt.Errorf("chainMux: no RPC pool for chain_id %s", tx.ChainId().String())
	}
	return p.SendRawTx(ctx, raw)
}

func (m *chainMux) TxPresent(ctx context.Context, hash types.Hash) (bool, bool, error) {
	found, mined := false, false
	var firstErr error
	for _, p := range m.pools {
		present, pending, err := p.TxPresent(ctx, hash)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if present {
			found = true
			if !pending {
				mined = true
			}
		}
	}
	if !found && firstErr != nil {
		return false, false, firstErr
	}
	return found, mined, nil
}

func (m *chainMux) Receipt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error) {
	var lastErr error
	for _, p := range m.pools {
		r, err := p.Receipt(ctx, hash)
		if err != nil {
			lastErr = err
			continue
		}
		if r != nil {
			return r, nil
		}
	}
	return nil, lastErr
}

func (m *chainMux) Nonce(ctx context.Context, wallet types.Address, pending bool) (uint64, error) {
	cid, ok := m.wallet[wallet]
	if !ok {
		return 0, fmt.Errorf("chainMux: wallet %s not mapped to a configured chain", wallet.Hex())
	}
	return m.pools[cid].Nonce(ctx, wallet, pending)
}

var _ txmanager.ChainProvider = (*chainMux)(nil)

// logAlert 各内部包自有 Alert 类型统一收敛进 zap 结构化日志.
func logAlert(log *zap.Logger, comp, sev, code, msg, detail string) {
	lvl := log.Warn
	if sev == "CRITICAL" {
		lvl = log.Error
	}
	lvl("security/ops alert",
		zap.String("component", comp), zap.String("severity", sev),
		zap.String("code", code), zap.String("message", msg), zap.String("detail", detail))
}

// waitReady 就绪等待: pg ping -> users 表探针 (gateway 迁移完成标志).
func waitReady(ctx context.Context, pool *pgxpool.Pool, log *zap.Logger) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := pool.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Fatal("postgres unreachable", zap.Error(err))
		}
		log.Warn("postgres not ready, retrying", zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	log.Info("postgres connected")

	for {
		var one int
		err := pool.QueryRow(ctx, `SELECT 1 FROM users LIMIT 1`).Scan(&one)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Fatal("schema not ready (users table unreadable — gateway migrations pending?)",
				zap.Error(err))
		}
		log.Info("waiting for schema (users table probe)", zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	log.Info("schema ready")
}

// creditWorker credit worker: 每 3s 扫 FINALIZED 且已解析 user_id 的充值,
// 逐笔 CreditDepositAtomically (FINALIZED->CREDITED + user_balances UPSERT,
// 幂等) — 与 indexer 解耦: 索引只管链上确认, 本 worker 只管余额入账.
func creditWorker(ctx context.Context, st *store.Store, chainID types.ChainID, log *zap.Logger) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		deps, err := st.ListFinalizedUncredited(ctx, chainID, 200)
		if err != nil {
			log.Warn("credit worker scan failed",
				zap.Uint64("chain_id", uint64(chainID)), zap.Error(err))
			continue
		}
		for _, d := range deps {
			if ctx.Err() != nil {
				return
			}
			if err := st.CreditDepositAtomically(ctx, d.ID); err != nil {
				log.Warn("credit deposit failed",
					zap.String("deposit", d.ID.String()),
					zap.Uint64("chain_id", uint64(chainID)), zap.Error(err))
				continue
			}
			log.Info("deposit credited",
				zap.String("deposit", d.ID.String()),
				zap.String("user", d.UserID.UUID.String()),
				zap.String("amount", d.Amount.String()),
				zap.String("asset", d.AssetSymbol),
				zap.Uint64("chain_id", uint64(chainID)))
		}
	}
}

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zap init:", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	cfg, err := loadAppConfig()
	if err != nil {
		log.Fatal("config load failed", zap.Error(err))
	}
	log.Info("config loaded",
		zap.Int("port", cfg.Port), zap.String("signer", cfg.SignerEndpoint),
		zap.Int("chains", len(cfg.Deploy.Chains)))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- PostgreSQL ----
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("pgxpool create failed", zap.Error(err))
	}
	defer pool.Close()
	waitReady(ctx, pool, log)

	// ---- 每链 RPC 仲裁池 + 热钱包->链映射 (nonce 路由) ----
	pools := make(map[types.ChainID]*chain.Pool, len(cfg.Deploy.Chains))
	walletChain := make(map[types.Address]types.ChainID)
	for _, cc := range cfg.Deploy.Chains {
		nodes := make([]chain.Node, 0, len(cc.RPCURLs))
		for i, url := range cc.RPCURLs {
			cli, err := chain.DialEthNode(ctx, url)
			if err != nil {
				log.Fatal("dial RPC node failed",
					zap.Uint64("chain_id", uint64(cc.ChainID)), zap.String("url", url), zap.Error(err))
			}
			nodeCli := chain.NodeClient(cli)
			if cc.DevnetTags {
				nodeCli = chain.NewDevnetTagAdapter(cli, cc.RequiredConfirmations)
			}
			nodes = append(nodes, chain.Node{
				Name: fmt.Sprintf("chain%d-node%d", cc.ChainID, i),
				Cli:  nodeCli,
			})
		}
		p := chain.NewPool(nodes, 1, chain.WithRequiredConfirmations(cc.RequiredConfirmations))
		pools[cc.ChainID] = p
		for _, w := range []types.Address{cc.RelayerHotWallet, cc.PayoutHotWallet} {
			if w == (types.Address{}) {
				continue
			}
			if prev, dup := walletChain[w]; dup && prev != cc.ChainID {
				log.Fatal("hot wallet shared across chains breaks nonce routing",
					zap.String("wallet", w.Hex()),
					zap.Uint64("chain_a", uint64(prev)), zap.Uint64("chain_b", uint64(cc.ChainID)))
			}
			walletChain[w] = cc.ChainID
		}
	}
	log.Info("rpc pools ready", zap.Int("chains", len(pools)))

	// ---- mTLS vault-signer 客户端 (审计锚点显式注入) ----
	signerAlertf := func(a signerclient.Alert) {
		logAlert(log, "signerclient", string(a.Severity), a.Code, a.Message, a.Detail)
	}
	signerClient, err := signerclient.NewClient(signerclient.Config{
		Endpoint:       cfg.SignerEndpoint,
		CertFile:       cfg.MTLSCertFile,
		KeyFile:        cfg.MTLSKeyFile,
		CAFile:         cfg.MTLSCAFile,
		ServerName:     cfg.MTLSServerName,
		AnchorDB:       pool, // 巡检锚点持久化 (005_audit_anchor.sql)
		AnchorSignerID: cfg.SignerID,
		AnchorChainID:  cfg.AnchorChainID,
	}, log, signerAlertf)
	if err != nil {
		log.Fatal("signer mTLS client init failed", zap.Error(err))
	}
	defer func() { _ = signerClient.Close() }()
	log.Info("signer mTLS client started (audit patrol active)",
		zap.String("endpoint", cfg.SignerEndpoint), zap.Int64("anchor_chain_id", cfg.AnchorChainID))

	// ---- txmanager ----
	txmAlertf := func(a txmanager.Alert) {
		logAlert(log, "txmanager", a.Severity, a.Code, a.Message, a.Detail)
	}
	txm := txmanager.New(pool, &chainMux{pools: pools, wallet: walletChain}, signerClient,
		txmanager.Config{}, log, txmAlertf)
	contracts := make(map[types.ChainID]txmanager.ChainContracts, len(cfg.Deploy.Chains))
	for _, cc := range cfg.Deploy.Chains {
		contracts[cc.ChainID] = txmanager.ChainContracts{
			BatchSweeper: cc.BatchSweeper,
			Factory:      cfg.Deploy.Factory,
			GasBase:      cc.SweepGasBase,
			GasItem:      cc.SweepGasItem,
		}
	}
	txm.SetChainContracts(contracts)
	txm.Start(ctx)
	log.Info("txmanager started")

	// ---- 每链 sweeper + credit worker + withdrawer ----
	st := store.NewStore(pool)
	var adminPriv ed25519.PrivateKey
	if cfg.AdminEd25519Priv != "" {
		raw, err := hex.DecodeString(strings.TrimPrefix(cfg.AdminEd25519Priv, "0x"))
		if err != nil || len(raw) != ed25519.SeedSize {
			log.Fatal("ADMIN_ED25519_PRIVATE_KEY must be a 64-hex (32B seed) ed25519 key")
		}
		adminPriv = ed25519.NewKeyFromSeed(raw)
	}
	sweepers := make([]*sweeper.Sweeper, 0, len(cfg.Deploy.Chains))
	for _, cc := range cfg.Deploy.Chains {
		swAlertf := func(a sweeper.Alert) {
			logAlert(log, "sweeper", a.Severity, a.Code, a.Message, a.Detail)
		}
		sw := sweeper.New(pool, txm, pools[cc.ChainID], st, sweeper.Config{
			ChainID:        cc.ChainID,
			BatchSweeper:   cc.BatchSweeper,
			RelayerHot:     cc.RelayerHotWallet,
			PayoutHot:      cc.PayoutHotWallet,
			MaxFeePerGas:   mustU256(cc.MaxFeePerGas),
			MaxPriorityFee: mustU256(cc.MaxPriorityFeePerGas),
			GasBase:        cc.SweepGasBase,
			GasItem:        cc.SweepGasItem,
		}, log, swAlertf)
		sw.Start(ctx)
		sweepers = append(sweepers, sw)
		log.Info("sweeper started", zap.Uint64("chain_id", uint64(cc.ChainID)))

		cid := cc.ChainID
		go creditWorker(ctx, st, cid, log)
		log.Info("credit worker started", zap.Uint64("chain_id", uint64(cid)))

		wd := withdrawer.New(pool, txm, signerClient, st, withdrawer.Config{
			ChainID:           cc.ChainID,
			PayoutHot:         cc.PayoutHotWallet,
			PayoutGasLimit:    cc.PayoutGasLimit,
			CooldownSecs:      cc.WithdrawalCooldownSecs,
			MaxFeePerGas:      mustU256(cc.MaxFeePerGas),
			MaxPriorityFee:    mustU256(cc.MaxPriorityFeePerGas),
			AdminEd25519Priv:  adminPriv,
		}, log)
		if wd.Enabled() {
			wd.Start(ctx)
			log.Info("withdrawer started", zap.Uint64("chain_id", uint64(cc.ChainID)))
		} else {
			log.Info("withdrawer disabled (no admin ed25519 key)",
				zap.Uint64("chain_id", uint64(cc.ChainID)))
		}
	}

	// ---- /healthz ----
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		hctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		status := "ok"
		code := http.StatusOK
		pgErr := pool.Ping(hctx)
		if pgErr != nil {
			status = "degraded"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    status,
			"postgres":  pgErr == nil,
			"chains":    len(pools),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("backend HTTP listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server failed", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received, draining")

	shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Warn("http shutdown error", zap.Error(err))
	}
	for _, sw := range sweepers {
		sw.Stop()
	}
	txm.Stop()
	log.Info("all workers stopped — bye")
}

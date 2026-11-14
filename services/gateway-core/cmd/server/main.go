// main.go — Wave-2 window I1: gateway-core 真实装配入口.
//
// 装配序: env 解析 -> pgxpool -> 迁移 -> create2 推导器 + initCodeHash
// 链上强断言 -> 每链 RPC 仲裁池 -> mTLS vault-signer 客户端 (审计锚点
// 显式注入 AnchorDB/AnchorSignerID/AnchorChainID) -> ledger -> order ->
// txmanager (多链路由 ChainProvider + SetChainContracts) -> SSE broker ->
// 每链 indexer/sweeper -> webhook outbox -> payout dispatcher -> Redis
// 频控/幂等 (断连降级) -> api.Server -> /healthz + 优雅停机.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/api"
	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/indexer"
	"vaultflow/gateway-core/internal/ledger"
	"vaultflow/gateway-core/internal/order"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/signerclient"
	"vaultflow/gateway-core/internal/sim"
	"vaultflow/gateway-core/internal/sse"
	"vaultflow/gateway-core/internal/sweeper"
	"vaultflow/gateway-core/internal/txmanager"
	"vaultflow/gateway-core/internal/types"
	"vaultflow/gateway-core/internal/webhook"
)

// ---------------------------------------------------------------------------
// 多链路由 ChainProvider — txmanager.BroadcastQueued 不按 chain_id 过滤, 其
// 单 chain 依赖在生产端须把广播/回执/nonce 按链路由到对应仲裁池:
//   - SendRawTx: 解签名交易内嵌 EIP-155 chainId 选池 (逐字节可信 — 签名字段);
//   - Receipt/TxPresent: 轮询全部池 (同 txHash 只属于签名时锁定的那条链);
//   - Nonce: 热钱包地址 -> chain_id 映射 (装配期要求各链热钱包地址互异).
//
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

// factoryHashQuerier create2.FactoryInitCodeQuerier — 经仲裁池 eth_call
// VaultFactory.initCodeHash().
type factoryHashQuerier struct {
	p *chain.Pool
}

var initCodeHashSel = crypto.Keccak256([]byte("initCodeHash()"))[:4]

func (q factoryHashQuerier) InitCodeHash(ctx context.Context, factory types.Address) (types.Hash, error) {
	out, err := q.p.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: initCodeHashSel}, nil)
	if err != nil {
		return types.Hash{}, err
	}
	if len(out) != 32 {
		return types.Hash{}, fmt.Errorf("initCodeHash() returned %d bytes (factory=%s)", len(out), factory.Hex())
	}
	return common.BytesToHash(out), nil
}

// ---------------------------------------------------------------------------
// 告警适配 — 各内部包自有 Alert 类型, 统一收敛进 zap 结构化日志.
// ---------------------------------------------------------------------------
func logAlert(log *zap.Logger, comp, sev, code, msg, detail string) {
	lvl := log.Warn
	if sev == "CRITICAL" {
		lvl = log.Error
	}
	lvl("security/ops alert",
		zap.String("component", comp), zap.String("severity", sev),
		zap.String("code", code), zap.String("message", msg), zap.String("detail", detail))
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
		zap.Int("chains", len(cfg.Deploy.Chains)), zap.Bool("sim", cfg.SimEnabled))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- PostgreSQL ----
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("pgxpool create failed", zap.Error(err))
	}
	defer pool.Close()
	// 就绪等待 (compose healthcheck 之外的本机/dockerless 启动兜底).
	deadline := time.Now().Add(60 * time.Second)
	for {
		err = pool.Ping(ctx)
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

	// ---- 迁移 (schema_migrations 跟踪 + lock_timeout=15s) ----
	applied, err := runMigrations(ctx, pool, cfg.MigrationsDir, log)
	if err != nil {
		log.Fatal("migrations failed", zap.Error(err))
	}
	log.Info("migrations done", zap.Ints("newly_applied", applied))

	// ---- CREATE2 推导器 + initCodeHash 强断言 ----
	deriver := create2.NewDeriver(cfg.Deploy.Factory, cfg.Deploy.Implementation)
	localHash := deriver.InitCodeHash()
	if cfg.Deploy.InitCodeHash != (types.Hash{}) && cfg.Deploy.InitCodeHash != localHash {
		log.Fatal("init_code_hash mismatch vs deployment config",
			zap.String("expected", cfg.Deploy.InitCodeHash.Hex()),
			zap.String("computed", localHash.Hex()))
	}
	log.Info("create2 deriver initialised",
		zap.String("factory", cfg.Deploy.Factory.Hex()),
		zap.String("implementation", cfg.Deploy.Implementation.Hex()),
		zap.String("init_code_hash", localHash.Hex()))

	// ---- 每链 RPC 仲裁池 ----
	pools := make(map[types.ChainID]*chain.Pool, len(cfg.Deploy.Chains))
	walletChain := make(map[types.Address]types.ChainID)
	var firstChain types.ChainID
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
				// FIX-G BUG-4: devnet 标签适配 — safe=0 与陈旧 safe
				// (latest-safe > requiredConfs) 均转译 NotFound, 触发
				// Pool 的 latest-requiredConfs 回退. 仅 devnet_tags
				// opt-in 链接线; 生产链缺省不包 (主网 safe 天然滞后,
				// 误转译会永久降级安全线为 latest-requiredConfs).
				nodeCli = chain.NewDevnetTagAdapter(cli, cc.RequiredConfirmations)
			}
			nodes = append(nodes, chain.Node{
				Name: fmt.Sprintf("chain%d-node%d", cc.ChainID, i),
				Cli:  nodeCli,
			})
		}
		// 单/双节点 dev 部署: quorum=1 (生产由多节点 + 多数仲裁).
		p := chain.NewPool(nodes, 1, chain.WithRequiredConfirmations(cc.RequiredConfirmations))
		pools[cc.ChainID] = p
		if firstChain == 0 {
			firstChain = cc.ChainID
		}
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
	// 链上强断言: 每条配置了 factory 的链, VaultFactory.initCodeHash() 必须与
	// 本地实现推导逐字节相等 (部署参数错位即拒绝启动).
	for cid, p := range pools {
		if err := deriver.VerifyChainFactoryHash(ctx, factoryHashQuerier{p: p}); err != nil {
			log.Fatal("on-chain initCodeHash verification failed",
				zap.Uint64("chain_id", uint64(cid)), zap.Error(err))
		}
		log.Info("on-chain initCodeHash verified", zap.Uint64("chain_id", uint64(cid)))
	}
	log.Info("rpc pools ready", zap.Int("chains", len(pools)))

	// ---- Redis (可空降级) ----
	var rdb redis.UniversalClient
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			log.Warn("REDIS_URL unparsable; in-process fallbacks engaged", zap.Error(err))
		} else {
			rdb = redis.NewClient(opt)
			defer func() { _ = rdb.Close() }()
		}
	} else {
		log.Warn("REDIS_URL unset — rate limiter / idempotency / SSE run in-process")
	}

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
		AnchorDB:       pool,         // I1 必配: 巡检锚点持久化 (005_audit_anchor.sql)
		AnchorSignerID: cfg.SignerID, // 空 → 退化取 Endpoint
		AnchorChainID:  cfg.AnchorChainID,
	}, log, signerAlertf)
	if err != nil {
		log.Fatal("signer mTLS client init failed", zap.Error(err))
	}
	defer func() { _ = signerClient.Close() }()
	log.Info("signer mTLS client started (audit patrol active)",
		zap.String("endpoint", cfg.SignerEndpoint), zap.Int64("anchor_chain_id", cfg.AnchorChainID))

	// ---- 业务服务 ----
	ledgerSvc := ledger.NewService(pool)
	orderSvc := order.NewService(pool, deriver, ledgerSvc)

	txmAlertf := func(a txmanager.Alert) {
		logAlert(log, "txmanager", a.Severity, a.Code, a.Message, a.Detail)
	}
	txm := txmanager.New(pool, &chainMux{pools: pools, wallet: walletChain}, signerClient,
		txmanager.Config{}, log, txmAlertf)
	contracts := make(map[types.ChainID]txmanager.ChainContracts, len(cfg.Deploy.Chains))
	for _, cc := range cfg.Deploy.Chains {
		contracts[cc.ChainID] = txmanager.ChainContracts{
			BatchSweeper:  cc.BatchSweeper,
			EIP3009Router: cc.EIP3009Router,
			Factory:       cfg.Deploy.Factory,
		}
	}
	txm.SetChainContracts(contracts)
	txm.Start(ctx)
	log.Info("txmanager started")

	// ---- SSE broker ----
	broker := sse.NewBroker(rdb, log)
	defer broker.Close()
	// FIX-G BUG-2: 订单域人工/周期跃迁事件汇 (indexer 侧走构造参数).
	orderSvc.SetOrderEventSink(broker)

	// ---- 每链 indexer + sweeper ----
	indexers := make([]*indexer.Indexer, 0, len(cfg.Deploy.Chains))
	sweepers := make([]*sweeper.Sweeper, 0, len(cfg.Deploy.Chains))
	for _, cc := range cfg.Deploy.Chains {
		idxAlertf := func(a indexer.Alert) {
			logAlert(log, "indexer", a.Severity, a.Code, a.Message, a.Detail)
		}
		ix := indexer.New(pool, pools[cc.ChainID], ledgerSvc, broker, txm, indexer.Config{
			ChainID:               cc.ChainID,
			PayoutHotWallet:       cc.PayoutHotWallet,
			RelayerHotWallet:      cc.RelayerHotWallet,
			ColdWallets:           cc.ColdWallets,
			BatchSweeper:          cc.BatchSweeper,
			StartHeight:           cc.StartHeight,
			RequiredConfirmations: cc.RequiredConfirmations,
		}, log, idxAlertf)
		ix.Start(ctx)
		indexers = append(indexers, ix)
		log.Info("indexer started", zap.Uint64("chain_id", uint64(cc.ChainID)))

		swAlertf := func(a sweeper.Alert) {
			logAlert(log, "sweeper", a.Severity, a.Code, a.Message, a.Detail)
		}
		var cold types.Address
		if len(cc.ColdWallets) > 0 {
			cold = cc.ColdWallets[0]
		}
		sw := sweeper.New(pool, txm, pools[cc.ChainID], ledgerSvc, sweeper.Config{
			ChainID:        cc.ChainID,
			BatchSweeper:   cc.BatchSweeper,
			RelayerHot:     cc.RelayerHotWallet,
			PayoutHot:      cc.PayoutHotWallet,
			ColdWallet:     cold,
			MaxFeePerGas:   mustU256(cc.MaxFeePerGas),
			MaxPriorityFee: mustU256(cc.MaxPriorityFeePerGas),
		}, log, swAlertf)
		sw.Start(ctx)
		sweepers = append(sweepers, sw)
		log.Info("sweeper started", zap.Uint64("chain_id", uint64(cc.ChainID)))
	}

	// ---- webhook outbox worker ----
	whAlertf := func(a webhook.Alert) {
		logAlert(log, "webhook", a.Severity, a.Code, a.Message, a.Detail)
	}
	wh := webhook.New(pool, &http.Client{Timeout: 10 * time.Second},
		webhook.Config{AllowInsecureHTTP: cfg.WebhookInsecureHTTP}, log, whAlertf)
	wh.Start(ctx)
	log.Info("webhook outbox worker started")

	// ---- payout dispatcher ----
	pdc := order.PayoutDispatcherConfig{Chains: map[types.ChainID]order.PayoutChainConfig{}}
	for _, cc := range cfg.Deploy.Chains {
		pdc.Chains[cc.ChainID] = order.PayoutChainConfig{
			PayoutWallet:         cc.PayoutHotWallet,
			GasLimit:             cc.PayoutGasLimit,
			MaxFeePerGas:         mustU256(cc.MaxFeePerGas),
			MaxPriorityFeePerGas: mustU256(cc.MaxPriorityFeePerGas),
		}
	}
	dispatcher := order.NewPayoutDispatcher(orderSvc, txm, pdc, log)
	dispatcher.Start(ctx)
	log.Info("payout dispatcher started")

	// ---- 过期订单周期重评器 (§5.2 周期重评边) ----
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := orderSvc.ReevaluateExpiredOrders(ctx, time.Now(), 500); err != nil {
					log.Warn("order expiry reevaluation failed", zap.Error(err))
				}
			}
		}
	}()
	log.Info("order expiry reevaluator started")

	// ---- 可选 dev 演练面 (ENABLE_DEV_SIMULATOR) ----
	// 注意: 必须经接口类型声明, 关闭态保持 nil 接口而非 typed-nil 指针
	// (否则 deps.Sim == nil 判定失效).
	var simSvc ports.SimulatorService
	var anvilRouter api.AnvilPassthrough
	if cfg.SimEnabled {
		clients := map[types.ChainID]*sim.AnvilClient{}
		targets := map[types.ChainID]sim.DrillTarget{}
		for _, cc := range cfg.Deploy.Chains {
			clients[cc.ChainID] = sim.NewAnvilClient(cc.RPCURLs[0], nil)
			var cold types.Address
			if len(cc.ColdWallets) > 0 {
				cold = cc.ColdWallets[0]
			}
			targets[cc.ChainID] = sim.DrillTarget{
				RelayerWallet:   cc.RelayerHotWallet,
				PayoutWallet:    cc.PayoutHotWallet,
				SweeperContract: cc.BatchSweeper,
				RelayContract:   cc.EIP3009Router,
				ColdWallet:      cold,
				FundingAccount:  cc.FundingAccount,
				MaxFeePerGas:    mustU256(cc.MaxFeePerGas).Big(),
				MaxPriorityFee:  mustU256(cc.MaxPriorityFeePerGas).Big(),
			}
		}
		router := sim.NewAnvilRouter(clients, firstChain)
		anvilRouter = router
		simSvc = sim.NewService(sim.Deps{
			Router: router, Signer: signerClient, TxMgr: txm,
			Ledger: ledgerSvc, Logger: log, Targets: targets,
		})
		log.Info("dev simulator enabled", zap.Int("chains", len(clients)))
	}

	// ---- API server ----
	apiCfg := api.DefaultConfig()
	apiCfg.SimEnabled = cfg.SimEnabled // /v1/sim/** 门禁 — 缺省 false 会误关已装配的演练面
	apiCfg.Chains = map[types.ChainID]api.ChainConfig{}
	for _, cc := range cfg.Deploy.Chains {
		apiCfg.Chains[cc.ChainID] = api.ChainConfig{
			ChainID:               cc.ChainID,
			RequiredConfirmations: cc.RequiredConfirmations,
			EIP3009Router:         cc.EIP3009Router,
			RelayerHotWallet:      cc.RelayerHotWallet,
			PayoutHotWallet:       cc.PayoutHotWallet,
			RelayGasLimit:         cc.RelayGasLimit,
			MaxFeePerGas:          mustU256(cc.MaxFeePerGas),
			MaxPriorityFeePerGas:  mustU256(cc.MaxPriorityFeePerGas),
		}
	}
	var idemStore api.IdempotencyStore
	if rdb != nil {
		idemStore = api.NewDegradingIdempotencyStore(api.NewRedisIdempotencyStore(rdb), log)
	} else {
		idemStore = api.NewMemoryIdempotencyStore()
	}
	apiSrv := api.NewServer(api.Deps{
		Config:      apiCfg,
		Logger:      log,
		Orders:      orderSvc,
		Ledger:      ledgerSvc,
		TxManager:   txm,
		Signer:      signerClient,
		Sim:         simSvc,
		Streamer:    broker,
		Store:       &pgStore{pool: pool},
		IdemStore:   idemStore,
		Limiter:     api.NewRateLimiter(rdb, log),
		AdminJWTKey: cfg.AdminJWTKey,
		Anvil:       anvilRouter,
	})

	// ---- HTTP mux: /healthz (compose 健康检查) + 业务面 ----
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
	mux.Handle("/", apiSrv.Handler())

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("gateway HTTP listening", zap.String("addr", srv.Addr))
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
	for _, ix := range indexers {
		ix.Stop()
	}
	for _, sw := range sweepers {
		sw.Stop()
	}
	dispatcher.Stop()
	wh.Stop()
	txm.Stop()
	log.Info("all workers stopped — bye")
}

// mustU256 十进制字符串 -> types.U256; 空串取零值 (配置可选).
func mustU256(s string) types.U256 {
	if s == "" {
		return types.U256{}
	}
	return types.MustParseU256(s)
}

// 契约锚点: 装配实际类型必须满足冻结 ports.
var (
	_ ports.LedgerService    = (*ledger.Service)(nil)
	_ ports.SignerClient     = (*signerclient.Client)(nil)
	_ ports.TxManager        = (*txmanager.Manager)(nil)
	_ ports.SSEBroker        = (*sse.Broker)(nil)
	_ ports.SimulatorService = (*sim.Service)(nil)
)

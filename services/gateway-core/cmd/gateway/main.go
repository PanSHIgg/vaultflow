// main.go — gateway 进程装配入口 (交易所托管模型的对外 REST API 面).
//
// 装配序: env 解析 -> pgxpool -> 迁移 -> create2 推导器 + initCodeHash 自检
// (纯本地比对, gateway 不持有 RPC 池, 链上强断言由 backend/indexer 进程负责)
// -> store -> exchange 业务门面 -> Redis (可空降级限流) -> mTLS vault-signer
// 客户端 (审计锚点显式注入 AnchorDB/AnchorSignerID/AnchorChainID) ->
// api.Server -> /healthz + 优雅停机.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/api"
	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/exchange"
	"vaultflow/gateway-core/internal/signerclient"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// 告警适配 — signerclient.Alert 收敛进 zap 结构化日志.
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
		zap.Int("chains", len(cfg.Deploy.Chains)))

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

	// ---- CREATE2 推导器 + initCodeHash 本地自检 ----
	// gateway 无 RPC 池, 仅做配置一致性断言 (链上 initCodeHash() 强校验由
	// 持 RPC 的 backend/indexer 进程承担). 每链可经 chains.<id>.factory/
	// implementation 覆盖全局部署面 (公链异址部署).
	globalDeriver := create2.NewDeriver(cfg.Deploy.Factory, cfg.Deploy.Implementation)
	localHash := globalDeriver.InitCodeHash()
	if cfg.Deploy.InitCodeHash != (types.Hash{}) && cfg.Deploy.InitCodeHash != localHash {
		log.Fatal("init_code_hash mismatch vs deployment config",
			zap.String("expected", cfg.Deploy.InitCodeHash.Hex()),
			zap.String("computed", localHash.Hex()))
	}
	log.Info("create2 deriver initialised",
		zap.String("factory", cfg.Deploy.Factory.Hex()),
		zap.String("implementation", cfg.Deploy.Implementation.Hex()),
		zap.String("init_code_hash", localHash.Hex()))
	derivers := make(map[types.ChainID]*create2.Deriver, len(cfg.Deploy.Chains))
	for _, cc := range cfg.Deploy.Chains {
		d := cfg.Deploy.Deriver(cc.ChainID)
		if d == nil {
			log.Fatal("no CREATE2 deriver resolvable for chain",
				zap.Uint64("chain_id", uint64(cc.ChainID)))
		}
		derivers[cc.ChainID] = d
		log.Info("chain deriver ready",
			zap.Uint64("chain_id", uint64(cc.ChainID)),
			zap.String("factory", d.Factory().Hex()),
			zap.String("init_code_hash", d.InitCodeHash().Hex()))
	}

	// ---- 业务门面 ----
	st := store.NewStore(pool)
	exchangeSvc := exchange.NewService(st, derivers)

	// ---- Redis (可空降级) ----
	var rdb redis.UniversalClient
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			log.Warn("REDIS_URL unparsable; in-process rate limiter engaged", zap.Error(err))
		} else {
			rdb = redis.NewClient(opt)
			defer func() { _ = rdb.Close() }()
		}
	} else {
		log.Warn("REDIS_URL unset — rate limiter runs in-process")
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
		AnchorDB:       pool,         // 巡检锚点持久化 (005_audit_anchor.sql)
		AnchorSignerID: cfg.SignerID, // 空 → 退化取 Endpoint
		AnchorChainID:  cfg.AnchorChainID,
	}, log, signerAlertf)
	if err != nil {
		log.Fatal("signer mTLS client init failed", zap.Error(err))
	}
	defer func() { _ = signerClient.Close() }()
	log.Info("signer mTLS client started (audit patrol active)",
		zap.String("endpoint", cfg.SignerEndpoint), zap.Int64("anchor_chain_id", cfg.AnchorChainID))

	// ---- API server ----
	apiCfg := api.DefaultConfig()
	apiCfg.Chains = map[types.ChainID]api.ChainConfig{}
	for _, cc := range cfg.Deploy.Chains {
		apiCfg.Chains[cc.ChainID] = api.ChainConfig{
			ChainID:               cc.ChainID,
			RequiredConfirmations: cc.RequiredConfirmations,
		}
	}
	apiSrv := api.NewServer(api.Deps{
		Config:      apiCfg,
		Logger:      log,
		Exchange:    exchangeSvc,
		Store:       st,
		Signer:      signerClient,
		Limiter:     api.NewRateLimiter(rdb, log),
		UserJWTKey:  cfg.JWTSecret,
		AdminJWTKey: cfg.AdminJWTKey,
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
			"chains":    len(cfg.Deploy.Chains),
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
	log.Info("gateway stopped — bye")
}

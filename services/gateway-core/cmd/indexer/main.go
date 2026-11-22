// main.go — cmd/indexer: 交易所托管模型的独立扫链进程.
//
// 装配序: env 解析 -> pgxpool -> 就绪等待 (pg + users 表, migrations 由
// gateway 进程负责) -> create2 推导器 + initCodeHash 链上强断言 ->
// 每链 RPC 仲裁池 (devnet_tags opt-in 包 DevnetTagAdapter) ->
// store.NewStore -> 每链 indexer.New + Start -> /healthz -> 优雅停机.
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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/indexer"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

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

	// schema 就绪: gateway 拥有迁移 — 本进程仅等待 users 表出现
	// (SELECT 1 FROM users LIMIT 1; 表不存在/为空均重试至 60s).
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
		zap.Int("port", cfg.Port), zap.Int("chains", len(cfg.Deploy.Chains)))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- PostgreSQL ----
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("pgxpool create failed", zap.Error(err))
	}
	defer pool.Close()
	waitReady(ctx, pool, log)

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
		// 单/双节点 dev 部署: quorum=1 (生产由多节点 + 多数仲裁).
		p := chain.NewPool(nodes, 1, chain.WithRequiredConfirmations(cc.RequiredConfirmations))
		pools[cc.ChainID] = p
	}
	// 链上强断言: 每条配置了 factory 的链, VaultFactory.initCodeHash() 必须与
	// 本地实现推导逐字节相等 (部署参数错位即拒绝启动). 每链可用
	// chains.<id>.factory/implementation 覆盖全局部署面 (公链异址部署).
	for cid, p := range pools {
		d := cfg.Deploy.Deriver(cid)
		if d == nil {
			log.Fatal("no CREATE2 deriver resolvable for chain",
				zap.Uint64("chain_id", uint64(cid)))
		}
		if err := d.VerifyChainFactoryHash(ctx, factoryHashQuerier{p: p}); err != nil {
			log.Fatal("on-chain initCodeHash verification failed",
				zap.Uint64("chain_id", uint64(cid)), zap.Error(err))
		}
		log.Info("on-chain initCodeHash verified", zap.Uint64("chain_id", uint64(cid)))
	}
	log.Info("rpc pools ready", zap.Int("chains", len(pools)))

	// ---- 每链 indexer ----
	st := store.NewStore(pool)
	indexers := make([]*indexer.Indexer, 0, len(cfg.Deploy.Chains))
	for _, cc := range cfg.Deploy.Chains {
		idxAlertf := func(a indexer.Alert) {
			logAlert(log, "indexer", a.Severity, a.Code, a.Message, a.Detail)
		}
		ix := indexer.New(pool, pools[cc.ChainID], st, indexer.Config{
			ChainID:               cc.ChainID,
			BatchSweeper:          cc.BatchSweeper,
			StartHeight:           cc.StartHeight,
			RequiredConfirmations: cc.RequiredConfirmations,
		}, log, idxAlertf)
		ix.Start(ctx)
		indexers = append(indexers, ix)
		log.Info("indexer started", zap.Uint64("chain_id", uint64(cc.ChainID)))
	}

	// ---- /healthz (compose 健康检查): pg + 每链同步游标 ----
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
		cursors := map[string]map[string]any{}
		rows, qerr := pool.Query(hctx,
			`SELECT chain_id, latest_synced_height, safe_head_height, latest_synced_hash
			   FROM chain_sync_cursors ORDER BY chain_id`)
		if qerr == nil {
			for rows.Next() {
				var cid, lh, sh int64
				var h string
				if err := rows.Scan(&cid, &lh, &sh, &h); err == nil {
					cursors[fmt.Sprintf("%d", cid)] = map[string]any{
						"latest_synced_height": lh,
						"safe_head_height":     sh,
						"latest_synced_hash":   h,
					}
				}
			}
			rows.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    status,
			"postgres":  pgErr == nil,
			"chains":    cursors,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("indexer HTTP listening", zap.String("addr", srv.Addr))
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
	log.Info("all indexers stopped — bye")
}

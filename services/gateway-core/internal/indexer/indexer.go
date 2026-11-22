// Package indexer implements 交易所托管模型的单链单写者同步与 LIFO 逆序回滚引擎.
//
//   - 每 chain_id 协程持 pg_try_advisory_lock(chain_id) 单写者串行推进;
//   - 入站分类: Transfer.to_address 命中 user_deposit_addresses ->
//     USER_DEPOSIT (回填 user_id); 命中 ACTIVE platform_wallets ->
//     COLD=TREASURY_REBALANCE / RELAYER,PAYOUT=TOPUP; 否则 UNMATCHED;
//   - 两段式确认: DETECTED -> (block_number <= safe_head) FINALIZED;
//     用户余额贷记由 backend credit worker 独立完成 (解耦);
//   - 出站两段式: BROADCASTED -> INCLUDED_UNSAFE -> FINALIZED, 联动
//     sweep_batches/items 与 user_deposit_addresses 部署/隔离标记;
//   - 统一 LIFO 逆序回滚: DETECTED->ORPHANED_SHALLOW / FINALIZED->
//     ORPHANED_DEEP / CREDITED->DebitUserBalanceOnOrphan 余额冲正.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// ChainQuerier indexer 对链能力的最小依赖 (*chain.Pool 生产实现, fake 单测).
type ChainQuerier interface {
	// Heights 仲裁 (latest,safe,finalized); minSynced 用于滞后节点剔除.
	Heights(ctx context.Context, minSynced uint64) (chain.Heights, error)
	// HeaderAt 多数哈希仲裁的区块头.
	HeaderAt(ctx context.Context, height uint64) (*chain.BlockHeader, error)
	// TransferLogs 按 canonical 头钉死拉取 ERC-20 Transfer 日志 (FIX-A/A1:
	// EIP-234 blockHash 参数优先; 节点不支持时区间拉取 + 严格
	// ev.BlockHash == hdr.Hash 过滤, 保证只返回 canonical 块日志).
	TransferLogs(ctx context.Context, hdr *chain.BlockHeader, tokens, watch []types.Address,
		maxFilterAddrs int) ([]chain.TransferEvent, error)
	// ReceiptAt 多数仲裁回执; 未找到返回 (nil, nil).
	ReceiptAt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error)
}

// Alert 运维/安全告警.
type Alert struct {
	Severity string // INFO / WARNING / CRITICAL
	Code     string
	Message  string
	Detail   string
	At       time.Time
}

// AlertFunc 告警投递回调.
type AlertFunc func(Alert)

// Config 单链同步参数.
type Config struct {
	ChainID        types.ChainID
	PollInterval   time.Duration // 同步 tick (默认 2s)
	BatchBlocks    int           // 单 tick 最多前推块数 (默认 64)
	MaxFilterAddrs int           // indexed_filter_max_addrs (<=0 视为无上限必走服务端过滤)
	MaxForkDepth   int           // 回滚步进上限 (默认 256, 超出报 CRITICAL 异常)

	BatchSweeper types.Address // 归集事件 emitter 校验 (零地址则跳过校验)
	// StartHeight 首启游标高度; nil → 从当前 safe 头开始 (不回放历史).
	StartHeight *uint64
	// RequiredConfirmations 链确认阈值 (观测/告警载荷; 链配置同源注入).
	RequiredConfirmations uint64
}

func (c *Config) withDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.BatchBlocks <= 0 {
		c.BatchBlocks = 64
	}
	if c.MaxForkDepth <= 0 {
		c.MaxForkDepth = 256
	}
}

// Indexer 单链单写者同步器.
type Indexer struct {
	db     *pgxpool.Pool
	chain  ChainQuerier
	st     *store.Store // 用户域门面: 地址归属/平台钱包/隔离标记/余额冲正
	alertf AlertFunc
	cfg    Config
	log    *zap.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
	now    func() time.Time // 测试注入时钟
}

// New 构造单链 Indexer.
//
// 平台钱包 (RELAYER/PAYOUT/COLD) 不再走静态配置 — 每个同步块经
// store.ListPlatformWallets 刷新 ACTIVE 集合, admin CRUD 无重启生效.
func New(db *pgxpool.Pool, ch ChainQuerier, st *store.Store,
	cfg Config, log *zap.Logger, alertf AlertFunc) *Indexer {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	return &Indexer{
		db: db, chain: ch, st: st,
		alertf: alertf, cfg: cfg, log: log, now: time.Now,
	}
}

func (ix *Indexer) raise(a Alert) {
	a.At = ix.now()
	if ix.alertf != nil {
		ix.alertf(a)
	}
	lvl := ix.log.Warn
	if a.Severity == "CRITICAL" {
		lvl = ix.log.Error
	}
	lvl("indexer alert",
		zap.String("severity", a.Severity), zap.String("code", a.Code),
		zap.String("message", a.Message), zap.String("detail", a.Detail))
}

// Start 获取 advisory lock 后进入同步循环 (幂等).
// 锁不可获取时每 PollInterval 重试 — 其他实例持锁期间本协程静默待命.
func (ix *Indexer) Start(ctx context.Context) {
	ix.wg.Add(1)
	go func() {
		defer ix.wg.Done()
		for {
			err := ix.runLocked(ctx)
			if ctx.Err() != nil {
				return
			}
			ix.log.Warn("indexer loop exited, re-entering after poll interval",
				zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(ix.cfg.PollInterval):
			}
		}
	}()
}

// Stop 停止同步循环 (advisory lock 随连接释放).
func (ix *Indexer) Stop() {
	if ix.cancel != nil {
		ix.cancel()
	}
	ix.wg.Wait()
}

// runLocked 持锁执行同步主循环; 锁占用失败重试至 ctx 取消.
func (ix *Indexer) runLocked(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	ix.cancel = cancel

	conn, err := ix.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("indexer: acquire lock conn: %w", err)
	}
	defer conn.Release()

	// pg_try_advisory_lock(key bigint) — 单链单写者不变量.
	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`,
			int64(ix.cfg.ChainID)).Scan(&got); err != nil {
			return fmt.Errorf("indexer: advisory lock: %w", err)
		}
		if got {
			break
		}
		ix.log.Info("advisory lock held by peer, standing by",
			zap.Uint64("chain", uint64(ix.cfg.ChainID)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ix.cfg.PollInterval):
		}
	}
	defer func() {
		// 会话级锁随 Release 归还; 显式 unlock 双保险.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(ix.cfg.ChainID))
	}()

	ix.log.Info("indexer single-writer lock acquired", zap.Uint64("chain", uint64(ix.cfg.ChainID)))

	t := time.NewTicker(ix.cfg.PollInterval)
	defer t.Stop()
	for {
		if err := ix.SyncOnce(ctx); err != nil {
			ix.log.Warn("sync tick failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// 同步主流程: 游标 -> 仲裁头 -> 前推 (哈希链校验 + LIFO) -> 两段式结算 -> 游标
// ---------------------------------------------------------------------------

// SyncOnce 执行一轮同步 (导出供测试直接驱动; 须在持锁协程内或独占测试环境调用).
func (ix *Indexer) SyncOnce(ctx context.Context) error {
	cur, err := ix.loadCursor(ctx)
	if err != nil {
		return err
	}
	heads, err := ix.chain.Heights(ctx, cur.LatestSyncedHeight)
	if err != nil {
		return err
	}

	upper := heads.Latest
	if bound := cur.LatestSyncedHeight + uint64(ix.cfg.BatchBlocks); upper > bound {
		upper = bound
	}
	for h := cur.LatestSyncedHeight + 1; h <= upper; h++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := ix.chain.HeaderAt(ctx, h)
		if err != nil {
			return err
		}
		if hdr.ParentHash != cur.LatestSyncedHash {
			// 哈希链断裂 → 找分叉点并执行 LIFO 逆序回滚.
			fork, err := ix.findForkPoint(ctx, cur)
			if err != nil {
				return err
			}
			if err := ix.rollbackToFork(ctx, cur, fork); err != nil {
				return err
			}
			cur, err = ix.loadCursor(ctx)
			if err != nil {
				return err
			}
			h = cur.LatestSyncedHeight // 重新评估同一高度 (for 循环自增)
			continue
		}
		if err := ix.processBlock(ctx, hdr); err != nil {
			return err
		}
		cur.LatestSyncedHeight = h
		cur.LatestSyncedHash = hdr.Hash
	}

	// 出站入块检测: BROADCASTED -> INCLUDED_UNSAFE.
	if err := ix.detectOutboundInclusions(ctx); err != nil {
		return err
	}

	// 两段式结算: 入站 FINALIZED + 出站 FINALIZED (安全高度以下).
	if heads.Safe > cur.SafeHeadHeight {
		cur.SafeHeadHeight = heads.Safe
	}
	if err := ix.settleDeposits(ctx, heads.Safe); err != nil {
		return err
	}
	if err := ix.settleAttempts(ctx, heads.Safe); err != nil {
		return err
	}
	return ix.updateCursor(ctx, cur)
}

// loadCursor 读同步游标; 无行时按 StartHeight 或 safe 头初始化.
func (ix *Indexer) loadCursor(ctx context.Context) (types.ChainSyncCursor, error) {
	var cur types.ChainSyncCursor
	var hash string
	err := ix.db.QueryRow(ctx,
		`SELECT latest_synced_height, latest_synced_hash, safe_head_height
		   FROM chain_sync_cursors WHERE chain_id=$1`, int64(ix.cfg.ChainID)).
		Scan(&cur.LatestSyncedHeight, &hash, &cur.SafeHeadHeight)
	if err == nil {
		cur.ChainID = ix.cfg.ChainID
		cur.LatestSyncedHash = types.HexToHash(hash)
		return cur, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return cur, err
	}
	// 首启初始化.
	var start uint64
	var startHash types.Hash
	if ix.cfg.StartHeight != nil {
		start = *ix.cfg.StartHeight
		if start > 0 {
			hdr, err := ix.chain.HeaderAt(ctx, start)
			if err != nil {
				return cur, fmt.Errorf("indexer: seed start height %d: %w", start, err)
			}
			startHash = hdr.Hash
		}
	} else {
		heads, err := ix.chain.Heights(ctx, 0)
		if err != nil {
			return cur, err
		}
		start = heads.Safe
		if start > 0 {
			hdr, err := ix.chain.HeaderAt(ctx, start)
			if err != nil {
				return cur, fmt.Errorf("indexer: seed safe head %d: %w", start, err)
			}
			startHash = hdr.Hash
		}
	}
	cur = types.ChainSyncCursor{
		ChainID: ix.cfg.ChainID, LatestSyncedHeight: start,
		LatestSyncedHash: startHash, SafeHeadHeight: start,
	}
	if _, err := ix.db.Exec(ctx,
		`INSERT INTO chain_sync_cursors (chain_id, latest_synced_height, latest_synced_hash, safe_head_height)
		 VALUES ($1,$2,$3,$4) ON CONFLICT (chain_id) DO NOTHING`,
		int64(ix.cfg.ChainID), int64(start), hashStr(startHash), int64(start)); err != nil {
		return cur, err
	}
	// 锚定块入 chain_blocks — 否则 findForkPoint 在首块以下找不到任何本地
	// 已知哈希, 游标锚点本身无法作为分叉公共祖先.
	if start > 0 {
		hdr, err := ix.chain.HeaderAt(ctx, start)
		if err != nil {
			return cur, fmt.Errorf("indexer: seed anchor block %d: %w", start, err)
		}
		if _, err := ix.db.Exec(ctx,
			`INSERT INTO chain_blocks (chain_id, block_hash, block_number, parent_hash, status)
			 VALUES ($1,$2,$3,$4,'CANONICAL') ON CONFLICT (chain_id, block_hash) DO NOTHING`,
			int64(ix.cfg.ChainID), hashStr(hdr.Hash), int64(start),
			hashStr(hdr.ParentHash)); err != nil {
			return cur, err
		}
	}
	return cur, nil
}

// updateCursor 持久化游标推进 (latest/hash/safe 单调).
func (ix *Indexer) updateCursor(ctx context.Context, cur types.ChainSyncCursor) error {
	_, err := ix.db.Exec(ctx,
		`UPDATE chain_sync_cursors
		   SET latest_synced_height=$2, latest_synced_hash=$3,
		       safe_head_height=GREATEST(safe_head_height,$4), updated_at=now()
		 WHERE chain_id=$1`,
		int64(ix.cfg.ChainID), int64(cur.LatestSyncedHeight),
		hashStr(cur.LatestSyncedHash), int64(cur.SafeHeadHeight))
	return err
}

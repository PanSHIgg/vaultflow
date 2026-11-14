// Package indexer implements W4-3: 单链单写者双向同步与 LIFO 逆序回滚引擎.
//
//   - 每 chain_id 协程持 pg_try_advisory_lock(chain_id) 单写者串行推进;
//   - §4.1 四分支入站分类记账 (单 PG 事务内 FINALIZED 跃迁 + 记账 +
//     webhook_outbox; 注: 账本 PostEntry 经 ports.LedgerService 走自身事务,
//     幂等 reference_id 保证"先记账后落状态"的可重放顺序 — 见交付报告契约说明);
//   - §4.2 出站两段式确认 (INCLUDED_UNSAFE -> 过 safe_head FINALIZED +
//     必记 GAS_SETTLE + 成功联动 SWEEP_SETTLE / PAYOUT_FLOW);
//   - §4.3 入站出站统一 LIFO 逆序回滚 (ORPHANED / BROADCASTED 重置 /
//     REORG_REVERSAL 红冲 / SUPERSEDED 兄弟复活 / 幂等槽重广播).
package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
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

// Rebroadcaster LIFO 回滚后重广播通道 (*txmanager.Manager 实现).
type Rebroadcaster interface {
	RebroadcastIntent(ctx context.Context, intentID uuid.UUID) error
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
	// 热钱包与金库地址 (§4.1 分支 A 识别 + watch 集合常驻成员).
	PayoutHotWallet  types.Address
	RelayerHotWallet types.Address
	ColdWallet       types.Address // 单值兼容字段 (A5)
	// ColdWallets A5: 冷金库集合 — 与 ColdWallet 取并集判等, 支持多冷钱包
	// 及 48h Timelock 轮换期"旧值 + PendingColdWallet"并存 (轮换期间把
	// 新旧地址一并放入本集合即可平滑过渡).
	ColdWallets []types.Address

	BatchSweeper types.Address // 归集事件 emitter 校验 (零地址则跳过校验)
	// StartHeight 首启游标高度; nil → 从当前 safe 头开始 (不回放历史).
	StartHeight *uint64
	// RequiredConfirmations 链确认阈值 (FIX-G BUG-2): SSE
	// confirmation_progress 事件的 required_confirmations 载荷;
	// 与链配置 required_confirmations 同源 (装配期注入).
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
	db      *pgxpool.Pool
	chain   ChainQuerier
	ledger  ports.LedgerService
	sse     ports.SSEBroker // 可 nil (降级仅落 outbox)
	rebc    Rebroadcaster   // 可 nil (回滚后不主动重广播, 靠 TxManager 巡检自愈)
	alertf  AlertFunc
	cfg     Config
	log     *zap.Logger
	coldSet map[types.Address]struct{} // A5: ColdWallet ∪ ColdWallets 判等集合

	cancel context.CancelFunc
	wg     sync.WaitGroup
	now    func() time.Time // 测试注入时钟
}

// New 构造单链 Indexer.
func New(db *pgxpool.Pool, ch ChainQuerier, ledger ports.LedgerService, sse ports.SSEBroker,
	rebc Rebroadcaster, cfg Config, log *zap.Logger, alertf AlertFunc) *Indexer {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	coldSet := make(map[types.Address]struct{}, len(cfg.ColdWallets)+1)
	if cfg.ColdWallet != (types.Address{}) {
		coldSet[cfg.ColdWallet] = struct{}{}
	}
	for _, w := range cfg.ColdWallets {
		coldSet[w] = struct{}{}
	}
	return &Indexer{
		db: db, chain: ch, ledger: ledger, sse: sse, rebc: rebc,
		alertf: alertf, cfg: cfg, log: log, coldSet: coldSet, now: time.Now,
	}
}

// isColdWallet A5: from ∈ 冷金库集合判定 (单值 + 多值/轮换集合并集).
func (ix *Indexer) isColdWallet(a types.Address) bool {
	_, ok := ix.coldSet[a]
	return ok
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

	// pg_try_advisory_lock(key bigint) — 单链单写者不变量 (§4.1.1).
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

	startSynced := cur.LatestSyncedHeight
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
			// 哈希链断裂 → §4.3: 找分叉点并执行 LIFO 逆序回滚.
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
	// FIX-G BUG-2: SSE 确认进度 — 本轮有新块入库时, 对仍在 DETECTED 的
	// 订单充值发 confirmation_progress (提交后 advisory 面; 查询失败仅
	// 告警不阻断游标推进).
	if cur.LatestSyncedHeight > startSynced {
		ix.emitConfirmationProgress(ctx, heads)
	}
	return ix.updateCursor(ctx, cur)
}

// publishOrderEvent 提交后 SSE 广播: broker 未注入 (nil) 时 no-op;
// broker.PublishOrderEvent 自身已吞 Redis 降级错误, 仍兜底记日志防
// 接口语义演进. 仅在业务事务 COMMIT 之后调用 — after-hook 机制保证.
func (ix *Indexer) publishOrderEvent(ctx context.Context, ev ports.OrderEvent) {
	if ix.sse == nil {
		return
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = ix.now()
	}
	if err := ix.sse.PublishOrderEvent(ctx, ev); err != nil {
		ix.log.Warn("sse order event publish failed",
			zap.String("order", ev.OrderID.String()),
			zap.String("type", string(ev.Type)), zap.Error(err))
	}
}

// emitConfirmationProgress 对仍 DETECTED 的订单关联充值逐笔广播
// CONFIRMATION_UPDATE — 收银台确认进度条数据源 (FIX-G BUG-2).
// confirmations = latest 视图中该笔的区块确认深度 (latest-block+1, 下限 0),
// required_confirmations 取链配置阈值; deposit 明细由 api 层 ReadStore 补全.
func (ix *Indexer) emitConfirmationProgress(ctx context.Context, heads chain.Heights) {
	if ix.sse == nil {
		return
	}
	rows, err := ix.db.Query(ctx,
		`SELECT d.id, d.block_number, d.matched_order_id
		   FROM onchain_deposits d
		  WHERE d.chain_id=$1 AND d.status='DETECTED' AND d.matched_order_id IS NOT NULL
		  ORDER BY d.block_number, d.tx_hash, d.log_index`,
		int64(ix.cfg.ChainID))
	if err != nil {
		ix.log.Warn("sse confirmation scan failed", zap.Error(err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var depID uuid.UUID
		var blk int64
		var oid uuid.UUID
		if err := rows.Scan(&depID, &blk, &oid); err != nil {
			ix.log.Warn("sse confirmation scan row", zap.Error(err))
			return
		}
		var conf uint64
		if heads.Latest+1 > uint64(blk) {
			conf = heads.Latest + 1 - uint64(blk)
		}
		ix.publishOrderEvent(ctx, ports.OrderEvent{
			OrderID: oid, Type: ports.OrderEventConfirmationUpdate,
			Confirmations:         conf,
			RequiredConfirmations: ix.cfg.RequiredConfirmations,
			OccurredAt:            ix.now(),
		})
	}
	if err := rows.Err(); err != nil {
		ix.log.Warn("sse confirmation scan drain", zap.Error(err))
	}
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

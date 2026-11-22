// Package sweeper implements 定额批量归集编排与热钱包水位监控 (交易所托管模型).
//
//   - 周期性聚合各白名单资产下"已 FINALIZED/CREDITED 且未隔离"的
//     user_deposit_addresses vault 精确余额 (Σ 入站 − 已 SWEPT − 在途
//     PENDING 占用), 组装 <=50 条目的 sweep_batches + sweep_batch_items,
//     经 TxManager 广播 BatchSweeper.deployAndSweepBatch;
//   - 冷金库目标每 sweep tick 动态取 store.GetActiveColdWallets 首个 ACTIVE
//     — admin 平台钱包 CRUD 无重启生效; 无 ACTIVE 冷金库时 CRITICAL 告警
//     并跳过本轮 (不误建批);
//   - 批次级 VaultSwept/VaultQuarantined 事件解析与状态落库由 indexer
//     两段式确认完成 — 本包只负责聚合建批与水位监控;
//   - 水位监控: payout 热钱包各稳定币备付额 + 双热钱包原生 ETH,
//     低于阈值触发 WARNING 告警.
package sweeper

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// MaxBatchItems 合约层 MAX_BATCH_ITEMS=50 强上限.
const MaxBatchItems = 50

// sweepGasCap 合约 SWEEP_GAS_CAP=120000 (单条目归集 gas 上界).
const sweepGasCap = 120_000

// Alert 水位/编排告警.
type Alert struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	At       time.Time
}

// AlertFunc 告警回调.
type AlertFunc func(Alert)

// WaterQuerier 水位监控所需链上余额查询 (Pool 生产实现).
type WaterQuerier interface {
	// ERC20BalanceOf eth_call balanceOf(owner).
	ERC20BalanceOf(ctx context.Context, token, owner types.Address) (*big.Int, error)
	// NativeBalance eth_getBalance.
	NativeBalance(ctx context.Context, owner types.Address) (*big.Int, error)
}

// Config 归集编排参数.
// 冷金库不再是静态字段 — 每 sweep tick 经 store.GetActiveColdWallets 动态
// 解析 (platform_wallets admin CRUD 热生效).
type Config struct {
	ChainID        types.ChainID
	PollInterval   time.Duration // 聚合 tick (默认 30s)
	WaterInterval  time.Duration // 水位 tick (默认 60s)
	MaxPerTick     int           // 单 tick 最多建批数 (默认 4)
	BatchSweeper   types.Address // BatchSweeper 合约 (必配)
	RelayerHot     types.Address // 归集专用热钱包 (nonce 归属 + 合约白名单调用方)
	PayoutHot      types.Address // 提现热钱包 (水位对象)
	GasBase        uint64        // 批次基础 gas (默认 80_000)
	GasItem        uint64        // 每条目 gas (默认 sweepGasCap=120_000; 公网 CREATE2 部署成本更高, 可覆盖)
	MaxFeePerGas   types.U256    // 归集 EIP-1559 费率
	MaxPriorityFee types.U256
	// PendingBatchTimeout PENDING 批次 (QueueIntent 曾失败, 尚无关联意图)
	// 重试窗口上限 — 超时标 FAILED + items RELEASED (B2 reaper; 默认 10min).
	PendingBatchTimeout time.Duration
	// MaxQueueAttempts PENDING 批次重试入队次数上限 (默认 20).
	MaxQueueAttempts int
	// LowWaterToken 各资产 payout 热钱包低水位阈值 (symbol->wei 原子单位).
	LowWaterToken map[string]*big.Int
	// LowWaterNative 单热钱包原生 ETH 低水位 (nil 跳过).
	LowWaterNative *big.Int
}

func (c *Config) withDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.WaterInterval <= 0 {
		c.WaterInterval = 60 * time.Second
	}
	if c.MaxPerTick <= 0 {
		c.MaxPerTick = 4
	}
	if c.GasBase == 0 {
		c.GasBase = 80_000
	}
	if c.GasItem == 0 {
		c.GasItem = sweepGasCap
	}
	if c.PendingBatchTimeout <= 0 {
		c.PendingBatchTimeout = 10 * time.Minute
	}
	if c.MaxQueueAttempts <= 0 {
		c.MaxQueueAttempts = 20
	}
}

// Sweeper 归集编排器.
type Sweeper struct {
	db     *pgxpool.Pool
	txm    ports.TxManager
	chain  WaterQuerier // 可 nil — 跳过链上水位 (仅建批)
	st     *store.Store // 冷金库动态解析 (platform_wallets ACTIVE COLD)
	cfg    Config
	log    *zap.Logger
	alertf AlertFunc

	cancel context.CancelFunc
	wg     sync.WaitGroup
	now    func() time.Time
}

// New 构造 Sweeper.
func New(db *pgxpool.Pool, txm ports.TxManager, wq WaterQuerier,
	st *store.Store, cfg Config, log *zap.Logger, alertf AlertFunc) *Sweeper {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	return &Sweeper{db: db, txm: txm, chain: wq, st: st,
		cfg: cfg, log: log, alertf: alertf, now: time.Now}
}

func (s *Sweeper) raise(a Alert) {
	a.At = s.now()
	if s.alertf != nil {
		s.alertf(a)
	}
	lvl := s.log.Warn
	if a.Severity == "CRITICAL" {
		lvl = s.log.Error
	}
	lvl("sweeper alert", zap.String("severity", a.Severity),
		zap.String("code", a.Code), zap.String("message", a.Message),
		zap.String("detail", a.Detail))
}

// Start 后台协程: 聚合归集 + 水位监控.
func (s *Sweeper) Start(ctx context.Context) {
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.cfg.PollInterval)
		defer t.Stop()
		for {
			if err := s.SweepOnce(ctx); err != nil {
				s.log.Warn("sweep tick failed", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.cfg.WaterInterval)
		defer t.Stop()
		for {
			if err := s.WaterOnce(ctx); err != nil {
				s.log.Warn("water tick failed", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Stop 停止后台协程.
func (s *Sweeper) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// ---------------------------------------------------------------------------
// 精确余额聚合 + 批次创建
// ---------------------------------------------------------------------------

// vaultBalance 单 vault 的待归集精确余额.
type vaultBalance struct {
	daID    uuid.UUID
	address types.Address
	salt    types.Hash
	amount  *big.Int
}

// aggregateUnswept 计算 asset 下全部非隔离 vault 的精确未归集余额:
//
//	Σ FINALIZED+CREDITED+SWEPT 入站(to=vault — 历史全部已确认入站)
//	− Σ SWEPT actual_swept_amount − Σ 存活批次 PENDING exact_sweep_amount.
//
// SWEPT 充值必须计入入站: 其资金虽已转出, 但出口由 item actual 扣减 —
// 若入站排除 SWEPT, 同一笔会被双重扣减, vault 永久残留历史已扫总额.
func (s *Sweeper) aggregateUnswept(ctx context.Context, asset, tokenContract string) ([]vaultBalance, error) {
	rows, err := s.db.Query(ctx,
		`SELECT uda.id, uda.address, uda.create2_salt,
		        COALESCE(SUM(d.amount),0)::text
		   FROM user_deposit_addresses uda
		   LEFT JOIN onchain_deposits d
		     ON d.chain_id=uda.chain_id AND d.to_address=uda.address
		    AND d.token_contract=$2 AND d.status IN ('FINALIZED','CREDITED','SWEPT')
		  WHERE uda.chain_id=$1 AND uda.is_quarantined=FALSE
		  GROUP BY uda.id, uda.address, uda.create2_salt`,
		int64(s.cfg.ChainID), tokenContract)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	totals := map[uuid.UUID]*vaultBalance{}
	var order []uuid.UUID
	for rows.Next() {
		var vb vaultBalance
		var addr, amt string
		var salt []byte
		if err := rows.Scan(&vb.daID, &addr, &salt, &amt); err != nil {
			return nil, err
		}
		vb.address = types.HexToAddress(addr)
		copy(vb.salt[:], salt)
		vb.amount = types.MustParseU256(amt).Big()
		totals[vb.daID] = &vb
		order = append(order, vb.daID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 扣减已归集与在途占用 (B2: PENDING 条目仅在存活批次下才计占用 —
	// FAILED/COMPLETED/PARTIAL_QUARANTINED 批次遗留 PENDING 视为已释放,
	// reconcileBatches 会将其落 RELEASED 审计态).
	rows2, err := s.db.Query(ctx,
		`SELECT i.deposit_address_id,
		        COALESCE(SUM(i.actual_swept_amount) FILTER (WHERE i.item_status='SWEPT'),0)::text,
		        COALESCE(SUM(i.exact_sweep_amount)  FILTER (
		            WHERE i.item_status='PENDING' AND b.status IN ('PENDING','IN_FLIGHT')),0)::text
		   FROM sweep_batch_items i JOIN sweep_batches b ON b.id=i.batch_id
		  WHERE b.chain_id=$1 AND b.asset_symbol=$2
		  GROUP BY i.deposit_address_id`,
		int64(s.cfg.ChainID), asset)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var daID uuid.UUID
		var swept, pending string
		if err := rows2.Scan(&daID, &swept, &pending); err != nil {
			return nil, err
		}
		if vb, ok := totals[daID]; ok {
			vb.amount.Sub(vb.amount, types.MustParseU256(swept).Big())
			vb.amount.Sub(vb.amount, types.MustParseU256(pending).Big())
		}
	}
	var out []vaultBalance
	for _, id := range order {
		if totals[id].amount.Sign() > 0 {
			out = append(out, *totals[id])
		}
	}
	return out, rows2.Err()
}

// SweepOnce 执行一轮聚合建批 (导出供测试直接驱动).
// 先跑 reconcileBatches 释放/重派遗留批次, 再动态解析本链 ACTIVE 冷金库
// (platform_wallets CRUD 热生效) — 无 ACTIVE 冷金库时 CRITICAL 告警并
// 跳过本轮 (不建批, 不失败退出, 下轮自动重试).
func (s *Sweeper) SweepOnce(ctx context.Context) error {
	if err := s.reconcileBatches(ctx); err != nil {
		s.log.Warn("sweep batch reconcile failed", zap.Error(err))
	}
	colds, err := s.st.GetActiveColdWallets(ctx, s.cfg.ChainID)
	if err != nil {
		return fmt.Errorf("resolve active cold wallets: %w", err)
	}
	if len(colds) == 0 {
		s.raise(Alert{Severity: "CRITICAL", Code: "NO_ACTIVE_COLD_WALLET",
			Message: "no ACTIVE cold wallet configured for chain — sweep skipped this tick",
			Detail:  fmt.Sprintf("chain=%d", s.cfg.ChainID)})
		return nil
	}
	cold := colds[0].Address // 首个 ACTIVE (创建序); 轮换期并存时取最早
	assets, err := s.assets(ctx)
	if err != nil {
		return err
	}
	made := 0
	for _, a := range assets {
		if made >= s.cfg.MaxPerTick {
			break
		}
		vbs, err := s.aggregateUnswept(ctx, a.symbol, a.contract)
		if err != nil {
			return err
		}
		for len(vbs) > 0 && made < s.cfg.MaxPerTick {
			n := len(vbs)
			if n > MaxBatchItems {
				n = MaxBatchItems
			}
			if err := s.createBatch(ctx, a, vbs[:n], cold); err != nil {
				return fmt.Errorf("create sweep batch %s: %w", a.symbol, err)
			}
			vbs = vbs[n:]
			made++
		}
	}
	return nil
}

type assetRow struct {
	symbol, contract string
}

func (s *Sweeper) assets(ctx context.Context) ([]assetRow, error) {
	rows, err := s.db.Query(ctx,
		`SELECT asset_symbol, token_contract FROM supported_assets
		  WHERE chain_id=$1 AND asset_symbol<>'ETH'`, int64(s.cfg.ChainID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []assetRow
	for rows.Next() {
		var a assetRow
		if err := rows.Scan(&a.symbol, &a.contract); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// createBatch 单事务建批 + 入队 TxManager (nonce 分配 QUEUED, 随后自动首播).
// cold 为本 tick 解析出的 ACTIVE 冷金库 (deployAndSweepBatch 目标, 落
// target_cold_wallet 快照 — 后续重发/rebuild 按批次快照编码).
func (s *Sweeper) createBatch(ctx context.Context, a assetRow, vbs []vaultBalance,
	cold types.Address) error {
	if s.cfg.BatchSweeper == (types.Address{}) || cold == (types.Address{}) {
		return fmt.Errorf("sweeper: BatchSweeper/cold wallet address not configured")
	}
	total := new(big.Int)
	items := make([]chain.SweepItem, 0, len(vbs))
	for _, vb := range vbs {
		items = append(items, chain.SweepItem{Salt: vb.salt, ExactAmount: vb.amount})
		total.Add(total, vb.amount)
	}
	data, err := chain.EncodeDeployAndSweepBatch(items,
		types.HexToAddress(a.contract), cold)
	if err != nil {
		return err
	}

	var batchID uuid.UUID
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO sweep_batches (chain_id, asset_symbol, target_cold_wallet, status)
			 VALUES ($1,$2,$3,'PENDING') RETURNING id`,
			int64(s.cfg.ChainID), a.symbol, addrStr(cold)).Scan(&batchID); err != nil {
			return err
		}
		for _, vb := range vbs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO sweep_batch_items (batch_id, deposit_address_id, exact_sweep_amount)
				 VALUES ($1,$2,$3::numeric)`, batchID, vb.daID, vb.amount.String()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 入队 TxManager — relayer 热钱包 nonce; 合约要求 msg.sender==relayerHot.
	gas := s.cfg.GasBase + s.cfg.GasItem*uint64(len(items))
	_, err = s.txm.QueueIntent(ctx, ports.QueueIntentRequest{
		IntentType:           types.IntentTypeSweepBatch,
		ReferenceID:          batchID,
		ChainID:              s.cfg.ChainID,
		WalletAddress:        s.cfg.RelayerHot,
		To:                   s.cfg.BatchSweeper,
		Value:                types.U256FromBig(new(big.Int)),
		Calldata:             data,
		GasLimit:             gas,
		MaxFeePerGas:         s.cfg.MaxFeePerGas,
		MaxPriorityFeePerGas: s.cfg.MaxPriorityFee,
	})
	if err != nil {
		return fmt.Errorf("queue sweep intent: %w", err)
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE sweep_batches SET status='IN_FLIGHT' WHERE id=$1 AND status='PENDING'`,
		batchID); err != nil {
		return err
	}
	s.log.Info("sweep batch queued",
		zap.String("batch", batchID.String()), zap.String("asset", a.symbol),
		zap.Int("items", len(items)), zap.String("total", total.String()))
	return nil
}

// ---------------------------------------------------------------------------
// B2 — 批次泄漏回收 (reaper) 与 ReleaseBatch 补偿
//
// 泄漏面: ① createBatch 落批次+items(PENDING) 后 QueueIntent 失败 → 批次滞留
// PENDING 无关联意图; ② 链上 Revert/Filler 取消/拒签使关联意图进失败终态,
// indexer 只翻批次 FAILED (finalize.go) 不释放 PENDING items; ③ 终态批次
// 遗留 PENDING items (事件未匹配等异常). 聚合扣减已按批次状态过滤 (B2-①),
// 本回收器负责把占用落到 RELEASED 审计态并让 vault 重新可归集.
// ---------------------------------------------------------------------------

// intentFailureTerminal 关联出站意图的失败终态 (命中即释放批次).
// REJECTED_BY_SIGNER 为 002 迁移新增终态.
var intentFailureTerminal = map[string]bool{
	"FAILED_REVERTED":     true,
	"CANCELLED_BY_FILLER": true,
	"NONCE_STOLEN_HALT":   true,
	"REJECTED_BY_SIGNER":  true,
}

// ReleaseBatch 作废批次: 活跃批次 -> FAILED, 全部未结 PENDING items ->
// RELEASED (单事务). 导出供 indexer/编排层在"意图终态失败"结算路径调用
// (Wave-2 可把 finalize.go 的批次 FAILED 分支改调本函数; 当前由
// reconcileBatches 兜底保证等价语义).
func (s *Sweeper) ReleaseBatch(ctx context.Context, batchID uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE sweep_batches SET status='FAILED'
			  WHERE id=$1 AND status IN ('PENDING','IN_FLIGHT')`, batchID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE sweep_batch_items SET item_status='RELEASED'
			  WHERE batch_id=$1 AND item_status='PENDING'`, batchID)
		return err
	})
}

// reconcileBatches 每 tick 执行的批次回收:
//
//	(a) 终态批次 (FAILED/COMPLETED/PARTIAL_QUARANTINED) 遗留 PENDING items → RELEASED;
//	(b) IN_FLIGHT 批次且关联 SWEEP_BATCH 意图进失败终态 → ReleaseBatch;
//	(c) PENDING 批次 → 有关联活意图则补标 IN_FLIGHT (自愈), 无意图则
//	    重建 calldata 重试 QueueIntent; 超时/超次数 → FAILED + RELEASED.
func (s *Sweeper) reconcileBatches(ctx context.Context) error {
	// (a) 终态批次遗留 PENDING items 释放 (覆盖 indexer FAILED 分支遗留).
	if _, err := s.db.Exec(ctx,
		`UPDATE sweep_batch_items i SET item_status='RELEASED'
		   FROM sweep_batches b
		  WHERE i.batch_id=b.id AND i.item_status='PENDING'
		    AND b.chain_id=$1
		    AND b.status IN ('FAILED','COMPLETED','PARTIAL_QUARANTINED')`,
		int64(s.cfg.ChainID)); err != nil {
		return err
	}

	// (b) IN_FLIGHT 批次 + 最新关联意图失败终态 → 整批释放
	// (LATERAL 取最新意图 — 历史终态行不误伤存活的重发意图; 无意图行的
	// IN_FLIGHT 批次属数据不一致, 仅 CRITICAL 告警不自动处置 — 该批次可能
	// 真的曾有在途广播, 盲目释放/重发会造成重复归集).
	rows, err := s.db.Query(ctx,
		`SELECT b.id, i.id, i.status
		   FROM sweep_batches b
		   LEFT JOIN LATERAL (
		       SELECT id, status FROM outbound_tx_intents
		        WHERE intent_type='SWEEP_BATCH' AND reference_id=b.id
		        ORDER BY created_at DESC LIMIT 1) i ON TRUE
		  WHERE b.chain_id=$1 AND b.status='IN_FLIGHT'`,
		int64(s.cfg.ChainID))
	if err != nil {
		return err
	}
	var suspects []struct {
		batch  uuid.UUID
		intent *uuid.UUID
		st     *string
	}
	for rows.Next() {
		var r struct {
			batch  uuid.UUID
			intent *uuid.UUID
			st     *string
		}
		if err := rows.Scan(&r.batch, &r.intent, &r.st); err != nil {
			rows.Close()
			return err
		}
		suspects = append(suspects, r)
	}
	rows.Close()
	for _, r := range suspects {
		if r.intent == nil {
			s.raise(Alert{Severity: "CRITICAL", Code: "SWEEP_BATCH_ORPHANED",
				Message: "IN_FLIGHT sweep batch has no outbound intent row — manual reconciliation required",
				Detail:  fmt.Sprintf("batch=%s chain=%d", r.batch, s.cfg.ChainID)})
			continue
		}
		if !intentFailureTerminal[*r.st] {
			continue
		}
		if err := s.ReleaseBatch(ctx, r.batch); err != nil {
			return fmt.Errorf("release batch %s (intent %s): %w", r.batch, *r.st, err)
		}
		s.raise(Alert{Severity: "WARNING", Code: "SWEEP_BATCH_RELEASED",
			Message: fmt.Sprintf("sweep batch released: outbound intent reached %s", *r.st),
			Detail:  fmt.Sprintf("batch=%s intent=%s chain=%d", r.batch, *r.intent, s.cfg.ChainID)})
	}

	// (c) PENDING 批次 reaper.
	rows2, err := s.db.Query(ctx,
		`SELECT id, created_at FROM sweep_batches
		  WHERE chain_id=$1 AND status='PENDING' ORDER BY created_at`,
		int64(s.cfg.ChainID))
	if err != nil {
		return err
	}
	type pendingBatch struct {
		id        uuid.UUID
		createdAt time.Time
	}
	var pend []pendingBatch
	for rows2.Next() {
		var b pendingBatch
		if err := rows2.Scan(&b.id, &b.createdAt); err != nil {
			rows2.Close()
			return err
		}
		pend = append(pend, b)
	}
	rows2.Close()
	for _, b := range pend {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.requeuePendingBatch(ctx, b.id, b.createdAt); err != nil {
			s.log.Warn("requeue pending batch failed",
				zap.String("batch", b.id.String()), zap.Error(err))
		}
	}
	return nil
}

// requeuePendingBatch 单 PENDING 批次处置 (reaper ③).
func (s *Sweeper) requeuePendingBatch(ctx context.Context, batchID uuid.UUID, createdAt time.Time) error {
	var intentID uuid.UUID
	var istatus string
	err := s.db.QueryRow(ctx,
		`SELECT id, status FROM outbound_tx_intents
		  WHERE intent_type='SWEEP_BATCH' AND reference_id=$1
		  ORDER BY created_at DESC LIMIT 1`, batchID).Scan(&intentID, &istatus)
	switch {
	case err == nil:
		switch {
		case intentFailureTerminal[istatus]:
			if err := s.ReleaseBatch(ctx, batchID); err != nil {
				return err
			}
			s.raise(Alert{Severity: "WARNING", Code: "SWEEP_BATCH_RELEASED",
				Message: fmt.Sprintf("pending sweep batch released: intent %s reached %s", intentID, istatus),
				Detail:  fmt.Sprintf("batch=%s chain=%d", batchID, s.cfg.ChainID)})
			return nil
		default:
			// 意图存活 (QUEUED/IN_FLIGHT/INCLUDED_PENDING_SAFE/CONFIRMED) —
			// QueueIntent 成功但批次补标曾失败 → 自愈推进 IN_FLIGHT.
			_, err := s.db.Exec(ctx,
				`UPDATE sweep_batches SET status='IN_FLIGHT'
				  WHERE id=$1 AND status='PENDING'`, batchID)
			return err
		}
	case errors.Is(err, pgx.ErrNoRows):
		// 无关联意图 — 当初 QueueIntent 失败 (nonce 熔断/DB 抖动等).
	default:
		return err
	}

	// 无意图: 超时或超限放弃 → FAILED + RELEASED.
	var attempts int
	if err := s.db.QueryRow(ctx,
		`SELECT queue_attempts FROM sweep_batches WHERE id=$1`, batchID).Scan(&attempts); err != nil {
		return err
	}
	if s.now().Sub(createdAt) >= s.cfg.PendingBatchTimeout || attempts >= s.cfg.MaxQueueAttempts {
		if err := s.ReleaseBatch(ctx, batchID); err != nil {
			return err
		}
		s.raise(Alert{Severity: "CRITICAL", Code: "SWEEP_BATCH_RELEASED",
			Message: "pending sweep batch abandoned: QueueIntent retries exhausted/timeout — vault funds re-sweepable",
			Detail: fmt.Sprintf("batch=%s age=%s attempts=%d chain=%d",
				batchID, s.now().Sub(createdAt), attempts, s.cfg.ChainID)})
		return nil
	}

	// 重建 calldata 重试入队 (与 createBatch 同编码路径).
	data, nItems, err := s.encodeBatchCalldata(ctx, batchID)
	if err != nil {
		return err
	}
	gas := s.cfg.GasBase + s.cfg.GasItem*uint64(nItems)
	_, qerr := s.txm.QueueIntent(ctx, ports.QueueIntentRequest{
		IntentType:           types.IntentTypeSweepBatch,
		ReferenceID:          batchID,
		ChainID:              s.cfg.ChainID,
		WalletAddress:        s.cfg.RelayerHot,
		To:                   s.cfg.BatchSweeper,
		Value:                types.U256FromBig(new(big.Int)),
		Calldata:             data,
		GasLimit:             gas,
		MaxFeePerGas:         s.cfg.MaxFeePerGas,
		MaxPriorityFeePerGas: s.cfg.MaxPriorityFee,
	})
	if _, uerr := s.db.Exec(ctx,
		`UPDATE sweep_batches SET queue_attempts=queue_attempts+1 WHERE id=$1`,
		batchID); uerr != nil {
		return uerr
	}
	if qerr != nil {
		s.log.Warn("pending batch requeue attempt failed",
			zap.String("batch", batchID.String()), zap.Int("attempt", attempts+1), zap.Error(qerr))
		return nil // 下轮再试 (计数已增)
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE sweep_batches SET status='IN_FLIGHT' WHERE id=$1 AND status='PENDING'`,
		batchID); err != nil {
		return err
	}
	s.log.Info("pending sweep batch re-queued",
		zap.String("batch", batchID.String()), zap.Int("attempt", attempts+1))
	return nil
}

// encodeBatchCalldata 为 PENDING 批次重建 deployAndSweepBatch calldata
// (与 txmanager.rebuildSweep 同数据口径; 返回 items 数供 gas 估算).
func (s *Sweeper) encodeBatchCalldata(ctx context.Context, batchID uuid.UUID) ([]byte, int, error) {
	var asset, cold string
	if err := s.db.QueryRow(ctx,
		`SELECT asset_symbol, target_cold_wallet FROM sweep_batches WHERE id=$1`,
		batchID).Scan(&asset, &cold); err != nil {
		return nil, 0, err
	}
	var token string
	if err := s.db.QueryRow(ctx,
		`SELECT token_contract FROM supported_assets
		  WHERE chain_id=$1 AND asset_symbol=$2`,
		int64(s.cfg.ChainID), asset).Scan(&token); err != nil {
		return nil, 0, fmt.Errorf("resolve token %s@%d: %w", asset, s.cfg.ChainID, err)
	}
	rows, err := s.db.Query(ctx,
		`SELECT uda.create2_salt, i.exact_sweep_amount::text
		   FROM sweep_batch_items i
		   JOIN user_deposit_addresses uda ON uda.id = i.deposit_address_id
		  WHERE i.batch_id=$1 AND i.item_status='PENDING' ORDER BY i.id`, batchID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var items []chain.SweepItem
	for rows.Next() {
		var salt []byte
		var amt string
		if err := rows.Scan(&salt, &amt); err != nil {
			return nil, 0, err
		}
		var h types.Hash
		copy(h[:], salt)
		items = append(items, chain.SweepItem{Salt: h, ExactAmount: types.MustParseU256(amt).Big()})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(items) == 0 {
		return nil, 0, fmt.Errorf("sweep batch %s has no pending items", batchID)
	}
	data, err := chain.EncodeDeployAndSweepBatch(items,
		types.HexToAddress(token), types.HexToAddress(cold))
	if err != nil {
		return nil, 0, err
	}
	return data, len(items), nil
}

// ---------------------------------------------------------------------------
// 水位监控
// ---------------------------------------------------------------------------

// WaterOnce 水位巡检: payout 热钱包各资产 + 双热钱包 ETH, 低水位告警.
func (s *Sweeper) WaterOnce(ctx context.Context) error {
	if s.chain == nil {
		return nil
	}
	assets, err := s.assets(ctx)
	if err != nil {
		return err
	}
	for _, a := range assets {
		th := s.cfg.LowWaterToken[a.symbol]
		if th == nil {
			continue
		}
		bal, err := s.chain.ERC20BalanceOf(ctx, types.HexToAddress(a.contract), s.cfg.PayoutHot)
		if err != nil {
			s.log.Warn("token water probe failed", zap.String("asset", a.symbol), zap.Error(err))
			continue
		}
		if bal.Cmp(th) < 0 {
			s.raise(Alert{Severity: "WARNING", Code: "LOW_TOKEN_WATER",
				Message: fmt.Sprintf("payout hot wallet %s balance %s below low-water %s",
					a.symbol, bal.String(), th.String()),
				Detail: fmt.Sprintf("chain=%d wallet=%s", s.cfg.ChainID, s.cfg.PayoutHot.Hex())})
		}
	}
	if s.cfg.LowWaterNative != nil {
		for _, w := range []types.Address{s.cfg.PayoutHot, s.cfg.RelayerHot} {
			if w == (types.Address{}) {
				continue
			}
			bal, err := s.chain.NativeBalance(ctx, w)
			if err != nil {
				s.log.Warn("native water probe failed", zap.String("wallet", w.Hex()), zap.Error(err))
				continue
			}
			if bal.Cmp(s.cfg.LowWaterNative) < 0 {
				s.raise(Alert{Severity: "WARNING", Code: "LOW_NATIVE_WATER",
					Message: fmt.Sprintf("hot wallet %s ETH balance %s below low-water %s",
						w.Hex(), bal.String(), s.cfg.LowWaterNative.String()),
					Detail: fmt.Sprintf("chain=%d", s.cfg.ChainID)})
			}
		}
	}
	return nil
}

// addrStr 规范化 (与 txmanager/indexer 同).
func addrStr(a types.Address) string {
	s := a.Hex()
	out := make([]byte, 0, len(s))
	out = append(out, s[:2]...)
	for i := 2; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

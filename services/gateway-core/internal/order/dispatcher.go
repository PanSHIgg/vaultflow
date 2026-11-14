package order

// FIX-D2: MERCHANT_PAYOUT 出站调度器.
//
// 此前 LOCKED_QUEUED 的 merchant_payouts 没有任何生产者 — 冻结腿已记但
// 出站 Intent 永不创建, §5.4 状态机死锁在首态. 本调度器周期扫描
// LOCKED_QUEUED 行, 组 ERC-20 transfer(to, amount) calldata, 经
// ports.TxManager.QueueIntent 落 MERCHANT_PAYOUT 意图 (reference_id =
// payout.ID 即 signer 幂等槽 payout_uuid), 成功后推进 IN_FLIGHT.
//
// 幂等与并发语义:
//   - QueueIntent 契约 (B3): (intent_type, reference_id) 冲突返回既有意图;
//     本实现另做出站意图预读兜底, 与唯一索引落地先后解耦;
//   - 崩溃于 QueueIntent 成功 / IN_FLIGHT 迁移之间的窗口由下一轮
//     "意图已存在 -> 直接推进状态"恢复, 不会重复发单;
//   - 并发取消竞速获胜时尽力 RequestCancel 已建意图, 防链上漏账;
//   - 失败按单条 payout 指数退避, 不阻塞批内其它单.
//
// 结算边界 (防双记): 本调度器只调 OnPayoutInFlight;
// OnPayoutConfirmed/OnPayoutFailed 的 PAYOUT_FLOW 第二腿由 indexer
// 出站 finalize 路径独占 (payoutexec:{att}:c{n} 参考键), 严禁重复接线.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// PayoutChainConfig 单链 payout 热钱包与 EIP-1559 费率/限额
// (Wave-2 I1 由部署配置装配; 缺链配置的 payout 跳过并退避).
type PayoutChainConfig struct {
	PayoutWallet         types.Address // payout_hot_wallet (Nonce 归属 + Policy 签名主体)
	GasLimit             uint64        // 0 -> 默认 100_000 (与 txmanager GasLimitPayout 同口径)
	MaxFeePerGas         types.U256
	MaxPriorityFeePerGas types.U256
}

// PayoutDispatcherConfig 调度器运行参数.
type PayoutDispatcherConfig struct {
	Chains map[types.ChainID]PayoutChainConfig
	// PollInterval 扫描周期 (默认 2s); BatchSize 单轮上限 (默认 32).
	PollInterval time.Duration
	BatchSize    int
	// BaseBackoff 失败退避基数 (默认 1s); MaxBackoff 退避上限 (默认 5m).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

const (
	defaultPollInterval  = 2 * time.Second
	defaultDispatchBatch = 32
	defaultBaseBackoff   = time.Second
	defaultMaxBackoff    = 5 * time.Minute
	defaultPayoutGas     = 100_000
)

// PayoutDispatcher LOCKED_QUEUED payout -> MERCHANT_PAYOUT intent 调度器.
type PayoutDispatcher struct {
	svc *Service
	txm ports.TxManager
	cfg PayoutDispatcherConfig
	log *zap.Logger

	mu     sync.Mutex
	fails  map[uuid.UUID]dispatchFail
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type dispatchFail struct {
	count     int
	notBefore time.Time
}

// NewPayoutDispatcher 构造调度器; txm 为出站意图管理器 (QueueIntent 幂等
// 契约见上), svc 复用其连接池与 OnPayoutInFlight 白名单迁移.
func NewPayoutDispatcher(svc *Service, txm ports.TxManager, cfg PayoutDispatcherConfig, log *zap.Logger) *PayoutDispatcher {
	if log == nil {
		log = zap.NewNop()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultDispatchBatch
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = defaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	return &PayoutDispatcher{svc: svc, txm: txm, cfg: cfg, log: log, fails: make(map[uuid.UUID]dispatchFail)}
}

// Start 后台周期调度 (Wave-2 I1 装配时调用); Stop 幂等停等.
func (d *PayoutDispatcher) Start(ctx context.Context) {
	cctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(d.cfg.PollInterval)
		defer ticker.Stop()
		for {
			if _, err := d.DispatchOnce(cctx); err != nil && !errors.Is(err, context.Canceled) {
				d.log.Warn("payout dispatch pass failed", zap.Error(err))
			}
			select {
			case <-cctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop 取消调度循环并等待退出.
func (d *PayoutDispatcher) Stop() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// DispatchOnce 单轮调度: 捞取 LOCKED_QUEUED 批, 逐条建意图并推进
// IN_FLIGHT; 返回本轮成功入队/恢复的条数. 批级扫描失败返回 error.
func (d *PayoutDispatcher) DispatchOnce(ctx context.Context) (int, error) {
	rows, err := d.svc.pool.Query(ctx,
		`SELECT `+payoutColumns+` FROM merchant_payouts
		 WHERE status = 'LOCKED_QUEUED' ORDER BY created_at, id LIMIT $1`,
		d.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("order: scan queued payouts: %w", err)
	}
	var batch []types.MerchantPayout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("order: scan payout row: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()

	dispatched := 0
	for _, p := range batch {
		if d.backedOff(p.ID) {
			continue
		}
		if err := d.dispatchOne(ctx, p); err != nil {
			d.recordFail(p.ID, err)
			continue
		}
		d.clearFail(p.ID)
		dispatched++
	}
	return dispatched, nil
}

// dispatchOne 单条 payout 编排: 意图预读 -> (缺失则) QueueIntent ->
// OnPayoutInFlight. 幂等: 意图已存在 (预读命中或 B3 冲突返回既有)
// 直接推进状态; QueueIntent 报错计入退避由下一轮重试.
func (d *PayoutDispatcher) dispatchOne(ctx context.Context, p types.MerchantPayout) error {
	cc, ok := d.cfg.Chains[p.ChainID]
	if !ok || cc.PayoutWallet == (types.Address{}) {
		return fmt.Errorf("order: payout wallet not configured for chain %d", p.ChainID)
	}

	// 意图预读: QueueIntent 幂等契约 (冲突返回既有) 的本地兜底 —
	// 崩溃恢复与并发双派发实例均收敛到同一意图, 不产生重复广播.
	intentID := uuid.Nil
	var haveIntent bool
	err := d.svc.pool.QueryRow(ctx, `
		SELECT id FROM outbound_tx_intents
		WHERE intent_type = $1 AND reference_id = $2
		  AND status IN ('QUEUED','IN_FLIGHT','INCLUDED_PENDING_SAFE','CONFIRMED','NONCE_STOLEN_HALT')
		ORDER BY created_at DESC LIMIT 1`,
		types.IntentTypeMerchantPayout.String(), p.ID).Scan(&intentID)
	switch {
	case err == nil:
		haveIntent = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return fmt.Errorf("order: intent lookup: %w", err)
	}

	if !haveIntent {
		token, err := d.resolveTokenContract(ctx, p)
		if err != nil {
			return err
		}
		calldata, err := chain.EncodeERC20Transfer(p.ToAddress, p.Amount.Big())
		if err != nil {
			return fmt.Errorf("order: encode transfer calldata: %w", err)
		}
		gasLimit := cc.GasLimit
		if gasLimit == 0 {
			gasLimit = defaultPayoutGas
		}
		intent, err := d.txm.QueueIntent(ctx, ports.QueueIntentRequest{
			IntentType:           types.IntentTypeMerchantPayout,
			ReferenceID:          p.ID, // == 票据内嵌 payout_uuid == SignTxRequest.payout_uuid
			ChainID:              p.ChainID,
			WalletAddress:        cc.PayoutWallet,
			To:                   token,
			Value:                types.U256{},
			Calldata:             calldata,
			GasLimit:             gasLimit,
			MaxFeePerGas:         cc.MaxFeePerGas,
			MaxPriorityFeePerGas: cc.MaxPriorityFeePerGas,
			RetrySeq:             uint32(p.RetrySeq),
			RefundTicket:         splitStoredRefundTicket(p.AdminRefundTicket),
		})
		if err != nil {
			return fmt.Errorf("order: queue payout intent: %w", err)
		}
		intentID = intent.ID
	}

	// LOCKED_QUEUED -> IN_FLIGHT (白名单迁移; 不设账本腿 — §5.4 该边
	// 仅关联出站意图). 并发语义:
	//   - ErrInvalidTransition + 现态 CANCELLED_UNLOCKED: 取消竞速获胜,
	//     已建意图须尽力 RequestCancel (GAP_FILLER) 防链上漏账;
	//   - ErrInvalidTransition + 现态 IN_FLIGHT/其它: 并发派发实例已推进,
	//     良性返回.
	if err := d.svc.OnPayoutInFlight(ctx, p.ID); err != nil {
		if errors.Is(err, ports.ErrInvalidTransition) {
			cur, gerr := d.svc.GetPayout(ctx, p.ID)
			if gerr == nil && cur.Status == types.PayoutCancelledUnlocked && intentID != uuid.Nil {
				if cerr := d.txm.RequestCancel(ctx, intentID); cerr != nil {
					d.log.Warn("payout cancelled post-queue; intent cancel best-effort failed",
						zap.String("payout_id", p.ID.String()), zap.Error(cerr))
				} else {
					d.log.Warn("payout cancelled post-queue; intent cancel requested",
						zap.String("payout_id", p.ID.String()))
				}
			}
			return nil
		}
		return fmt.Errorf("order: mark in-flight: %w", err)
	}
	return nil
}

// resolveTokenContract supported_assets 解析 ERC-20 合约地址
// (与 txmanager rebuildPayout 同一口径).
func (d *PayoutDispatcher) resolveTokenContract(ctx context.Context, p types.MerchantPayout) (types.Address, error) {
	var tokenHex string
	err := d.svc.pool.QueryRow(ctx,
		`SELECT token_contract FROM supported_assets WHERE chain_id = $1 AND asset_symbol = $2`,
		int64(p.ChainID), p.AssetSymbol).Scan(&tokenHex)
	if err != nil {
		return types.Address{}, fmt.Errorf("order: resolve token %s@%d: %w", p.AssetSymbol, p.ChainID, err)
	}
	return types.HexToAddress(tokenHex), nil
}

// ---------------------------------------------------------------------------
// 失败退避 (进程内; 重启清零属良性 — 下轮 PollInterval 即恢复)
// ---------------------------------------------------------------------------

func (d *PayoutDispatcher) backedOff(id uuid.UUID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.fails[id]
	return ok && time.Now().Before(f.notBefore)
}

func (d *PayoutDispatcher) recordFail(id uuid.UUID, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.fails[id]
	f.count++
	delay := d.cfg.BaseBackoff
	for i := 1; i < f.count && delay < d.cfg.MaxBackoff; i++ {
		delay *= 2
		if delay > d.cfg.MaxBackoff {
			delay = d.cfg.MaxBackoff
		}
	}
	f.notBefore = time.Now().Add(delay)
	d.fails[id] = f
	d.log.Warn("payout dispatch failed; backing off",
		zap.String("payout_id", id.String()), zap.Int("fail_count", f.count),
		zap.Duration("next_retry_in", delay), zap.Error(err))
}

func (d *PayoutDispatcher) clearFail(id uuid.UUID) {
	d.mu.Lock()
	delete(d.fails, id)
	d.mu.Unlock()
}

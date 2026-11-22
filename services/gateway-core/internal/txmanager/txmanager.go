// Package txmanager implements W4-2 后半: 交易生命周期引擎.
//
//   - wallet_nonce_cursors SELECT ... FOR UPDATE 同事务分配连续 Nonce,
//     落 outbound_tx_intents(QUEUED) (§4.2 / plan W4-2);
//   - 组装 EIP-1559 (EIP-2718 Type-2) 未签名交易 → vault-signer 策略签名 →
//     eth_sendRawTransaction 广播 → outbound_tx_attempts(BROADCASTED);
//   - Nonce 巡检矩阵 (ARCHITECTURE §4.2):
//     ① 超时未入块 RBF attempt_no+1 提价;
//     ② Mempool 驱逐 (K>=latest_nonce、全部候选 getTransactionByHash==nil、
//     pending_nonce<=K) 标 DROPPED → 命中 Signer 幂等槽重签补发 (新 attempt 行);
//     ③ 在途取消追加同 Nonce GAP_FILLER 候选;
//     ④ 带外占用熔断 (K<latest_nonce 且本系统全部候选无回执; QUEUED 首播
//     SendRawTx 失败时以 latest-nonce 探针做零 attempt 等价判定) →
//     NONCE_STOLEN_HALT 停队列 + 紧急告警.
//
// DROPPED 不可复活 — 只能以新 attempt_no 补发; SUPERSEDED→BROADCASTED
// 仅由 §4.3 LIFO 深回滚路径放行 (indexer 调用 RebroadcastIntent).
package txmanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 依赖抽象 (单测 fake 注入; 生产由 chain.Pool / signerclient.Client 满足)
// ---------------------------------------------------------------------------

// ChainProvider TxManager 所需的链能力子集 (广播 / mempool 可见性 / nonce / 回执).
type ChainProvider interface {
	// SendRawTx 广播已签名原始字节; 返回链上 tx_hash.
	// IsAlreadyKnownError 类幂等错误由实现端转成功或原样返回 (调用方兜底).
	SendRawTx(ctx context.Context, raw []byte) (types.Hash, error)
	// TxPresent 交易是否被节点看到; mined 表示已被打包进块 (非 pending).
	TxPresent(ctx context.Context, hash types.Hash) (found bool, mined bool, err error)
	// Receipt eth_getTransactionReceipt; 未找到返回 (nil, nil).
	Receipt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error)
	// Nonce eth_getTransactionCount: pending=false→latest, true→pending.
	Nonce(ctx context.Context, wallet types.Address, pending bool) (uint64, error)
}

// Alert 安全/运维告警.
type Alert struct {
	Severity string // INFO / WARNING / CRITICAL
	Code     string
	Message  string
	Detail   string
	At       time.Time
}

// AlertFunc 告警投递回调.
type AlertFunc func(Alert)

// AlertCodeNonceStolen 私钥带外占用熔断 — 最高优先级.
const AlertCodeNonceStolen = "NONCE_STOLEN_HALT"

// Config TxManager 运行参数 (均有开发态默认值, 测试可调小).
type Config struct {
	PollInterval      time.Duration // 派发/巡检 tick (默认 2s)
	InFlightTimeout   time.Duration // 超时未入块触发 RBF (默认 45s)
	DroppedMinAge     time.Duration // 候选至少存活此时长方可判 DROPPED (默认 30s)
	FeeBumpBps        int64         // RBF/重发提价基点 (默认 1250 = +12.5%)
	GasLimitPayout    uint64        // ERC-20 transfer (默认 100_000)
	GasLimitRelay     uint64        // EIP3009 router (默认 200_000)
	GasLimitSweepBase uint64        // 归集基础 (默认 80_000)
	GasLimitSweepItem uint64        // 归集每条目 (默认 160_000)
	MaxDispatchBatch  int           // 单 tick 最多首播意图数 (默认 32)
}

func (c *Config) withDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.InFlightTimeout <= 0 {
		c.InFlightTimeout = 45 * time.Second
	}
	if c.DroppedMinAge <= 0 {
		c.DroppedMinAge = 30 * time.Second
	}
	if c.FeeBumpBps <= 0 {
		c.FeeBumpBps = 1250
	}
	if c.GasLimitPayout == 0 {
		c.GasLimitPayout = 100_000
	}
	if c.GasLimitRelay == 0 {
		c.GasLimitRelay = 200_000
	}
	if c.GasLimitSweepBase == 0 {
		c.GasLimitSweepBase = 80_000
	}
	if c.GasLimitSweepItem == 0 {
		c.GasLimitSweepItem = 160_000
	}
	if c.MaxDispatchBatch <= 0 {
		c.MaxDispatchBatch = 32
	}
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager 交易生命周期引擎, 实现 ports.TxManager.
type Manager struct {
	db     *pgxpool.Pool
	chain  ChainProvider
	signer ports.SignerClient
	cfg    Config
	log    *zap.Logger
	alertf AlertFunc

	// pendingParams 首播请求的内存载荷 fast-path (002 迁移起载荷已持久化到
	// outbound_tx_intents.payload_* 列; 重启后经 rebuildTxParams 从列/业务表
	// 确定性重建 — 详见 rebuild.go 说明).
	pendingParams sync.Map // uuid.UUID -> ports.QueueIntentRequest

	dispatch chan struct{}
	now      func() time.Time // 测试可注入时钟

	// chainContracts 每链合约地址配置 (rebuild 路径; 装配注入, 见 SetChainContracts).
	chainContracts map[types.ChainID]ChainContracts

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 构造 Manager (不启动后台协程 — Start 显式开启).
func New(db *pgxpool.Pool, chain ChainProvider, signer ports.SignerClient,
	cfg Config, log *zap.Logger, alertf AlertFunc) *Manager {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	return &Manager{
		db:       db,
		chain:    chain,
		signer:   signer,
		cfg:      cfg,
		log:      log,
		alertf:   alertf,
		dispatch: make(chan struct{}, 1),
		now:      time.Now,
	}
}

// raise 投递告警 (回调 + 结构化日志双通道).
func (m *Manager) raise(a Alert) {
	a.At = m.now()
	if m.alertf != nil {
		m.alertf(a)
	}
	lvl := m.log.Warn
	if a.Severity == "CRITICAL" {
		lvl = m.log.Error
	}
	lvl("txmanager alert",
		zap.String("severity", a.Severity), zap.String("code", a.Code),
		zap.String("message", a.Message), zap.String("detail", a.Detail))
}

// kick 非阻塞唤醒派发协程.
func (m *Manager) kick() {
	select {
	case m.dispatch <- struct{}{}:
	default:
	}
}

// Start 启动派发与巡检后台协程 (幂等; Stop 等待退出).
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return
	}
	ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(m.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.dispatch:
				m.drain(ctx)
			case <-t.C:
				m.drain(ctx)
				if err := m.WatchdogTick(ctx); err != nil {
					m.log.Warn("watchdog tick failed", zap.Error(err))
				}
			}
		}
	}()
}

func (m *Manager) drain(ctx context.Context) {
	if _, err := m.BroadcastQueued(ctx); err != nil {
		m.log.Warn("dispatch drain failed", zap.Error(err))
	}
}

// Stop 停止后台协程.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// ---------------------------------------------------------------------------
// ports.TxManager 实现
// ---------------------------------------------------------------------------

// liveIntentStatuses SQL — "存活"意图状态集, 与 003_intent_ref_idem.sql
// uq_intents_live_type_ref 部分唯一索引谓词一致 (终态行可同 reference 再分配).
const liveIntentStatuses = `'QUEUED','IN_FLIGHT','INCLUDED_PENDING_SAFE','CONFIRMED','NONCE_STOLEN_HALT'`

// idempotentRefType 该意图类型是否受 (intent_type, reference_id) 存活唯一约束
// (008 重建的 uq_intents_live_type_ref 仅覆盖 SWEEP_BATCH);
// NONCE_GAP_FILLER 的 reference_id 语义不同, 不受约束.
func idempotentRefType(t types.IntentType) bool {
	return t == types.IntentTypeSweepBatch
}

// QueueIntent 同事务分配连续 Nonce 并落 QUEUED 意图, 随后通知派发协程首播.
//
//   - B3: 同 (intent_type, reference_id) 已存在存活意图时幂等返回既有行,
//     不新占 Nonce (并发冲突由 uq_intents_live_type_ref 兜底, 23505 时回读);
//   - B5: 首见钱包链上 latest nonce 探测失败直接返回错误 (fail-closed),
//     绝不落 0 游标导致陈旧 nonce 死循环;
//   - B1: 载荷 (to/value/calldata/gas/fee) 同事务持久化 payload_* 列,
//     重启后 QUEUED/重发路径可确定性重建 (rebuild.go).
//
// 该钱包队列已被 NONCE_STOLEN_HALT 熔断时返回 ErrNonceHalted.
func (m *Manager) QueueIntent(ctx context.Context, req ports.QueueIntentRequest) (types.OutboundTxIntent, error) {
	if err := m.checkHalted(ctx, req.ChainID, req.WalletAddress); err != nil {
		return types.OutboundTxIntent{}, err
	}

	var intent types.OutboundTxIntent
	var wa string
	idempotentHit := false
	err := pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		// B3 幂等命中: 存活同引用意图直接返回 (不消费 nonce).
		if idempotentRefType(req.IntentType) {
			err := tx.QueryRow(ctx,
				`SELECT id, intent_type, reference_id, chain_id, wallet_address, nonce, status, created_at
				   FROM outbound_tx_intents
				  WHERE intent_type=$1 AND reference_id=$2
				    AND status IN (`+liveIntentStatuses+`)
				  ORDER BY created_at DESC LIMIT 1`,
				string(req.IntentType), req.ReferenceID).
				Scan(&intent.ID, &intent.IntentType, &intent.ReferenceID, &intent.ChainID,
					&wa, &intent.Nonce, &intent.Status, &intent.CreatedAt)
			if err == nil {
				idempotentHit = true
				return nil // 幂等返回既有意图
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		// 游标行 SELECT ... FOR UPDATE; 首见钱包探测链上 latest nonce 初始化.
		var nextNonce int64
		err := tx.QueryRow(ctx,
			`SELECT next_nonce FROM wallet_nonce_cursors
			  WHERE chain_id=$1 AND wallet_address=$2 FOR UPDATE`,
			int64(req.ChainID), addrStr(req.WalletAddress)).Scan(&nextNonce)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// B5 fail-closed: 探测失败不落游标 (防 nonce 起点错配死循环).
			initNonce, err := m.chain.Nonce(ctx, req.WalletAddress, false)
			if err != nil {
				return fmt.Errorf("txmanager: nonce probe failed for new wallet %s on chain %d (fail-closed): %w",
					req.WalletAddress.Hex(), req.ChainID, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO wallet_nonce_cursors (chain_id, wallet_address, next_nonce)
				 VALUES ($1,$2,$3) ON CONFLICT (chain_id, wallet_address) DO NOTHING`,
				int64(req.ChainID), addrStr(req.WalletAddress), int64(initNonce)); err != nil {
				return err
			}
			// 并发首见同钱包: 竞速输家的投机插入被吞掉后必须重读赢家已
			// 提交的游标行 (READ COMMITTED 下 ON CONFLICT 等待对方事务
			// 落定), 否则输家沿用本地 initNonce 与赢家撞出重复 nonce.
			if err := tx.QueryRow(ctx,
				`SELECT next_nonce FROM wallet_nonce_cursors
				  WHERE chain_id=$1 AND wallet_address=$2 FOR UPDATE`,
				int64(req.ChainID), addrStr(req.WalletAddress)).Scan(&nextNonce); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE wallet_nonce_cursors SET next_nonce=next_nonce+1, updated_at=now()
			  WHERE chain_id=$1 AND wallet_address=$2`,
			int64(req.ChainID), addrStr(req.WalletAddress)); err != nil {
			return err
		}
		// B1 载荷持久化: payload_* 列随意图同事务落库.
		if err := tx.QueryRow(ctx,
			`INSERT INTO outbound_tx_intents
			   (intent_type, reference_id, chain_id, wallet_address, nonce, status,
			    payload_to, payload_data, payload_value, payload_gas,
			    payload_max_fee, payload_max_tip)
			 VALUES ($1,$2,$3,$4,$5,'QUEUED',$6,$7,$8::numeric,$9,$10::numeric,$11::numeric)
			 RETURNING id, intent_type, reference_id, chain_id, wallet_address, nonce, status, created_at`,
			string(req.IntentType), req.ReferenceID, int64(req.ChainID),
			addrStr(req.WalletAddress), nextNonce,
			addrStr(req.To), req.Calldata, req.Value.String(), int64(req.GasLimit),
			req.MaxFeePerGas.String(), req.MaxPriorityFeePerGas.String()).
			Scan(&intent.ID, &intent.IntentType, &intent.ReferenceID, &intent.ChainID,
				&wa, &intent.Nonce, &intent.Status, &intent.CreatedAt); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		// 并发同引用: uq_intents_live_type_ref 23505 — 事务整体回滚 (nonce 不消耗),
		// 回读胜出的存活意图幂等返回.
		if idempotentRefType(req.IntentType) && isUniqueViolation(err, "uq_intents_live_type_ref") {
			if existing, e := m.liveIntentByRef(ctx, req.IntentType, req.ReferenceID); e == nil {
				// FIX-E/E5: 并发输家回读胜出意图后, 绝不能把本次请求的内存
				// 参数挂到胜出意图名下 — 两侧载荷是否一致无从证明 (同
				// reference 不同 calldata 的重试合法存在, 如重建后的
				// FAILED_RETRYABLE 换参重发). 仅当持久化 payload_* 与本次
				// 请求逐字段一致时才补 fast-path (与 rebuild 结果同源等价).
				m.attachParamsIfIdentical(ctx, existing, req)
				m.kick()
				return existing, nil
			}
		}
		return types.OutboundTxIntent{}, fmt.Errorf("txmanager: queue intent: %w", err)
	}
	intent.WalletAddress = types.HexToAddress(wa)

	if idempotentHit {
		// FIX-E/E5: 幂等命中复用存活意图 — 同样须校验持久化载荷与本次
		// 请求逐字段一致, 否则 retry 的内存参数会顶替胜出意图的权威
		// payload_* 进入签名 fast-path (载荷漂移即签错账).
		m.attachParamsIfIdentical(ctx, intent, req)
	} else {
		m.pendingParams.Store(intent.ID, req)
	}
	m.kick()
	return intent, nil
}

// attachParamsIfIdentical E5 — 23505 并发输家回读胜出意图后, 校验胜出意图
// 持久化 payload_* 与本次请求是否逐字段一致: 一致才允许把 req 挂到
// pendingParams fast-path (与 payload_* 重建结果同源等价, 不改变签名语义);
// 不一致 (同引用换参重试) 时只告警不挂载 — 胜出意图的持久化载荷保持
// 唯一权威, 保证最终签出的永远是胜者自己提交的载荷.
func (m *Manager) attachParamsIfIdentical(ctx context.Context, existing types.OutboundTxIntent,
	req ports.QueueIntentRequest) {
	var to string
	var data []byte
	var value, gas, fee, tip string
	err := m.db.QueryRow(ctx,
		`SELECT COALESCE(payload_to,''), COALESCE(payload_data,''::bytea),
		        COALESCE(payload_value::text,''), COALESCE(payload_gas::text,''),
		        COALESCE(payload_max_fee::text,''), COALESCE(payload_max_tip::text,'')
		   FROM outbound_tx_intents WHERE id=$1`, existing.ID).
		Scan(&to, &data, &value, &gas, &fee, &tip)
	if err != nil {
		m.log.Warn("re-read winning intent payload failed; losing retry params not attached",
			zap.String("intent", existing.ID.String()), zap.Error(err))
		return
	}
	identical := to == addrStr(req.To) && bytes.Equal(data, req.Calldata) &&
		value == req.Value.String() && gas == fmt.Sprintf("%d", req.GasLimit) &&
		fee == req.MaxFeePerGas.String() && tip == req.MaxPriorityFeePerGas.String()
	if !identical {
		m.log.Warn("queue-intent idempotent retry carries a DIFFERENT payload — "+
			"winning intent's persisted payload stays authoritative",
			zap.String("intent", existing.ID.String()),
			zap.String("type", string(req.IntentType)),
			zap.String("reference", existing.ReferenceID.String()))
		return
	}
	m.pendingParams.Store(existing.ID, req)
}

// liveIntentByRef 回读存活态同引用意图 (并发幂等命中与诊断查询共用).
func (m *Manager) liveIntentByRef(ctx context.Context, t types.IntentType, ref uuid.UUID) (types.OutboundTxIntent, error) {
	var it types.OutboundTxIntent
	var wa string
	err := m.db.QueryRow(ctx,
		`SELECT id, intent_type, reference_id, chain_id, wallet_address, nonce, status, created_at
		   FROM outbound_tx_intents
		  WHERE intent_type=$1 AND reference_id=$2
		    AND status IN (`+liveIntentStatuses+`)
		  ORDER BY created_at DESC LIMIT 1`, string(t), ref).
		Scan(&it.ID, &it.IntentType, &it.ReferenceID, &it.ChainID,
			&wa, &it.Nonce, &it.Status, &it.CreatedAt)
	if err != nil {
		return it, err
	}
	it.WalletAddress = types.HexToAddress(wa)
	return it, nil
}

// isUniqueViolation 判定 PG 23505 且可选约束/索引名匹配.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return constraint == "" || pgErr.ConstraintName == constraint
	}
	return false
}

// GetIntent 查询意图生命周期状态.
func (m *Manager) GetIntent(ctx context.Context, intentID uuid.UUID) (types.OutboundTxIntent, error) {
	row := m.db.QueryRow(ctx,
		`SELECT id, intent_type, reference_id, chain_id, wallet_address, nonce, status, created_at
		   FROM outbound_tx_intents WHERE id=$1`, intentID)
	var it types.OutboundTxIntent
	var wa string
	if err := row.Scan(&it.ID, &it.IntentType, &it.ReferenceID, &it.ChainID,
		&wa, &it.Nonce, &it.Status, &it.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return it, ports.ErrNotFound
		}
		return it, err
	}
	it.WalletAddress = types.HexToAddress(wa)
	return it, nil
}

// ListAttempts 列出意图下同 Nonce 全部候选 (RBF/补发/Filler 历史回放).
func (m *Manager) ListAttempts(ctx context.Context, intentID uuid.UUID) ([]types.OutboundTxAttempt, error) {
	rows, err := m.db.Query(ctx,
		`SELECT id, intent_id, attempt_no, attempt_kind, chain_id, tx_hash,
		        block_number, block_hash, receipt_status,
		        max_fee_per_gas::text, max_priority_fee_per_gas::text,
		        gas_used, effective_gas_price::text, status, broadcasted_at
		   FROM outbound_tx_attempts WHERE intent_id=$1 ORDER BY attempt_no`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.OutboundTxAttempt
	for rows.Next() {
		var a types.OutboundTxAttempt
		var maxFee, maxTip string
		var bn *int64
		var bh *string
		var rs *int32
		var gu *int64
		var egp *string
		var th string
		if err := rows.Scan(&a.ID, &a.IntentID, &a.AttemptNo, &a.Kind, &a.ChainID,
			&th, &bn, &bh, &rs, &maxFee, &maxTip, &gu, &egp,
			&a.Status, &a.BroadcastedAt); err != nil {
			return nil, err
		}
		a.TxHash = types.HexToHash(th)
		if bn != nil {
			v := uint64(*bn)
			a.BlockNumber = &v
		}
		if bh != nil {
			h := types.HexToHash(*bh)
			a.BlockHash = &h
		}
		a.ReceiptStatus = rs
		a.MaxFeePerGas = types.MustParseU256(maxFee)
		a.MaxPriorityFeePerGas = types.MustParseU256(maxTip)
		if gu != nil {
			v := uint64(*gu)
			a.GasUsed = &v
		}
		if egp != nil {
			u := types.MustParseU256(*egp)
			a.EffectiveGasPrice = &u
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NonceCursorStatus 热钱包 Nonce 游标与熔断状态 (监控台).
func (m *Manager) NonceCursorStatus(ctx context.Context, chainID types.ChainID, wallet types.Address) (ports.NonceCursorStatus, error) {
	out := ports.NonceCursorStatus{ChainID: chainID, WalletAddress: wallet}
	if err := m.db.QueryRow(ctx,
		`SELECT next_nonce FROM wallet_nonce_cursors
		  WHERE chain_id=$1 AND wallet_address=$2`,
		int64(chainID), addrStr(wallet)).Scan(&out.NextNonce); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	if err := m.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbound_tx_intents
		  WHERE chain_id=$1 AND wallet_address=$2 AND status='IN_FLIGHT'`,
		int64(chainID), addrStr(wallet)).Scan(&out.InFlightIntents); err != nil {
		return out, err
	}
	out.Halted = m.isHalted(ctx, chainID, wallet)
	return out, nil
}

// isHalted 该钱包是否存在 NONCE_STOLEN_HALT 熔断意图 (DB 为熔断唯一事实源).
func (m *Manager) isHalted(ctx context.Context, chainID types.ChainID, wallet types.Address) bool {
	var exists bool
	if err := m.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM outbound_tx_intents
		  WHERE chain_id=$1 AND wallet_address=$2 AND status='NONCE_STOLEN_HALT')`,
		int64(chainID), addrStr(wallet)).Scan(&exists); err != nil {
		return true // 查询失败时保守按熔断处理
	}
	return exists
}

func (m *Manager) checkHalted(ctx context.Context, chainID types.ChainID, wallet types.Address) error {
	if m.isHalted(ctx, chainID, wallet) {
		return fmt.Errorf("%w: %s on chain %d", ports.ErrNonceHalted, wallet.Hex(), chainID)
	}
	return nil
}

// addrStr 地址规范化: 0x 前缀小写 hex (§2.5.3 编码规范).
func addrStr(a types.Address) string {
	h := a.Hex()
	out := make([]byte, 0, len(h))
	out = append(out, h[:2]...)
	for i := 2; i < len(h); i++ {
		c := h[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// 编译期接口断言.
var (
	_ ports.TxManager = (*Manager)(nil)
	_                 = ethereum.NotFound // go-ethereum 版本锚点 (Receipt 语义依赖)
)

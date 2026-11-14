// Package sse 实现 ports.SSEBroker: Redis Pub/Sub 多实例分发 + 进程内
// 降级通知 + per-order 环形缓冲回放 (支撑 Last-Event-ID 断线续传).
package sse

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
)

// StreamEvent 带 per-order 单调 seq 的事件信封 (SSE "id:" 字段语义).
// FIX-D4: Src 为发布实例标识 — seq 只在本实例/INCR 空间内单调,
// 跨实例可能撞号; 远端事件去重键为 (Src, Seq) 而非 Seq 单字段,
// 否则实例 A 的 seq=1 会被误判为实例 B seq=1 的回声而丢真事件.
type StreamEvent struct {
	Seq   uint64           `json:"seq"`
	Src   string           `json:"src,omitempty"`
	Event ports.OrderEvent `json:"event"`
}

const (
	// defaultHistory 环形缓冲每订单保留条数.
	defaultHistory = 64
	// channelPrefix Redis Pub/Sub 频道前缀.
	channelPrefix = "vf:sse:order:"
	// seqKeyPrefix per-order 全局序列号 Redis INCR 键前缀
	// (多实例共用 -> 单订单 seq 全局单调, Last-Event-ID 语义跨实例一致).
	seqKeyPrefix = "vf:sse:seq:"
	// degradedSeqBase Redis 不可用时本地降级 seq 起点: 高位段与 INCR
	// 空间隔离防撞号 (INCR 达 2^62 需 ~146 亿年, 实际不可能重叠).
	degradedSeqBase = uint64(1) << 62
	// subBuf 订阅通道容量; 溢出即断开由客户端重同步 (不阻塞生产者).
	subBuf = 128
)

// Broker ports.SSEBroker 实现.
// 多实例: 生产者 Publish 时写 Redis channel 并落本实例缓冲;
// 每个实例的 dispatcher 协程 PSUBSCRIBE 所有订单频道, 按 channel 后缀
// 解析 orderID 派发给本地订阅者. Redis 断连仅退化跨实例分发, 进程内
// 订阅与回放不受影响 (openapi: "Redis outage transparently degrades
// fan-out to in-process delivery; ordering per id is still guaranteed").
type Broker struct {
	rdb        redis.UniversalClient
	log        *zap.Logger
	history    int
	instanceID string // 本实例标识, 写入信封 Src 供 (Src,Seq) 去重

	mu   sync.Mutex
	seq  map[uuid.UUID]uint64 // 本地分配计数兼远端水位 (仅 < degradedSeqBase)
	buf  map[uuid.UUID][]StreamEvent
	subs map[uuid.UUID]map[chan StreamEvent]struct{}

	stopCh  chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup
}

// NewBroker 装配: rdb 为 nil 时纯进程内模式 (本地计数分配 seq;
// rdb 非 nil 时经 INCR vf:sse:seq:<orderID> 分配跨实例单调 seq).
func NewBroker(rdb redis.UniversalClient, log *zap.Logger) *Broker {
	if log == nil {
		log = zap.NewNop()
	}
	b := &Broker{
		rdb:        rdb,
		log:        log,
		history:    defaultHistory,
		instanceID: uuid.NewString(),
		seq:        make(map[uuid.UUID]uint64),
		buf:        make(map[uuid.UUID][]StreamEvent),
		subs:       make(map[uuid.UUID]map[chan StreamEvent]struct{}),
		stopCh:     make(chan struct{}),
	}
	if rdb != nil {
		b.wg.Add(1)
		go b.dispatcher()
	}
	return b
}

// Close 停止 dispatcher 并关闭全部订阅通道.
func (b *Broker) Close() {
	if b.stopped.Swap(true) {
		return
	}
	close(b.stopCh)
	b.wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	for oid, set := range b.subs {
		for ch := range set {
			close(ch)
		}
		delete(b.subs, oid)
	}
}

// ---------------------------------------------------------------------------
// ports.SSEBroker 实现
// ---------------------------------------------------------------------------

// PublishOrderEvent 发布订单事件: 分配 per-order 单调 seq, 落环形缓冲,
// 投递进程内订阅者, 再尽力 Publish 至 Redis 供跨实例分发.
// Redis 失败仅告警降级, 不向生产方报错 (Redis 不承担账务状态).
func (b *Broker) PublishOrderEvent(ctx context.Context, ev ports.OrderEvent) error {
	e := StreamEvent{Seq: b.allocSeq(ctx, ev.OrderID), Src: b.instanceID, Event: ev}
	b.mu.Lock()
	buf := append(b.buf[ev.OrderID], e)
	if len(buf) > b.history {
		buf = append([]StreamEvent(nil), buf[len(buf)-b.history:]...)
	}
	b.buf[ev.OrderID] = buf
	b.mu.Unlock()

	b.deliverLocal(e)

	if b.rdb != nil {
		payload, err := json.Marshal(e)
		if err == nil {
			if err := b.rdb.Publish(ctx, channelPrefix+ev.OrderID.String(), payload).Err(); err != nil {
				b.log.Warn("sse redis publish degraded; in-process delivery only",
					zap.String("order_id", ev.OrderID.String()), zap.Error(err))
			}
		}
	}
	return nil
}

// SubscribeOrder ports.SSEBroker 冻结签名: 包装富订阅 (afterSeq=0 全量回放).
func (b *Broker) SubscribeOrder(ctx context.Context, orderID uuid.UUID) (<-chan ports.OrderEvent, func(), error) {
	envCh, cancel, err := b.Subscribe(ctx, orderID, 0)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan ports.OrderEvent, subBuf)
	go func() {
		defer close(out)
		for e := range envCh {
			select {
			case out <- e.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, cancel, nil
}

// ---------------------------------------------------------------------------
// 富订阅 (api SSE handler 专用): Last-Event-ID 回放 + 实时推送
// ---------------------------------------------------------------------------

// Subscribe 订阅单订单: 先回放缓冲内 seq > afterSeq 的事件, 再顺序转发实时事件;
// 缓冲溢出即关闭通道 (客户端走 retry+snapshot 重同步).
func (b *Broker) Subscribe(ctx context.Context, orderID uuid.UUID, afterSeq uint64) (<-chan StreamEvent, func(), error) {
	if b.stopped.Load() {
		return nil, nil, ports.ErrUnavailable
	}
	out := make(chan StreamEvent, subBuf)
	live := make(chan StreamEvent, subBuf)

	b.mu.Lock()
	var replay []StreamEvent
	for _, e := range b.buf[orderID] {
		if e.Seq > afterSeq {
			replay = append(replay, e)
		}
	}
	set, ok := b.subs[orderID]
	if !ok {
		set = make(map[chan StreamEvent]struct{})
		b.subs[orderID] = set
	}
	set[live] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if set, ok := b.subs[orderID]; ok {
			if _, reg := set[live]; reg {
				delete(set, live)
				close(live)
			}
			if len(set) == 0 {
				delete(b.subs, orderID)
			}
		}
		b.mu.Unlock()
	}

	go func() {
		defer close(out)
		defer cancel()
		for _, e := range replay {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			case <-b.stopCh:
				return
			}
		}
		for {
			select {
			case e, open := <-live:
				if !open {
					return
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				case <-b.stopCh:
					return
				}
			case <-ctx.Done():
				return
			case <-b.stopCh:
				return
			}
		}
	}()
	return out, cancel, nil
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// deliverLocal 非阻塞投递进程内订阅者; 通道溢出即摘除该订阅
// (客户端重连后由缓冲回放补齐 — 保序语义不变).
func (b *Broker) deliverLocal(e StreamEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.subs[e.Event.OrderID]
	if !ok {
		return
	}
	for ch := range set {
		select {
		case ch <- e:
		default:
			delete(set, ch)
			close(ch)
			b.log.Warn("sse subscriber channel overflow; dropping subscription",
				zap.String("order_id", e.Event.OrderID.String()))
		}
	}
	if len(set) == 0 {
		delete(b.subs, e.Event.OrderID)
	}
}

// dispatcher Redis Pub/Sub 收信协程: PSUBSCRIBE 全部订单频道,
// 自动重连 (指数回退 200ms..5s); 消息按 channel 后缀解析 orderID 派发.
func (b *Broker) dispatcher() {
	defer b.wg.Done()
	backoff := 200 * time.Millisecond
	for {
		if b.stopped.Load() {
			return
		}
		if err := b.dispatchOnce(); err != nil && !b.stopped.Load() {
			b.log.Warn("sse redis dispatcher reconnecting", zap.Error(err))
		}
		select {
		case <-b.stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (b *Broker) dispatchOnce() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-b.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	ps := b.rdb.PSubscribe(ctx, channelPrefix+"*")
	defer ps.Close()
	ch := ps.Channel()
	for msg := range ch {
		oidStr := msg.Channel[len(channelPrefix):]
		oid, err := uuid.Parse(oidStr)
		if err != nil {
			continue
		}
		var e StreamEvent
		if err := json.Unmarshal([]byte(msg.Payload), &e); err != nil {
			b.log.Warn("sse undecodable pubsub payload", zap.Error(err))
			continue
		}
		if b.ingestRemote(oid, e) {
			b.deliverLocal(e)
		}
	}
	return ctx.Err()
}

// allocSeq 分配单订单事件 seq: rdb 非 nil 时经 INCR 取全局单调值
// (跨实例 Last-Event-ID 语义一致); INCR 失败降级高位段本地分配 —
// (Src,Seq) 去重保证降级期事件不丢; rdb 为 nil 时纯本地计数.
func (b *Broker) allocSeq(ctx context.Context, oid uuid.UUID) uint64 {
	if b.rdb != nil {
		if n, err := b.rdb.Incr(ctx, seqKeyPrefix+oid.String()).Result(); err == nil {
			return uint64(n)
		}
		b.log.Warn("sse seq INCR degraded; instance-local high-range seq",
			zap.String("order_id", oid.String()))
		b.mu.Lock()
		defer b.mu.Unlock()
		b.seq[oid]++
		return degradedSeqBase + b.seq[oid]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq[oid]++
	return b.seq[oid]
}

// ingestRemote 远端事件落本地缓冲供回放并返回是否为新事件
// (去重键 (Src,Seq): 同实例回声与重复投递被滤除, 跨实例同 seq
// 的不同事件不再互吞 — FIX-D4). 不递增本地 seq; 仅抬升水位
// 保证本实例本地分配不复用已见序号.
func (b *Broker) ingestRemote(oid uuid.UUID, e StreamEvent) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Seq > b.seq[oid] && e.Seq < degradedSeqBase {
		b.seq[oid] = e.Seq
	}
	for _, existing := range b.buf[oid] {
		if existing.Seq == e.Seq && existing.Src == e.Src {
			return false
		}
	}
	buf := append(b.buf[oid], e)
	if len(buf) > b.history {
		buf = append([]StreamEvent(nil), buf[len(buf)-b.history:]...)
	}
	b.buf[oid] = buf
	return true
}

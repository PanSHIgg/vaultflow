package sse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

func newTestBroker(t *testing.T) *Broker {
	t.Helper()
	b := NewBroker(nil, nil)
	t.Cleanup(b.Close)
	return b
}

func ev(orderID uuid.UUID, typ ports.OrderEventType) ports.OrderEvent {
	return ports.OrderEvent{
		OrderID: orderID, Type: typ,
		Status: types.OrderAwaitingPayment, OccurredAt: time.Now().UTC(),
	}
}

func TestPublishSubscribe_InProcess(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, stop, err := b.Subscribe(ctx, oid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if err := b.PublishOrderEvent(ctx, ev(oid, ports.OrderEventDepositDetected)); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.Seq != 1 {
			t.Fatalf("seq=%d want 1", e.Seq)
		}
		if e.Event.Type != ports.OrderEventDepositDetected || e.Event.OrderID != oid {
			t.Fatalf("bad event %+v", e.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestReplay_AfterSeq(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := b.PublishOrderEvent(ctx, ev(oid, ports.OrderEventConfirmationUpdate)); err != nil {
			t.Fatal(err)
		}
	}

	ctxSub, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, stop, err := b.Subscribe(ctxSub, oid, 1) // Last-Event-ID=1 -> 回放 2,3
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	var seqs []uint64
	timeout := time.After(2 * time.Second)
	for len(seqs) < 2 {
		select {
		case e := <-ch:
			seqs = append(seqs, e.Seq)
		case <-timeout:
			t.Fatal("replay incomplete")
		}
	}
	if len(seqs) != 2 || seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("replayed seqs=%v want [2 3]", seqs)
	}
}

func TestPortAdapter_SubscribeOrder(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, stop, err := b.SubscribeOrder(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if err := b.PublishOrderEvent(ctx, ev(oid, ports.OrderEventStatusChanged)); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.Type != ports.OrderEventStatusChanged {
			t.Fatalf("type=%s", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestPerOrderSeqIsolation(t *testing.T) {
	b := newTestBroker(t)
	a, c := uuid.New(), uuid.New()
	ctx := context.Background()
	_ = b.PublishOrderEvent(ctx, ev(a, ports.OrderEventDepositDetected))
	_ = b.PublishOrderEvent(ctx, ev(c, ports.OrderEventDepositDetected))
	_ = b.PublishOrderEvent(ctx, ev(a, ports.OrderEventDepositDetected))

	ctxSub, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, stop, err := b.Subscribe(ctxSub, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	got := map[uint64]bool{}
	for i := 0; i < 2; i++ {
		e := <-ch
		got[e.Seq] = true
	}
	if !got[1] || !got[2] {
		t.Fatalf("seqs=%v want {1,2}", got)
	}
}

func TestClose_ClosesSubscriptions(t *testing.T) {
	b := NewBroker(nil, nil)
	oid := uuid.New()
	ctx := context.Background()
	ch, _, err := b.Subscribe(ctx, oid, 0)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel still open after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after broker Close")
	}
	if _, _, err := b.Subscribe(ctx, oid, 0); !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("subscribe after close: err=%v want ErrUnavailable", err)
	}
}

func TestOverflowSubscriberDropped(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, stop, err := b.Subscribe(ctx, oid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// 不读 channel 连续发布: out(128) 填满后 pump 阻塞, live(128) 再填满
	// 即触发摘除; 发 3*subBuf 保证必溢出.
	for i := 0; i < subBuf*3; i++ {
		if err := b.PublishOrderEvent(ctx, ev(oid, ports.OrderEventConfirmationUpdate)); err != nil {
			t.Fatal(err)
		}
	}
	closed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !closed {
		select {
		case _, open := <-live:
			if !open {
				closed = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !closed {
		t.Fatal("overflowed subscriber should be dropped")
	}
}

// ===========================================================================
// FIX-D4 回归: 跨实例事件身份 (Src,Seq) 去重 — seq 仅本实例单调,
// 实例间撞号不得互吞.
// ===========================================================================

func TestIngestRemote_DedupBySrcAndSeq(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()

	// 实例 A 事件 seq=1 -> 新事件落缓冲.
	if !b.ingestRemote(oid, StreamEvent{Seq: 1, Src: "inst-A", Event: ev(oid, ports.OrderEventDepositDetected)}) {
		t.Fatal("first remote event must be accepted")
	}
	// 同 (Src,Seq) 重复投递 -> 滤除 (回声/重发).
	if b.ingestRemote(oid, StreamEvent{Seq: 1, Src: "inst-A", Event: ev(oid, ports.OrderEventDepositDetected)}) {
		t.Fatal("echo/duplicate (inst-A,1) must be dropped")
	}
	// FIX-D4 核心: 实例 B 的 seq=1 是不同事件, 不得被当回声丢.
	if !b.ingestRemote(oid, StreamEvent{Seq: 1, Src: "inst-B", Event: ev(oid, ports.OrderEventConfirmationUpdate)}) {
		t.Fatal("distinct-source (inst-B,1) must NOT be dropped — seq is per-instance")
	}
	// 实例 A seq=2 -> 新.
	if !b.ingestRemote(oid, StreamEvent{Seq: 2, Src: "inst-A", Event: ev(oid, ports.OrderEventStatusChanged)}) {
		t.Fatal("(inst-A,2) must be accepted")
	}
	if len(b.buf[oid]) != 3 {
		t.Fatalf("buffer = %d events, want 3", len(b.buf[oid]))
	}
}

func TestReplay_MixedSourceOrdering(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 跨实例缓冲: inst-A seq1, inst-B seq1, inst-A seq2 (到达顺序).
	b.ingestRemote(oid, StreamEvent{Seq: 1, Src: "inst-A", Event: ev(oid, ports.OrderEventDepositDetected)})
	b.ingestRemote(oid, StreamEvent{Seq: 1, Src: "inst-B", Event: ev(oid, ports.OrderEventConfirmationUpdate)})
	b.ingestRemote(oid, StreamEvent{Seq: 2, Src: "inst-A", Event: ev(oid, ports.OrderEventStatusChanged)})

	// Last-Event-ID=1 (语义 = 全局 seq 水位): 只回放 seq>1 的 inst-A seq2.
	ch, stop, err := b.Subscribe(ctx, oid, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case e := <-ch:
		if e.Seq != 2 || e.Src != "inst-A" {
			t.Fatalf("replayed %+v, want (inst-A,2)", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replay missing")
	}
}

func TestIngestRemote_DegradedSeqDoesNotRaiseWatermark(t *testing.T) {
	b := newTestBroker(t)
	oid := uuid.New()
	// 高位降级段事件 (INCR 失效时实例本地分配) 只落缓冲, 不得抬升
	// 低位水位 — 否则恢复后远端低位 seq 会被 seq[oid] 误判.
	b.ingestRemote(oid, StreamEvent{Seq: degradedSeqBase + 5, Src: "inst-A",
		Event: ev(oid, ports.OrderEventDepositDetected)})
	if b.seq[oid] != 0 {
		t.Fatalf("degraded seq raised watermark: seq[%s]=%d", oid, b.seq[oid])
	}
}

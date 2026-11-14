package order

// FIX-G BUG-2 回归: order.Service 人工/周期跃迁路径的提交后 SSE 发布.
//
//   - ReevaluateExpiredOrders: AWAITING_PAYMENT -> EXPIRED 发 expired 帧;
//   - ReviewOrder RESOLVE_REVERSAL: -> REVERSAL_RESOLVED 发 reversal_resolved;
//   - 仅 commit 后发布 — fake broker 发布时点独立连接回读已提交状态作证据.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

type fixgOrderSSE struct {
	pool       *pgxpool.Pool
	mu         sync.Mutex
	evs        []ports.OrderEvent
	orderAtPub []string
}

func (f *fixgOrderSSE) PublishOrderEvent(ctx context.Context, ev ports.OrderEvent) error {
	var st string
	_ = f.pool.QueryRow(ctx,
		`SELECT status FROM payment_orders WHERE id=$1`, ev.OrderID).Scan(&st)
	f.mu.Lock()
	f.evs = append(f.evs, ev)
	f.orderAtPub = append(f.orderAtPub, st)
	f.mu.Unlock()
	return nil
}

func (f *fixgOrderSSE) SubscribeOrder(ctx context.Context, orderID uuid.UUID) (<-chan ports.OrderEvent, func(), error) {
	return nil, func() {}, nil
}

func (f *fixgOrderSSE) snapshot() ([]ports.OrderEvent, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.OrderEvent(nil), f.evs...), append([]string(nil), f.orderAtPub...)
}

func TestOrderSinkExpiredAndReviewEvents(t *testing.T) {
	svc, pool := newService(t)
	rec := &fixgOrderSSE{pool: pool}
	svc.SetOrderEventSink(rec)
	ctx := context.Background()

	// --- 周期过期: AWAITING_PAYMENT -> EXPIRED -> expired 帧 ---
	o := createTestOrder(t, svc, "sse-expire", "500", time.Now().Add(-time.Hour))
	n, err := svc.ReevaluateExpiredOrders(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated %d want 1", n)
	}
	evs, pub := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events %d want 1: %+v", len(evs), evs)
	}
	if evs[0].Type != ports.OrderEventExpired || evs[0].OrderID != o.ID ||
		evs[0].Status != types.OrderExpired {
		t.Fatalf("expired event %+v", evs[0])
	}
	if pub[0] != "EXPIRED" {
		t.Fatalf("expired published pre-commit: committed=%q", pub[0])
	}

	// --- 人工审核: SETTLED_REVERSED -> REVERSAL_RESOLVED -> reversal_resolved ---
	o2 := createTestOrder(t, svc, "sse-review", "700", time.Now().Add(time.Hour))
	if _, err := pool.Exec(ctx,
		`UPDATE payment_orders SET status='SETTLED_REVERSED', received_amount=700 WHERE id=$1`,
		o2.ID); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ReviewOrder(ctx, ports.ReviewOrderRequest{
		OrderID: o2.ID, Action: ports.ReviewResolveReversal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != types.OrderReversalResolved {
		t.Fatalf("review outcome %s", out.Status)
	}
	evs, pub = rec.snapshot()
	if len(evs) != 2 {
		t.Fatalf("events %d want 2: %+v", len(evs), evs)
	}
	if evs[1].Type != ports.OrderEventStatusChanged || evs[1].OrderID != o2.ID ||
		evs[1].Status != types.OrderReversalResolved {
		t.Fatalf("review event %+v", evs[1])
	}
	if pub[1] != "REVERSAL_RESOLVED" {
		t.Fatalf("review event published pre-commit: committed=%q", pub[1])
	}
}

// 未注入 broker 时全部路径必须 no-op (生产降级).
func TestOrderSinkNilNoop(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	o := createTestOrder(t, svc, "sse-nil", "300", time.Now().Add(-time.Hour))
	if _, err := svc.ReevaluateExpiredOrders(ctx, time.Now(), 10); err != nil {
		t.Fatal(err)
	}
	var st string
	if err := svc.pool.QueryRow(ctx,
		`SELECT status FROM payment_orders WHERE id=$1`, o.ID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "EXPIRED" {
		t.Fatalf("status %s want EXPIRED", st)
	}
}

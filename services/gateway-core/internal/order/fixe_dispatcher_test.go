package order

// FIX-E/E2 回归 (PG 集成): QueueIntent 成功 -> OnPayoutInFlight 窗口内
// CancelPayout 竞速获胜 — Dispatcher 必须对已建意图调用 RequestCancel,
// payout 保持 CANCELLED_UNLOCKED, 不抛错不阻塞.

import (
	"context"
	"fmt"
	"testing"

	"vaultflow/gateway-core/internal/types"
)

func TestPayoutDispatcher_CancelRaceCancelsIntent(t *testing.T) {
	s, pool, fake, d := newDispatcher(t)
	ctx := context.Background()
	p := seedRefundPayout(t, s, pool, "60")

	// 竞速钩子: 意图已排队、业务行尚未流转的瞬间, 取消获胜.
	fake.onQueue = func() {
		if _, err := s.CancelPayout(ctx, p.ID); err != nil {
			panic(fmt.Sprintf("race cancel failed: %v", err))
		}
	}
	n, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce must treat lost-race as benign: %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched = %d, want 1 (queue succeeded, transition lost the race)", n)
	}
	if len(fake.queued) != 1 {
		t.Fatalf("QueueIntent calls = %d, want 1", len(fake.queued))
	}
	if len(fake.cancels) != 1 {
		t.Fatalf("RequestCancel calls = %d, want 1 — created intent must be cancelled", len(fake.cancels))
	}
	var st string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM merchant_payouts WHERE id=$1`, p.ID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if types.PayoutStatus(st) != types.PayoutCancelledUnlocked {
		t.Fatalf("payout %s want CANCELLED_UNLOCKED, got %s", p.ID, st)
	}
}

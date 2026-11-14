package types

// FIX-E/E4 回归: Go 侧状态常量与迁移白名单对齐断言.
//   - outbound_tx_intents.status 'REJECTED_BY_SIGNER' (002_intent_payload.sql);
//   - sweep_batch_items.item_status 'RELEASED' (004_sweep_release.sql).

import "testing"

func TestFixEStatusConstantsAlignWithMigrations(t *testing.T) {
	if string(IntentRejectedBySigner) != "REJECTED_BY_SIGNER" {
		t.Fatalf("IntentRejectedBySigner=%q want REJECTED_BY_SIGNER", IntentRejectedBySigner)
	}
	if string(SweepItemReleased) != "RELEASED" {
		t.Fatalf("SweepItemReleased=%q want RELEASED", SweepItemReleased)
	}

	// REJECTED_BY_SIGNER 为显式终态 — 白名单无任何出边.
	for _, to := range []TxIntentStatus{
		IntentQueued, IntentInFlight, IntentIncludedPendingSafe, IntentConfirmed,
		IntentFailedReverted, IntentCancelledByFiller, IntentNonceStolenHalt,
		IntentRejectedBySigner} {
		if IntentRejectedBySigner.CanTransition(to) {
			t.Fatalf("REJECTED_BY_SIGNER -> %s must be forbidden (terminal)", to)
		}
	}

	// PENDING -> RELEASED 为 B2 reaper 合法边; RELEASED 自身终态无出边.
	if !SweepItemPending.CanTransition(SweepItemReleased) {
		t.Fatal("SweepItemPending -> SweepItemReleased must be allowed (004 reaper)")
	}
	for _, to := range []SweepItemStatus{
		SweepItemPending, SweepItemSwept, SweepItemQuarantinedRevert, SweepItemReleased} {
		if SweepItemReleased.CanTransition(to) {
			t.Fatalf("RELEASED -> %s must be forbidden (terminal)", to)
		}
	}
}

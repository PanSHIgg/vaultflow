package api

// FIX-E/E4 回归: 意图状态 API 过滤白名单纳入 REJECTED_BY_SIGNER
// (002_intent_payload.sql 入库值, 监控台按状态过滤须能选中该终态).

import (
	"testing"

	"vaultflow/gateway-core/internal/types"
)

func TestFixEIntentStatusFilterAcceptsRejectedBySigner(t *testing.T) {
	if !intentStatusValid(types.IntentRejectedBySigner) {
		t.Fatal("intentStatusValid must accept REJECTED_BY_SIGNER")
	}
	if !intentStatusValid(types.TxIntentStatus("REJECTED_BY_SIGNER")) {
		t.Fatal("raw string filter REJECTED_BY_SIGNER rejected")
	}
	// 既有 7 态不受影响.
	for _, s := range []types.TxIntentStatus{
		types.IntentQueued, types.IntentInFlight, types.IntentIncludedPendingSafe,
		types.IntentConfirmed, types.IntentFailedReverted,
		types.IntentCancelledByFiller, types.IntentNonceStolenHalt} {
		if !intentStatusValid(s) {
			t.Fatalf("existing status %s dropped from whitelist", s)
		}
	}
	if intentStatusValid(types.TxIntentStatus("BOGUS")) {
		t.Fatal("unknown status must stay rejected")
	}
}

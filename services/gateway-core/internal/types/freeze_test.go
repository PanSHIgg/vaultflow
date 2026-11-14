package types

import "testing"

// Phase 0 契约冻结自检: 枚举数量与 §5/§6 白名单迁移表钉死, 防 Wave-1 接口漂移.

func TestFrozenEnumCounts(t *testing.T) {
	if got := len(AllLedgerAccountCodes()); got != 14 {
		t.Fatalf("ledger account codes = %d, want 14", got)
	}
	debit, credit := 0, 0
	for _, a := range AllLedgerAccountCodes() {
		if !a.IsValid() {
			t.Fatalf("account %s not in normal-side map", a)
		}
		if a.NormalSide() == DirectionDebit {
			debit++
		} else {
			credit++
		}
	}
	if debit != 6 || credit != 8 {
		t.Fatalf("normal side split = %d/%d, want 6 DEBIT / 8 CREDIT", debit, credit)
	}
	if got := len(AllEntryTemplateCodes()); got != 9 {
		t.Fatalf("entry template codes = %d, want 9", got)
	}
	if got := len(PayoutSourceAccounts); got != 4 {
		t.Fatalf("payout source accounts = %d, want 4", got)
	}
	if got := len(SuspenseLiabilityAccounts); got != 3 {
		t.Fatalf("suspense liability accounts = %d, want 3", got)
	}
}

func TestDepositMachine(t *testing.T) {
	allowed := [][2]DepositStatus{
		{DepositDetected, DepositFinalized},
		{DepositDetected, DepositOrphanedShallow},
		{DepositFinalized, DepositOrphanedDeep},
	}
	for _, p := range allowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("deposit %s -> %s should be allowed", p[0], p[1])
		}
	}
	if DepositFinalized.CanTransition(DepositOrphanedShallow) {
		t.Error("FINALIZED -> ORPHANED_SHALLOW must be denied (deep rollback goes ORPHANED_DEEP)")
	}
	if DepositOrphanedShallow.CanTransition(DepositDetected) {
		t.Error("orphaned record must not resurrect (new fork block re-inserts as new row)")
	}
}

func TestOrderMachine(t *testing.T) {
	allowed := [][2]OrderStatus{
		{OrderAwaitingPayment, OrderPartiallyPaid},
		{OrderAwaitingPayment, OrderPaid},
		{OrderAwaitingPayment, OrderOverpaid},
		{OrderAwaitingPayment, OrderExpired},
		{OrderAwaitingPayment, OrderLatePaidReview},
		{OrderPartiallyPaid, OrderPartiallyPaid},
		{OrderPartiallyPaid, OrderAwaitingPayment},
		{OrderPartiallyPaid, OrderPaid},
		{OrderPartiallyPaid, OrderOverpaid},
		{OrderPartiallyPaid, OrderUnderpaidExpiredReview},
		{OrderPartiallyPaid, OrderLatePaidReview},
		{OrderExpired, OrderLatePaidReview},
		{OrderUnderpaidExpiredReview, OrderLatePaidReview},
		{OrderUnderpaidExpiredReview, OrderExpired},
		{OrderUnderpaidExpiredReview, OrderPaid},
		{OrderUnderpaidExpiredReview, OrderRefundInProgress},
		{OrderLatePaidReview, OrderExpired},
		{OrderLatePaidReview, OrderUnderpaidExpiredReview},
		{OrderLatePaidReview, OrderPaid},
		{OrderLatePaidReview, OrderRefundInProgress},
		{OrderRefundInProgress, OrderRefunded},
		{OrderRefundInProgress, OrderLatePaidReview},
		{OrderOverpaid, OrderPaid},
		{OrderPaid, OrderSettledReversed},
		{OrderOverpaid, OrderSettledReversed},
		{OrderSettledReversed, OrderReversalResolved},
	}
	if len(allowed) != 26 {
		t.Fatalf("order transition whitelist = %d edges, want 26 per §5.2", len(allowed))
	}
	for _, p := range allowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("order %s -> %s should be allowed", p[0], p[1])
		}
	}
	denied := [][2]OrderStatus{
		{OrderAwaitingPayment, OrderRefunded},
		{OrderPaid, OrderAwaitingPayment},
		{OrderRefunded, OrderSettledReversed},
		{OrderReversalResolved, OrderPaid},
		{OrderExpired, OrderPaid},
	}
	for _, p := range denied {
		if p[0].CanTransition(p[1]) {
			t.Errorf("order %s -> %s must be denied", p[0], p[1])
		}
	}
	for _, s := range []OrderStatus{OrderPaid, OrderOverpaid, OrderRefunded, OrderReversalResolved} {
		if !s.IsBusinessTerminal() {
			t.Errorf("order %s should be business-terminal (§4.1 分支 C)", s)
		}
	}
	if OrderExpired.IsBusinessTerminal() {
		t.Error("EXPIRED is not business-terminal: late payment goes LATE_PAID_REVIEW")
	}
}

func TestIntentAndAttemptMachines(t *testing.T) {
	allowed := [][2]TxIntentStatus{
		{IntentQueued, IntentInFlight},
		{IntentQueued, IntentNonceStolenHalt},
		{IntentInFlight, IntentInFlight},
		{IntentInFlight, IntentNonceStolenHalt},
		{IntentInFlight, IntentIncludedPendingSafe},
		{IntentIncludedPendingSafe, IntentInFlight},
		{IntentIncludedPendingSafe, IntentConfirmed},
		{IntentIncludedPendingSafe, IntentCancelledByFiller},
		{IntentIncludedPendingSafe, IntentFailedReverted},
		{IntentConfirmed, IntentInFlight},
		{IntentFailedReverted, IntentInFlight},
		{IntentCancelledByFiller, IntentInFlight},
	}
	for _, p := range allowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("intent %s -> %s should be allowed", p[0], p[1])
		}
	}
	if IntentNonceStolenHalt.CanTransition(IntentInFlight) {
		t.Error("NONCE_STOLEN_HALT must stay halted (人工处置, 无自动出边)")
	}
	attemptAllowed := [][2]TxAttemptStatus{
		{AttemptBroadcasted, AttemptIncludedUnsafe},
		{AttemptBroadcasted, AttemptSuperseded},
		{AttemptBroadcasted, AttemptDropped},
		{AttemptIncludedUnsafe, AttemptFinalized},
		{AttemptIncludedUnsafe, AttemptBroadcasted},
		{AttemptFinalized, AttemptBroadcasted},
		{AttemptSuperseded, AttemptBroadcasted},
	}
	for _, p := range attemptAllowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("attempt %s -> %s should be allowed", p[0], p[1])
		}
	}
}

func TestPayoutMachine(t *testing.T) {
	allowed := [][2]PayoutStatus{
		{PayoutLockedQueued, PayoutInFlight},
		{PayoutLockedQueued, PayoutCancelledUnlocked},
		{PayoutInFlight, PayoutCompleted},
		{PayoutInFlight, PayoutFailedRetryable},
		{PayoutFailedRetryable, PayoutInFlight},
		{PayoutFailedRetryable, PayoutCancelledUnlocked},
		{PayoutCompleted, PayoutInFlight},
	}
	for _, p := range allowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("payout %s -> %s should be allowed", p[0], p[1])
		}
	}
	if PayoutCancelledUnlocked.CanTransition(PayoutInFlight) {
		t.Error("CANCELLED_UNLOCKED is terminal")
	}
}

func TestTemplateLegs(t *testing.T) {
	cases := []struct {
		tpl           EntryTemplateCode
		debit, credit LedgerAccountCode
		want          bool
	}{
		{TemplateInternalTreasuryRebalance, AccountCustodyHotPayout, AccountCustodyCold, true},
		{TemplateDepositFinalized, AccountCustodyUnswept, AccountOrderEscrowLiab, true},
		{TemplateDepositFinalized, AccountCustodyUnswept, AccountSuspenseUnmatchedLiab, true},
		{TemplateOrderSettle, AccountOrderEscrowLiab, AccountMerchantAvailable, true},
		{TemplateOrderSettle, AccountSuspenseRefundableLiab, AccountMerchantAvailable, true},
		{TemplateOrderSettle, AccountSuspenseRefundableLiab, AccountSuspenseOverpayLiab, false},
		{TemplateGasSettle, AccountGasExpense, AccountHotWalletNative, true},
		{TemplateSweepSettle, AccountCustodyCold, AccountCustodyUnswept, true},
		{TemplatePayoutFlow, AccountMerchantAvailable, AccountPayoutLockedLiab, true},
		{TemplatePayoutFlow, AccountPayoutLockedLiab, AccountCustodyHotPayout, true},
		{TemplatePayoutFlow, AccountPayoutLockedLiab, AccountSuspenseRefundableLiab, true},
		{TemplateReorgReversal, AccountMerchantReorgReceivable, AccountCustodyUnswept, true},
		{TemplateReorgReversal, AccountCustodyUnswept, AccountCustodyCold, true},
		{TemplateTreasuryTopup, AccountHotWalletNative, AccountTreasuryEquityCapital, true},
		{TemplateTreasuryTopup, AccountOrderEscrowLiab, AccountTreasuryEquityCapital, false},
	}
	for _, c := range cases {
		if got := c.tpl.Allows(c.debit, c.credit); got != c.want {
			t.Errorf("%s.Allows(%s,%s) = %v, want %v", c.tpl, c.debit, c.credit, got, c.want)
		}
	}
}

func TestU256(t *testing.T) {
	u, err := ParseU256("100000000")
	if err != nil {
		t.Fatal(err)
	}
	if fee := u.MulBpsFloor(25); fee.String() != "250000" {
		t.Fatalf("fee = %s, want 250000 (floor)", fee)
	}
	if _, err := ParseU256("-5"); err == nil {
		t.Error("negative parse must fail")
	}
	if _, err := u.Sub(MustParseU256("100000001")); err != ErrU256Negative {
		t.Fatalf("sub past zero = %v, want ErrU256Negative", err)
	}
	var zero U256
	if zero.String() != "0" || !zero.IsZero() {
		t.Error("zero value must be safe and equal 0")
	}
	b, _ := zero.MarshalText()
	if string(b) != "0" {
		t.Error("zero MarshalText = ", string(b))
	}
}

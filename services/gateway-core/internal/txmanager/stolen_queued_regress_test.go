package txmanager

// QUEUED 首播 nonce 带外消耗熔断回归测试 (Wave-3 e2e s2-quarantine-gas 暴露的缺陷):
// nonce 在分配至首播之间被私钥带外消耗时, SendRawTx 持续撞 "nonce too low",
// 而 WatchdogTick 只扫 IN_FLIGHT — 原实现 §4.2④ NONCE_STOLEN_HALT 对 QUEUED
// 意图永不触发, 队列死循环重试. signBroadcastPersist 现于广播失败分支对
// QUEUED 意图做 latest-nonce 探针等价判定 (零 attempt 行即"非本系统消耗"
// 证明), IN_FLIGHT 重发路径仍归巡检 Receipt provedEmpty 唯一裁定.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ① QUEUED 意图 + SendRawTx "nonce too low" + latest nonce 已越过 K →
// NONCE_STOLEN_HALT + CRITICAL 告警 + 零 attempt 行 + 钱包队列熔断.
func TestQueuedNonceStolenHaltsOnBroadcastFailure(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	var alerts []Alert
	m := New(db, fc, fs, Config{}, nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	it2, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	// 分配 nonce=K 至首播之间私钥带外消费该 Nonce — 链上 latest 前进到 K+1,
	// 节点对任何 nonce<=K 的候选一律 "nonce too low".
	fc.latestNonce = it.Nonce + 1
	fc.sendErr = errors.New("nonce too low")

	n, err := m.BroadcastQueued(ctx)
	if err != nil || n != 2 {
		t.Fatalf("broadcast n=%d err=%v", n, err)
	}
	got, err := m.GetIntent(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.IntentNonceStolenHalt {
		t.Fatalf("intent status=%s want NONCE_STOLEN_HALT", got.Status)
	}
	atts, err := m.ListAttempts(ctx, it.ID)
	if err != nil || len(atts) != 0 {
		t.Fatalf("QUEUED halt must leave zero attempt rows, got %+v err=%v", atts, err)
	}
	var crit bool
	for _, a := range alerts {
		if a.Code == AlertCodeNonceStolen && a.Severity == "CRITICAL" {
			crit = true
		}
	}
	if !crit {
		t.Fatalf("missing CRITICAL %s alert: %+v", AlertCodeNonceStolen, alerts)
	}
	// 熔断唯一事实源是 DB 行 — broadcastIntent.checkHalted 挡住同钱包其余
	// QUEUED 意图 (it2 保持悬挂, 不再消费签名/广播资源).
	got2, err := m.GetIntent(ctx, it2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != types.IntentQueued {
		t.Fatalf("queued peer intent status=%s want QUEUED (halted wallet must not dispatch)",
			got2.Status)
	}
	// QueueIntent.checkHalted 拒绝同钱包新意图.
	if _, err := m.QueueIntent(ctx, queueReq(uuid.New())); !errors.Is(err, ports.ErrNonceHalted) {
		t.Fatalf("want ErrNonceHalted, got %v", err)
	}
}

// ② QUEUED 首播瞬时错误 + latest nonce 未越过 K → 保持 QUEUED 常规重试,
// last_error 透出失败原因, 不误熔断.
func TestQueuedBroadcastTransientFailureRetries(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	var alerts []Alert
	m := New(db, fc, fs, Config{}, nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	fc.sendErr = errors.New("connection refused") // latest nonce 仍为 K — 未被消费
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetIntent(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.IntentQueued {
		t.Fatalf("intent status=%s want QUEUED (transient retry preserved)", got.Status)
	}
	var le *string
	if err := db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, it.ID).Scan(&le); err != nil {
		t.Fatal(err)
	}
	if le == nil || !strings.Contains(*le, "connection refused") {
		t.Fatalf("last_error=%v want transient broadcast error recorded", le)
	}
	for _, a := range alerts {
		if a.Code == AlertCodeNonceStolen {
			t.Fatalf("transient failure must not raise %s: %+v", AlertCodeNonceStolen, alerts)
		}
	}
	// 故障恢复后同一 Nonce 正常首播 — 重试语义完整保留.
	fc.sendErr = nil
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status=%s want IN_FLIGHT after recovery", got.Status)
	}
}

// ③ QUEUED 首播失败且 latest-nonce 探针本身出错 → 证据不足 fail-open,
// 保持 QUEUED 不熔断 (与 watchdog inconclusive 哲学一致).
func TestQueuedNonceProbeErrorFailsOpen(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	var alerts []Alert
	m := New(db, fc, fs, Config{}, nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	fc.sendErr = errors.New("nonce too low")
	fc.nonceErr = errors.New("rpc flaky") // 探针无法给出确定证据
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetIntent(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.IntentQueued {
		t.Fatalf("intent status=%s want QUEUED — inconclusive probe must not halt", got.Status)
	}
	var le *string
	if err := db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, it.ID).Scan(&le); err != nil {
		t.Fatal(err)
	}
	if le == nil || !strings.Contains(*le, "nonce too low") {
		t.Fatalf("last_error=%v want broadcast failure recorded", le)
	}
	for _, a := range alerts {
		if a.Code == AlertCodeNonceStolen {
			t.Fatalf("probe error must not raise %s: %+v", AlertCodeNonceStolen, alerts)
		}
	}
}

// ④ 负向钉死: IN_FLIGHT 重发路径的 SendRawTx 失败绝不走 QUEUED 探针自熔断
// — IN_FLIGHT 意图已有自有候选, "带外消耗 vs 自有入块"唯一归巡检
// inspectIntent 的 Receipt provedEmpty 证据裁定 (否则自有入块误触熔断).
func TestInFlightResendFailureNeverSelfHalts(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	var alerts []Alert
	m := New(db, fc, fs, Config{DroppedMinAge: time.Millisecond, InFlightTimeout: time.Hour},
		nil, func(a Alert) { alerts = append(alerts, a) })
	ctx := context.Background()

	it, err := m.QueueIntent(ctx, relayReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE outbound_tx_attempts SET broadcasted_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	// Mempool 驱逐场景 (latest<=K, 候选不可见, pending<=K, 已超龄) →
	// markDroppedResend → resendDropped 重签后 SendRawTx 失败 — 意图须保持
	// IN_FLIGHT 等下轮巡检, 不得借首播探针自熔断.
	fc.sendErr = errors.New("nonce too low")
	if err := m.WatchdogTick(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetIntent(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status=%s want IN_FLIGHT — resend failure must not self-halt",
			got.Status)
	}
	// 直接驱动 resendDropped 并把 latest nonce 抬到 K+1 (QUEUED 探针若误生效
	// 此条件必熔断) — 仍然不熔断, 证明门控严格限定 IntentQueued.
	fc.latestNonce = it.Nonce + 1
	if err := m.resendDropped(ctx, got); err == nil {
		t.Fatal("want resend broadcast error")
	}
	got, _ = m.GetIntent(ctx, it.ID)
	if got.Status != types.IntentInFlight {
		t.Fatalf("intent status=%s want IN_FLIGHT — nonce-stolen arbitration belongs to watchdog only",
			got.Status)
	}
	for _, a := range alerts {
		if a.Code == AlertCodeNonceStolen {
			t.Fatalf("IN_FLIGHT resend failure must not raise %s: %+v", AlertCodeNonceStolen, alerts)
		}
	}
}

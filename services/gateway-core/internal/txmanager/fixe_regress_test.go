package txmanager

// FIX-E 回归测试 (PG 集成; 未设 DSN 自动 skip):
//   E2 — RequestCancel 受理 QUEUED (标记 -> 首播 filler 化) +
//         broadcastIntent 对 CANCELLED_UNLOCKED payout 的业务复核;
//   E5 — 并发同引用不同载荷的 QueueIntent: 胜出意图的持久化 payload_*
//         是唯一签名权威, 输家内存参数绝不混挂.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// E2 ①: QUEUED 意图 RequestCancel 受理 -> 持久标记 -> 下一轮
// BroadcastQueued 以同 Nonce GAP_FILLER 填洞 (业务载荷不出签名器).
func TestRequestCancelQueuedIntentFillsNonce(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	it, err := m.QueueIntent(ctx, queueReq(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RequestCancel(ctx, it.ID); err != nil {
		t.Fatalf("RequestCancel must accept QUEUED intents (E2): %v", err)
	}
	var le *string
	if err := db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, it.ID).Scan(&le); err != nil {
		t.Fatal(err)
	}
	if le == nil || !strings.HasPrefix(*le, lastErrorPrefixCancelRequested) {
		t.Fatalf("cancel marker missing, last_error=%v", le)
	}
	n, err := m.BroadcastQueued(ctx)
	if err != nil || n != 1 {
		t.Fatalf("broadcast n=%d err=%v", n, err)
	}
	atts, err := m.ListAttempts(ctx, it.ID)
	if err != nil || len(atts) != 1 {
		t.Fatalf("attempts %+v err=%v", atts, err)
	}
	if atts[0].Kind != types.AttemptKindGapFiller || atts[0].Status != types.AttemptBroadcasted {
		t.Fatalf("attempt %+v want GAP_FILLER/BROADCASTED", atts[0])
	}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[0]); err != nil {
		t.Fatal(err)
	}
	if *tx.To() != [20]byte(wallet()) || tx.Gas() != 21_000 ||
		tx.Value().Sign() != 0 || len(tx.Data()) != 0 {
		t.Fatalf("filler tx to=%x gas=%d value=%s data=%x — business payload must never sign",
			tx.To(), tx.Gas(), tx.Value(), tx.Data())
	}
	if got, _ := m.GetIntent(ctx, it.ID); got.Status != types.IntentInFlight {
		t.Fatalf("intent %s want IN_FLIGHT (filler in transit)", got.Status)
	}
}

// E2 ②: QueueIntent→首播窗口内 CancelPayout 竞速获胜 (payout 已是
// CANCELLED_UNLOCKED) — broadcastIntent 业务复核后改发同 Nonce GAP_FILLER.
func TestBroadcastIntentCancelledPayoutGoesFiller(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	pid := seedPayout(t, db)
	if _, err := db.Exec(ctx,
		`UPDATE merchant_payouts SET status='CANCELLED_UNLOCKED' WHERE id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	it, err := m.QueueIntent(ctx, queueReq(pid))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	atts, err := m.ListAttempts(ctx, it.ID)
	if err != nil || len(atts) != 1 {
		t.Fatalf("attempts %+v err=%v", atts, err)
	}
	if atts[0].Kind != types.AttemptKindGapFiller {
		t.Fatalf("attempt kind=%s want GAP_FILLER — cancelled payout must not broadcast business transfer",
			atts[0].Kind)
	}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[0]); err != nil {
		t.Fatal(err)
	}
	if *tx.To() != [20]byte(wallet()) || tx.Gas() != 21_000 {
		t.Fatalf("business transfer leaked on-chain: to=%x gas=%d", tx.To(), tx.Gas())
	}
	// 取消原因持久标记 (sticky — 下次 tick 重放仍是 filler 语义).
	var le *string
	if err := db.QueryRow(ctx,
		`SELECT last_error FROM outbound_tx_intents WHERE id=$1`, it.ID).Scan(&le); err != nil {
		t.Fatal(err)
	}
	if le == nil || !strings.HasPrefix(*le, lastErrorPrefixCancelRequested) {
		t.Fatalf("cancel reason marker missing, last_error=%v", le)
	}
}

// E5: 并发同引用不同载荷的 QueueIntent — 胜出意图的持久化 payload_*
// 是唯一签名权威; 输家 retry 参数绝不混入 pendingParams fast-path.
func TestQueueIntentConcurrentDriftKeepsWinnerPayload(t *testing.T) {
	db := freshDB(t)
	fc := &fakeChain{present: map[types.Hash]bool{}}
	fs := &fakeSigner{}
	m := New(db, fc, fs, Config{}, nil, nil)
	ctx := context.Background()

	// 预占 nonce 游标 — 聚焦意图幂等竞速本身, 与首见游标初始化路径解耦.
	if _, err := db.Exec(ctx,
		`INSERT INTO wallet_nonce_cursors (chain_id, wallet_address, next_nonce)
		 VALUES (31337,$1,0)`, addrStr(wallet())); err != nil {
		t.Fatal(err)
	}

	ref := uuid.New()
	const racers = 32
	start := make(chan struct{})
	ids := make(chan uuid.UUID, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := queueReq(ref)
			req.Calldata = []byte{0xde, 0xad, byte(i >> 8), byte(i)} // 每个竞争者不同载荷
			<-start
			it, err := m.QueueIntent(ctx, req)
			if err != nil {
				t.Errorf("queue racer %d: %v", i, err)
				return
			}
			ids <- it.ID
		}(i)
	}
	close(start)
	wg.Wait()
	close(ids)
	winner := uuid.Nil
	for id := range ids {
		if winner == uuid.Nil {
			winner = id
		}
		if id != winner {
			t.Fatalf("concurrent same-reference produced two intents: %s vs %s", winner, id)
		}
	}
	if winner == uuid.Nil {
		t.Fatal("no intent created")
	}
	// 唯一权威: 持久化 payload_data; 若输家 pendingParams 混入, 首播将
	// 签出输家 calldata — 广播字解码后与持久列逐字节比对钉死语义.
	var persisted []byte
	if err := db.QueryRow(ctx,
		`SELECT payload_data FROM outbound_tx_intents WHERE id=$1`, winner).
		Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	// fast-path 一致性: 若挂载了 pendingParams, 其 calldata 必须与持久列一致.
	if v, ok := m.pendingParams.Load(winner); ok {
		req := v.(ports.QueueIntentRequest)
		if !bytes.Equal(req.Calldata, persisted) {
			t.Fatalf("pendingParams drifted: mem %x vs persisted %x", req.Calldata, persisted)
		}
	}
	if _, err := m.BroadcastQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("broadcasts=%d want 1", len(fc.sent))
	}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(fc.sent[0]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tx.Data(), persisted) {
		t.Fatalf("signed calldata %x != persisted winner payload %x — loser params leaked into signer",
			tx.Data(), persisted)
	}
}

// E5 单测锚点: attachParamsIfIdentical 仅在持久化载荷与请求逐字段一致时
// 才允许 fast-path 挂载 (不同载荷重试绝不覆写胜出意图参数).
func TestAttachParamsIfIdentical(t *testing.T) {
	db := freshDB(t)
	m := New(db, &fakeChain{}, &fakeSigner{}, Config{}, nil, nil)
	ctx := context.Background()

	ref := uuid.New()
	req := queueReq(ref)
	it, err := m.QueueIntent(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	m.pendingParams.Delete(it.ID) // 模拟重启后空 fast-path

	// 异载荷 -> 不挂载 + 持久列保持原值.
	drifted := queueReq(ref)
	drifted.Calldata = []byte{0xba, 0xdc, 0x0d, 0xde}
	m.attachParamsIfIdentical(ctx, it, drifted)
	if _, ok := m.pendingParams.Load(it.ID); ok {
		t.Fatal("drifted payload must not attach to winner's pendingParams")
	}
	var persisted []byte
	if err := db.QueryRow(ctx,
		`SELECT payload_data FROM outbound_tx_intents WHERE id=$1`, it.ID).
		Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persisted, req.Calldata) {
		t.Fatalf("persisted payload corrupted: %x want %x", persisted, req.Calldata)
	}
	// 同载荷重放 -> fast-path 挂载与 rebuild 同源等价, 允许.
	m.attachParamsIfIdentical(ctx, it, req)
	if _, ok := m.pendingParams.Load(it.ID); !ok {
		t.Fatal("identical payload should attach to pendingParams fast-path")
	}
}

package indexer

// FIX-G BUG-2 回归: indexer 提交后 SSE 发布.
//
//   - block_detected   — processBlock 捕获订单关联 Transfer 后;
//   - confirmation_progress — 仍 DETECTED 的订单充值在块推进时;
//   - paid 等 STATUS_CHANGED — finalizeDeposit 结算提交后;
//   - 全部事件仅可在业务事务 COMMIT 后发布: fake broker 在每次发布时点
//     以独立连接回读已提交状态作为直接证据 (先发后提则读到旧值/不存在).

import (
	"context"
	"sync"
	"testing"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// fixgSSE 发布记录器: 事件 + 发布时点独立连接读到的已提交状态.
type fixgSSE struct {
	db         *pgxpool.Pool
	mu         sync.Mutex
	evs        []ports.OrderEvent
	orderAtPub []string // 发布时点 payment_orders.status (未提交不可见)
	depAtPub   []string // 发布时点该订单最新 onchain_deposits.status
}

func (f *fixgSSE) PublishOrderEvent(ctx context.Context, ev ports.OrderEvent) error {
	var ost, dst string
	_ = f.db.QueryRow(ctx,
		`SELECT status FROM payment_orders WHERE id=$1`, ev.OrderID).Scan(&ost)
	_ = f.db.QueryRow(ctx,
		`SELECT status FROM onchain_deposits
		  WHERE matched_order_id=$1
		  ORDER BY block_number DESC, tx_hash DESC, log_index DESC LIMIT 1`,
		ev.OrderID).Scan(&dst)
	f.mu.Lock()
	f.evs = append(f.evs, ev)
	f.orderAtPub = append(f.orderAtPub, ost)
	f.depAtPub = append(f.depAtPub, dst)
	f.mu.Unlock()
	return nil
}

func (f *fixgSSE) SubscribeOrder(ctx context.Context, orderID uuid.UUID) (<-chan ports.OrderEvent, func(), error) {
	return nil, func() {}, nil
}

func (f *fixgSSE) snapshot() ([]ports.OrderEvent, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.OrderEvent(nil), f.evs...),
		append([]string(nil), f.orderAtPub...), append([]string(nil), f.depAtPub...)
}

// 订单事件流: DETECTED -> CONFIRMATION_UPDATE -> STATUS_CHANGED(PAID),
// 注资 (TOPUP) 不产生订单事件; 每个事件发布时点已提交状态可见.
func TestSSEIndexerPostCommitEvents(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	seedAsset(t, db, "USDT", usdtContract)
	m := seedMerchant(t, db, 100)

	v1, o1, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	v2, o2, _ := seedOrder(t, db, m, "USDC", 500, "AWAITING_PAYMENT", time.Hour)

	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")
	blk6 := uint64(6)
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 5, Finalized: 5},
		headers: chainOf(10, 'A'),
		evs: map[uint64][]chain.TransferEvent{blk6: {
			transferEv(v1, usdcContract, ext, 1000, blk6, types.HexToHash("0x01")),
			transferEv(v2, usdtContract, ext, 600, blk6, types.HexToHash("0x02")),         // 异币 — 仍关联订单
			transferEv(payoutHot, usdcContract, ext, 2000, blk6, types.HexToHash("0x04")), // 注资 — 无订单
		}},
		receipts: map[types.Hash]*ethtypes.Receipt{},
	}
	seedCursor(t, db, 5, chainOf(10, 'A')[5].Hash)
	rec := &fixgSSE{db: db}
	led := newFakeLedger(db)
	ix := New(db, fq, led, rec, &fakeRebc{}, Config{
		ChainID:               testChain,
		BatchBlocks:           100,
		PayoutHotWallet:       payoutHot,
		RelayerHotWallet:      relayerHot,
		ColdWallet:            coldWallet,
		BatchSweeper:          sweeperAddr,
		RequiredConfirmations: 1,
	}, nil, nil)

	// Tick 1: 捕获 (safe 未达) -> DETECTED + CONFIRMATION_UPDATE.
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	evs, orderPub, depPub := rec.snapshot()
	if len(evs) != 4 {
		t.Fatalf("tick1 events %d want 4 (2x DETECTED + 2x CONF): %+v", len(evs), evs)
	}
	// 块内 evs 顺序: o1, o2 检测事件 (注资无事件); 随后确认进度 (按块内序).
	if evs[0].Type != ports.OrderEventDepositDetected || evs[0].OrderID != o1 {
		t.Fatalf("ev0 %+v want DETECTED o1", evs[0])
	}
	if evs[1].Type != ports.OrderEventDepositDetected || evs[1].OrderID != o2 {
		t.Fatalf("ev1 %+v want DETECTED o2 (wrong-asset is still order-matched)", evs[1])
	}
	for i := 2; i < 4; i++ {
		if evs[i].Type != ports.OrderEventConfirmationUpdate {
			t.Fatalf("ev%d %+v want CONFIRMATION_UPDATE", i, evs[i])
		}
		// 充值在块 6, latest=7 -> confirmations = 7+1-6 = 2; required=1.
		if evs[i].Confirmations != 2 || evs[i].RequiredConfirmations != 1 {
			t.Fatalf("conf payload %+v want 2/1", evs[i])
		}
	}
	if evs[2].OrderID != o1 || evs[3].OrderID != o2 {
		t.Fatalf("conf order binding %+v %+v", evs[2], evs[3])
	}
	// 发布时点证据: 检测事件的充值行必须已提交为 DETECTED (非 ORPHANED/不存在).
	for i := 0; i < 2; i++ {
		if depPub[i] != "DETECTED" {
			t.Fatalf("ev%d published before deposit commit visible: committed=%q", i, depPub[i])
		}
	}

	// Tick 2: safe 推至 9 -> 结算 -> STATUS_CHANGED(PAID).
	fq.heads = chain.Heights{Latest: 8, Safe: 9, Finalized: 9}
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	evs, orderPub, depPub = rec.snapshot()
	if len(evs) != 5 {
		t.Fatalf("tick2 events %d want 5 (+STATUS_CHANGED): %+v", len(evs), evs)
	}
	paid := evs[4]
	if paid.Type != ports.OrderEventStatusChanged || paid.OrderID != o1 ||
		paid.Status != types.OrderPaid || paid.ReceivedAmount.String() != "1000" {
		t.Fatalf("settle event %+v want STATUS_CHANGED PAID received=1000", paid)
	}
	// 发布时点订单必须已提交 PAID — 先于提交发布会读到 AWAITING_PAYMENT.
	if orderPub[4] != "PAID" {
		t.Fatalf("paid event published before commit: committed=%q", orderPub[4])
	}
	// 异币单不应产出 STATUS_CHANGED (仅 DEPOSIT_DETECTED + CONF).
	for _, ev := range evs {
		if ev.Type == ports.OrderEventStatusChanged && ev.OrderID == o2 {
			t.Fatalf("wrong-asset order emitted spurious status change: %+v", ev)
		}
	}
}

// 无 broker 注入 (生产降级路径) 必须 no-op 而非 panic — 由 newIx(…, nil, …)
// 覆盖全量既有用例, 此处显式走一遍含事件路径.
func TestSSENilBrokerNoop(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	seedAsset(t, db, "USDC", usdcContract)
	m := seedMerchant(t, db, 100)
	v1, o1, _ := seedOrder(t, db, m, "USDC", 1000, "AWAITING_PAYMENT", time.Hour)
	ext := types.HexToAddress("0x9999999999999999999999999999999999999999")
	fq := &fakeChainQ{
		heads:   chain.Heights{Latest: 7, Safe: 9, Finalized: 9},
		headers: chainOf(10, 'A'),
		evs: map[uint64][]chain.TransferEvent{6: {
			transferEv(v1, usdcContract, ext, 1000, 6, types.HexToHash("0x01")),
		}},
		receipts: map[types.Hash]*ethtypes.Receipt{},
	}
	seedCursor(t, db, 5, chainOf(10, 'A')[5].Hash)
	ix := newIx(db, fq, newFakeLedger(db), &fakeRebc{}) // sse=nil
	if err := ix.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := orderState(t, db, o1)
	if st != "PAID" {
		t.Fatalf("order %s want PAID (nil broker must not affect settlement)", st)
	}
}

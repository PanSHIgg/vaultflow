package chain

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// fake NodeClient
// ---------------------------------------------------------------------------

type fakeNode struct {
	number   uint64
	byTag    map[BlockTag]*BlockHeader
	byNum    map[uint64]*BlockHeader
	logs     []ethtypes.Log
	logErr   error
	queries  []ethereum.FilterQuery
	receipts map[string]*ethtypes.Receipt
	vis      map[types.Hash][2]bool // found, pending
	latest   map[string]uint64      // addr -> nonce
	pending  map[string]uint64
	balances map[string]*big.Int
	callRes  []byte
	sendErr  error
	tagErr   map[BlockTag]error
	// noEIP234 模拟不支持 eth_getLogs blockHash 参数的老节点.
	noEIP234 bool
	// FIX-G BUG-1 回归: 记录 NonceAt 收到的 blockNumber 参数
	// (nil = latest; -1 会序列化为非法 "-0x1" 被 JSON-RPC -32602 拒绝).
	mu        sync.Mutex
	nonceArgs []*big.Int
}

func (f *fakeNode) BlockNumber(ctx context.Context) (uint64, error) { return f.number, nil }

func (f *fakeNode) HeaderByTag(ctx context.Context, tag BlockTag) (*BlockHeader, error) {
	if f.tagErr != nil {
		if e := f.tagErr[tag]; e != nil {
			return nil, e
		}
	}
	if h, ok := f.byTag[tag]; ok {
		return h, nil
	}
	if h, ok := f.byTag[TagLatest]; ok {
		return h, nil
	}
	return nil, ethereum.NotFound
}

func (f *fakeNode) HeaderByNumber(ctx context.Context, num uint64) (*BlockHeader, error) {
	if h, ok := f.byNum[num]; ok {
		return h, nil
	}
	return nil, ethereum.NotFound
}

func (f *fakeNode) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]ethtypes.Log, error) {
	f.queries = append(f.queries, q)
	if q.BlockHash != nil && f.noEIP234 {
		return nil, errors.New("invalid argument 0: blockHash filter not supported")
	}
	return f.logs, f.logErr
}

func (f *fakeNode) TransactionReceipt(ctx context.Context, h types.Hash) (*ethtypes.Receipt, error) {
	if r, ok := f.receipts[h.Hex()]; ok {
		return r, nil
	}
	return nil, ethereum.NotFound
}

func (f *fakeNode) TxVisibility(ctx context.Context, h types.Hash) (bool, bool, error) {
	if v, ok := f.vis[h]; ok {
		return v[0], v[1], nil
	}
	return false, false, nil
}

func (f *fakeNode) NonceAt(ctx context.Context, a types.Address, bn *big.Int) (uint64, error) {
	f.mu.Lock()
	f.nonceArgs = append(f.nonceArgs, bn)
	f.mu.Unlock()
	return f.latest[a.Hex()], nil
}

func (f *fakeNode) PendingNonceAt(ctx context.Context, a types.Address) (uint64, error) {
	return f.pending[a.Hex()], nil
}

func (f *fakeNode) BalanceAt(ctx context.Context, a types.Address, bn *big.Int) (*big.Int, error) {
	if b, ok := f.balances[a.Hex()]; ok {
		return b, nil
	}
	return new(big.Int), nil
}

func (f *fakeNode) CallContract(ctx context.Context, msg ethereum.CallMsg, bn *big.Int) ([]byte, error) {
	return f.callRes, nil
}

func (f *fakeNode) SendRawTransaction(ctx context.Context, raw []byte) (types.Hash, error) {
	return types.Hash(crypto.Keccak256Hash(raw)), f.sendErr
}

func (f *fakeNode) Close() {}

func hdr(num uint64, b byte) *BlockHeader {
	return &BlockHeader{Number: num, Hash: types.HexToHash(fillHash(b)), ParentHash: types.HexToHash(fillHash(b - 1))}
}

func fillHash(b byte) string {
	return "0x" + strings.Repeat(string(rune('0'+(b%16))), 2) + strings.Repeat("0", 62)
}

// ---------------------------------------------------------------------------
// Pool 仲裁
// ---------------------------------------------------------------------------

func TestHeightsLagExclusion(t *testing.T) {
	fast := func() *fakeNode {
		return &fakeNode{number: 100, byTag: map[BlockTag]*BlockHeader{
			TagLatest: hdr(100, 0xaa), TagSafe: hdr(90, 0x90), TagFinalized: hdr(80, 0x80),
		}}
	}
	lagNode := &fakeNode{number: 50, byTag: map[BlockTag]*BlockHeader{
		TagLatest: hdr(50, 0x50), TagSafe: hdr(45, 0x45), TagFinalized: hdr(40, 0x40),
	}}
	p := NewPool([]Node{{Name: "a", Cli: fast()}, {Name: "b", Cli: fast()}, {Name: "c", Cli: lagNode}}, 2)

	// minSynced=95: 滞后节点 latest=50 < 95 被剔除.
	h, err := p.Heights(context.Background(), 95)
	if err != nil {
		t.Fatal(err)
	}
	if h.Latest != 100 || h.Safe != 90 || h.Finalized != 80 {
		t.Fatalf("heights %+v, want {100 90 80}", h)
	}
}

func TestHeightsSafeTagFallback(t *testing.T) {
	// 开发链 (anvil): safe/finalized 标签不支持 -> 回退 latest.
	n := func() *fakeNode {
		return &fakeNode{number: 42, byTag: map[BlockTag]*BlockHeader{TagLatest: hdr(42, 0x2a)},
			tagErr: map[BlockTag]error{TagSafe: errors.New("unknown block"), TagFinalized: errors.New("unknown block")}}
	}
	p := NewPool([]Node{{Name: "a", Cli: n()}, {Name: "b", Cli: n()}}, 2)
	h, err := p.Heights(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Latest != 42 || h.Safe != 42 {
		t.Fatalf("fallback heights %+v, want {42 42 42-ish}", h)
	}
}

func TestHeaderAtMajority(t *testing.T) {
	mk := func(h *BlockHeader) *fakeNode { return &fakeNode{byNum: map[uint64]*BlockHeader{7: h}} }
	good := hdr(7, 0x77)
	bad := hdr(7, 0xde)
	p := NewPool([]Node{{Name: "a", Cli: mk(good)}, {Name: "b", Cli: mk(good)}, {Name: "c", Cli: mk(bad)}}, 2)
	got, err := p.HeaderAt(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != good.Hash {
		t.Fatalf("majority hash %s want %s", got.Hash, good.Hash)
	}

	// 三节点全分歧 -> ErrNoQuorum.
	p2 := NewPool([]Node{{Name: "a", Cli: mk(hdr(7, 0x1))}, {Name: "b", Cli: mk(hdr(7, 0x2))},
		{Name: "c", Cli: mk(hdr(7, 0x3))}}, 2)
	if _, err := p2.HeaderAt(context.Background(), 7); !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("want ErrNoQuorum, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 自适应 Transfer 日志
// ---------------------------------------------------------------------------

func mkTransferLog(token, to types.Address, amt int64, blk uint64, blkHash types.Hash) ethtypes.Log {
	topics := []types.Hash{ERC20TransferTopic,
		common.BytesToHash(types.HexToAddress("0x9999999999999999999999999999999999999999").Bytes()),
		common.BytesToHash(to.Bytes())}
	data := new(big.Int).SetInt64(amt).FillBytes(make([]byte, 32))
	return ethtypes.Log{Address: token, Topics: topics, Data: data,
		BlockNumber: blk, TxHash: types.HexToHash(fillHash(0x55)), Index: 3, BlockHash: common.Hash(blkHash)}
}

func TestTransferLogsServerSideFilter(t *testing.T) {
	watched := types.HexToAddress("0x1111111111111111111111111111111111111111")
	token := types.HexToAddress("0x2222222222222222222222222222222222222222")
	hdr := &BlockHeader{Number: 10, Hash: types.HexToHash(fillHash(0x66))}
	f := &fakeNode{logs: []ethtypes.Log{mkTransferLog(token, watched, 700, 10, hdr.Hash)}}
	p := NewPool([]Node{{Name: "a", Cli: f}}, 1)

	evs, err := p.TransferLogs(context.Background(), hdr, []types.Address{token},
		[]types.Address{watched}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 {
		t.Fatalf("expected 1 FilterLogs call, got %d", len(f.queries))
	}
	if f.queries[0].BlockHash == nil || *f.queries[0].BlockHash != common.Hash(hdr.Hash) {
		t.Fatal("pinned query must carry EIP-234 blockHash = canonical header hash")
	}
	if f.queries[0].FromBlock != nil || f.queries[0].ToBlock != nil {
		t.Fatal("blockHash is mutually exclusive with fromBlock/toBlock")
	}
	if len(f.queries[0].Topics) < 3 || f.queries[0].Topics[2] == nil {
		t.Fatal("watch<=threshold must use server-side topics[2] filter")
	}
	if len(evs) != 1 || evs[0].Amount.Int64() != 700 || evs[0].To != watched {
		t.Fatalf("events %+v", evs)
	}
}

func TestTransferLogsInMemoryMatch(t *testing.T) {
	watched := types.HexToAddress("0x1111111111111111111111111111111111111111")
	other := types.HexToAddress("0x3333333333333333333333333333333333333333")
	token := types.HexToAddress("0x2222222222222222222222222222222222222222")
	hdr := &BlockHeader{Number: 10, Hash: types.HexToHash(fillHash(0x66))}
	f := &fakeNode{logs: []ethtypes.Log{
		mkTransferLog(token, watched, 100, 10, hdr.Hash),
		mkTransferLog(token, other, 200, 10, hdr.Hash), // 非监控地址 — 内存剔除
	}}
	p := NewPool([]Node{{Name: "a", Cli: f}}, 1)

	watch := make([]types.Address, 0, 10)
	watch = append(watch, watched)
	for i := 0; i < 9; i++ {
		watch = append(watch, common.BigToAddress(big.NewInt(int64(i+1))))
	}
	evs, err := p.TransferLogs(context.Background(), hdr, []types.Address{token}, watch, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries[0].Topics) >= 3 && f.queries[0].Topics[2] != nil {
		t.Fatal("watch>threshold must NOT use topics[2] server filter")
	}
	if len(evs) != 1 || evs[0].To != watched || evs[0].Amount.Int64() != 100 {
		t.Fatalf("in-memory match failed: %+v", evs)
	}
}

// ---------------------------------------------------------------------------
// FIX-A/A1 回归: blockHash 钉死 + EIP-234 不支持回退 + 分叉视图剔除/告警
// ---------------------------------------------------------------------------

func TestTransferLogsFallbackStrictBlockHashFilter(t *testing.T) {
	watched := types.HexToAddress("0x1111111111111111111111111111111111111111")
	token := types.HexToAddress("0x2222222222222222222222222222222222222222")
	hdr := &BlockHeader{Number: 10, Hash: types.HexToHash(fillHash(0x66))}
	forkHash := types.HexToHash(fillHash(0x77))
	f := &fakeNode{noEIP234: true, logs: []ethtypes.Log{
		mkTransferLog(token, watched, 100, 10, hdr.Hash),
		mkTransferLog(token, watched, 200, 10, forkHash), // 同高度分叉视图日志 — 必须剔除
	}}
	var alerts []ForkViewAlert
	p := NewPool([]Node{{Name: "a", Cli: f}}, 1,
		WithForkViewAlert(func(a ForkViewAlert) { alerts = append(alerts, a) }))

	evs, err := p.TransferLogs(context.Background(), hdr, []types.Address{token},
		[]types.Address{watched}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 2 {
		t.Fatalf("expected pinned+ranged queries, got %d", len(f.queries))
	}
	if f.queries[0].BlockHash == nil {
		t.Fatal("first query must be the pinned blockHash query")
	}
	if f.queries[1].FromBlock == nil || f.queries[1].FromBlock.Uint64() != 10 ||
		f.queries[1].ToBlock == nil || f.queries[1].ToBlock.Uint64() != 10 {
		t.Fatalf("fallback must be bounded to the single canonical height, got %+v", f.queries[1])
	}
	if len(evs) != 1 || evs[0].Amount.Int64() != 100 || evs[0].BlockHash != hdr.Hash {
		t.Fatalf("fork-view log leaked into result: %+v", evs)
	}
	if len(alerts) != 1 || alerts[0].Divergent != 1 || alerts[0].GotHash != forkHash ||
		alerts[0].Node != "a" || alerts[0].WantHash != hdr.Hash {
		t.Fatalf("fork-view alert %+v", alerts)
	}
}

func TestTransferLogsBlockHashParamIgnored(t *testing.T) {
	// 老节点静默忽略 blockHash 参数: 钉死响应混入异哈希日志即视为
	// EIP-234 不支持 -> 区间回退 + 严格过滤 + 告警.
	watched := types.HexToAddress("0x1111111111111111111111111111111111111111")
	token := types.HexToAddress("0x2222222222222222222222222222222222222222")
	hdr := &BlockHeader{Number: 10, Hash: types.HexToHash(fillHash(0x66))}
	forkHash := types.HexToHash(fillHash(0x88))
	f := &fakeNode{logs: []ethtypes.Log{
		mkTransferLog(token, watched, 100, 10, hdr.Hash),
		mkTransferLog(token, watched, 999, 10, forkHash),
	}}
	var alerts []ForkViewAlert
	p := NewPool([]Node{{Name: "a", Cli: f}}, 1,
		WithForkViewAlert(func(a ForkViewAlert) { alerts = append(alerts, a) }))

	evs, err := p.TransferLogs(context.Background(), hdr, []types.Address{token},
		[]types.Address{watched}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 2 {
		t.Fatalf("expected pinned+ranged queries, got %d", len(f.queries))
	}
	if len(evs) != 1 || evs[0].Amount.Int64() != 100 {
		t.Fatalf("wrong-hash log leaked: %+v", evs)
	}
	if len(alerts) != 1 || alerts[0].Divergent != 1 || alerts[0].GotHash != forkHash {
		t.Fatalf("alerts %+v", alerts)
	}
}

func TestHeightsSafeFallbackRequiredConfirmations(t *testing.T) {
	// A3: safe/finalized 标签不支持 -> safe = max(latest-requiredConfs,0), fin<=safe.
	n := func() *fakeNode {
		return &fakeNode{number: 42, byTag: map[BlockTag]*BlockHeader{TagLatest: hdr(42, 0x2a)},
			tagErr: map[BlockTag]error{TagSafe: errors.New("unknown block"), TagFinalized: errors.New("unknown block")}}
	}
	p := NewPool([]Node{{Name: "a", Cli: n()}, {Name: "b", Cli: n()}}, 2, WithRequiredConfirmations(12))
	h, err := p.Heights(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Latest != 42 || h.Safe != 30 || h.Finalized != 30 {
		t.Fatalf("heights %+v, want {42 30 30}", h)
	}
	// requiredConfs > latest -> safe 下限 0, fin<=safe.
	p2 := NewPool([]Node{{Name: "a", Cli: n()}}, 1, WithRequiredConfirmations(100))
	h2, err := p2.Heights(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Safe != 0 || h2.Finalized != 0 {
		t.Fatalf("clamped heights %+v, want safe=0 fin=0", h2)
	}
	// requiredConfs=0 -> 兼容旧行为 (safe=latest).
	p3 := NewPool([]Node{{Name: "a", Cli: n()}}, 1)
	h3, err := p3.Heights(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if h3.Safe != 42 {
		t.Fatalf("zero-conf fallback safe=%d want 42", h3.Safe)
	}
}

// ---------------------------------------------------------------------------
// FIX-G BUG-1 回归: latest nonce 探测必须发 "latest" 标签 (NonceAt nil),
// 严禁 TagLatest.Num() (-1) — "-0x1" 参数被 anvil/生产节点以 -32602 拒绝,
// 新热钱包首笔 intent 因此永远无法建 nonce 游标.
// ---------------------------------------------------------------------------

func TestPoolNonceLatestTagNoMinusOne(t *testing.T) {
	addr := types.HexToAddress("0x1111111111111111111111111111111111111111")
	f := &fakeNode{latest: map[string]uint64{addr.Hex(): 7}}
	p := NewPool([]Node{{Name: "a", Cli: f}}, 1)
	n, err := p.Nonce(context.Background(), addr, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("nonce %d want 7", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.nonceArgs) == 0 {
		t.Fatal("latest nonce path never reached NonceAt")
	}
	for i, bn := range f.nonceArgs {
		if bn != nil {
			t.Fatalf("NonceAt call %d got blockNumber=%v — must be nil (\"latest\" tag); "+
				"a negative tag serialises as '-0x1' and is rejected with -32602", i, bn)
		}
	}

	// pending 路径必须继续走 PendingNonceAt, 不触碰 NonceAt.
	f2 := &fakeNode{pending: map[string]uint64{addr.Hex(): 9}}
	p2 := NewPool([]Node{{Name: "a", Cli: f2}}, 1)
	n2, err := p2.Nonce(context.Background(), addr, true)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 9 {
		t.Fatalf("pending nonce %d want 9", n2)
	}
	if len(f2.nonceArgs) != 0 {
		t.Fatalf("pending=true must not reach NonceAt, got %d calls", len(f2.nonceArgs))
	}
}

// ---------------------------------------------------------------------------
// FIX-G BUG-4 回归: devnet 标签适配 — 真实 safe 透传 / safe=0 转译 /
// 陈旧 safe (latest-safe > requiredConfs) 转译 -> Pool 走既定回退.
// ---------------------------------------------------------------------------

func TestDevnetTagAdapter(t *testing.T) {
	ctx := context.Background()
	mk := func(latest, safe, fin uint64) *fakeNode {
		return &fakeNode{number: latest, byTag: map[BlockTag]*BlockHeader{
			TagLatest: hdr(latest, 0xaa), TagSafe: hdr(safe, 0x55), TagFinalized: hdr(fin, 0x44)}}
	}

	// ① 阈值内的真实 safe/finalized 原样透传 (生产等效语义).
	ad := NewDevnetTagAdapter(mk(100, 95, 90), 12)
	h, err := ad.HeaderByTag(ctx, TagSafe)
	if err != nil || h == nil || h.Number != 95 {
		t.Fatalf("real safe must pass through: %+v err=%v", h, err)
	}
	if h, err = ad.HeaderByTag(ctx, TagFinalized); err != nil || h == nil || h.Number != 90 {
		t.Fatalf("real finalized must pass through: %+v err=%v", h, err)
	}
	if h, err = ad.HeaderByTag(ctx, TagLatest); err != nil || h == nil || h.Number != 100 {
		t.Fatalf("latest tag must never be adapted: %+v err=%v", h, err)
	}

	// ② safe=0 且链已有高度 -> 转译 NotFound -> safe=latest-requiredConfs.
	zero := &fakeNode{number: 50, byTag: map[BlockTag]*BlockHeader{
		TagLatest: hdr(50, 0xaa), TagSafe: hdr(0, 0x00), TagFinalized: hdr(0, 0x00)}}
	p := NewPool([]Node{{Name: "a", Cli: NewDevnetTagAdapter(zero, 1)}}, 1,
		WithRequiredConfirmations(1))
	hs, err := p.Heights(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Safe != 49 || hs.Finalized != 49 {
		t.Fatalf("zero safe must fall back to latest-requiredConfs: %+v want safe=49 fin=49", hs)
	}

	// ③ 陈旧 safe (anvil latest-32) 超阈值 -> 转译 NotFound -> 回退生效.
	stale := mk(100, 68, 36)
	p = NewPool([]Node{{Name: "a", Cli: NewDevnetTagAdapter(stale, 1)}}, 1,
		WithRequiredConfirmations(1))
	hs, err = p.Heights(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Safe != 99 || hs.Finalized != 99 {
		t.Fatalf("stale safe (lag 32 > 1) must fall back: %+v want safe=99 fin=99", hs)
	}
	// 同节点在 requiredConfs>=lag 时属可接受 -> 透传 (防误伤真实标签).
	p = NewPool([]Node{{Name: "a", Cli: NewDevnetTagAdapter(stale, 64)}}, 1,
		WithRequiredConfirmations(64))
	hs, err = p.Heights(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Safe != 68 || hs.Finalized != 36 {
		t.Fatalf("within-threshold tags must pass through: %+v want safe=68 fin=36", hs)
	}
}

// ---------------------------------------------------------------------------
// EthNodeClient — httptest JSON-RPC
// ---------------------------------------------------------------------------

func rpcServer(t *testing.T, handler func(method string, params []json.RawMessage) (any, *rpcErr)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     int64             `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		res, re := handler(env.Method, env.Params)
		w.Header().Set("Content-Type", "application/json")
		if re != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": env.ID,
				"error": map[string]any{"code": re.Code, "message": re.Msg}})
			return
		}
		raw, _ := json.Marshal(res)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": env.ID, "result": json.RawMessage(raw)})
	}))
}

type rpcErr struct {
	Code int
	Msg  string
}

func TestEthNodeClientHTTP(t *testing.T) {
	srv := rpcServer(t, func(method string, params []json.RawMessage) (any, *rpcErr) {
		switch method {
		case "eth_chainId":
			return "0x7a69", nil
		case "eth_blockNumber":
			return "0x64", nil
		case "eth_getBlockByNumber":
			return map[string]any{"number": "0x2a", "hash": fillHash(0x2a), "parentHash": fillHash(0x29)}, nil
		case "eth_getBalance":
			return "0xde0b6b3a7640000", nil
		case "eth_sendRawTransaction":
			return nil, &rpcErr{Code: -32000, Msg: "already known"}
		case "eth_getTransactionCount":
			return "0x5", nil
		case "eth_call":
			return "0x" + strings.Repeat("0", 63) + "7", nil
		}
		return nil, &rpcErr{Code: -32601, Msg: "method not found"}
	})
	defer srv.Close()

	c, err := DialEthNode(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	n, err := c.BlockNumber(context.Background())
	if err != nil || n != 100 {
		t.Fatalf("blockNumber %d %v", n, err)
	}
	h, err := c.HeaderByTag(context.Background(), TagSafe)
	if err != nil || h.Number != 42 || h.Hash != types.HexToHash(fillHash(0x2a)) {
		t.Fatalf("header %+v %v", h, err)
	}
	bal, err := c.BalanceAt(context.Background(), types.HexToAddress("0x1"), nil)
	if err != nil || bal.Cmp(big.NewInt(1_000_000_000_000_000_000)) != 0 {
		t.Fatalf("balance %v %v", bal, err)
	}
	if _, err := c.SendRawTransaction(context.Background(), []byte{0x02}); !IsAlreadyKnownError(err) {
		t.Fatalf("want already-known, got %v", err)
	}
	nonce, err := c.PendingNonceAt(context.Background(), types.HexToAddress("0x1"))
	if err != nil || nonce != 5 {
		t.Fatalf("nonce %d %v", nonce, err)
	}
}

// ---------------------------------------------------------------------------
// ABI 编解码与事件解析
// ---------------------------------------------------------------------------

func TestEncodeERC20Transfer(t *testing.T) {
	to := types.HexToAddress("0x3333333333333333333333333333333333333333")
	data, err := EncodeERC20Transfer(to, big.NewInt(1234567))
	if err != nil {
		t.Fatal(err)
	}
	want := "a9059cbb" + "0000000000000000000000003333333333333333333333333333333333333333" +
		"000000000000000000000000000000000000000000000000000000000012d687"
	got := strings.ToLower(strings.TrimPrefix(common.BytesToHash(data[:4]).Hex(), "0x"))
	_ = got // selector 由 geth abi 计算 — 直接断言完整 hex 更简单
	full := encodeHex(data)
	if full != want {
		t.Fatalf("transfer calldata mismatch:\n got %s\nwant %s", full, want)
	}
}

func TestEncodeSweepBatchSelector(t *testing.T) {
	items := []SweepItem{
		{Salt: types.HexToHash(fillHash(0x11)), ExactAmount: big.NewInt(111)},
		{Salt: types.HexToHash(fillHash(0x22)), ExactAmount: big.NewInt(222)},
	}
	token := types.HexToAddress("0x2222222222222222222222222222222222222222")
	cold := types.HexToAddress("0x4444444444444444444444444444444444444444")
	data, err := EncodeDeployAndSweepBatch(items, token, cold)
	if err != nil {
		t.Fatal(err)
	}
	sel := crypto.Keccak256([]byte("deployAndSweepBatch((bytes32,uint256)[],address,address)"))[:4]
	for i := range sel {
		if data[i] != sel[i] {
			t.Fatalf("selector mismatch: got %x want %x", data[:4], sel)
		}
	}
	if len(data) <= 4+64 {
		t.Fatal("calldata too short for tuple-array payload")
	}
}

func TestParseSweepEvents(t *testing.T) {
	sweeper := types.HexToAddress("0x5555555555555555555555555555555555555555")
	vault := types.HexToAddress("0x6666666666666666666666666666666666666666")
	salt := types.HexToHash(fillHash(0x77))
	mk := func(topic types.Hash, emitter types.Address) *ethtypes.Log {
		return &ethtypes.Log{Address: emitter,
			Topics: []types.Hash{topic, common.BytesToHash(vault.Bytes()), salt},
			Data:   new(big.Int).SetInt64(888).FillBytes(make([]byte, 32))}
	}
	logs := []*ethtypes.Log{
		mk(VaultSweptTopic, sweeper),
		mk(VaultQuarantinedTopic, sweeper),
		mk(VaultSweptTopic, types.HexToAddress("0x7777777777777777777777777777777777777777")), // 异 emitter 剔除
	}
	evs := ParseSweepEvents(logs, sweeper)
	if len(evs) != 2 {
		t.Fatalf("want 2 events (emitter filter), got %d", len(evs))
	}
	if evs[0].Kind != SweepEventSwept || evs[1].Kind != SweepEventQuarantined {
		t.Fatalf("kinds %v %v", evs[0].Kind, evs[1].Kind)
	}
	if evs[0].Vault != vault || evs[0].Salt != salt || evs[0].Amount.Int64() != 888 {
		t.Fatalf("event fields %+v", evs[0])
	}
}

func encodeHex(b []byte) string {
	const hexdig = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdig[c>>4]
		out[i*2+1] = hexdig[c&0x0f]
	}
	return string(out)
}

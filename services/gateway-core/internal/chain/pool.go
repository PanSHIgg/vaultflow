package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"vaultflow/gateway-core/internal/types"
)

// 包级哨兵错误 — 仲裁池无法达成法定多数时抛出, 上层按可重试故障处理.
var (
	// ErrNoQuorum 健康节点不足 quorum 或多节点结果分歧无多数.
	ErrNoQuorum = errors.New("chain: no quorum among rpc nodes")
	// ErrNoNodes 池内没有任何已配置节点.
	ErrNoNodes = errors.New("chain: rpc pool has no nodes")
)

// Node 池内一个 RPC 节点.
type Node struct {
	Name string // 节点标识 (用于日志与告警)
	Cli  NodeClient
}

// Pool 多节点仲裁池 (§4.1/W4-1).
//
//   - latest/safe/finalized 高度按"至少 quorum 个节点支持"的最大值仲裁;
//   - latest < minSynced 的节点视为滞后节点被当轮剔除 — 否则一个落后节点
//     报告的过期链视图会触发对 parent_hash 的误判, 造成假 Reorg (ARCHITECTURE §4.1);
//   - 指定高度区块头按多数哈希仲裁 (防单节点分叉视图).
type Pool struct {
	nodes  []Node
	quorum int // 达成一致的最低节点数 (默认多数 len/2+1)

	// requiredConfs A3: safe 标签缺失时的确认数回退深度
	// (safe = max(latest-required_confirmations, 0)); 0 = 退化为 latest.
	requiredConfs uint64
	// forkAlert A1: 节点分叉视图/参数忽略证据回调 (见 ForkViewAlert).
	forkAlert func(ForkViewAlert)
}

// PoolOption NewPool 可选装配参数 (配置链接入点, Wave-2 I1 由链配置注入).
type PoolOption func(*Pool)

// WithRequiredConfirmations A3: 链上 required_confirmations 阈值 —
// 节点不支持 safe 标签时 safe 回退为 max(latest-requiredConfirmations, 0);
// finalized 标签缺失时 fin 回退为 safe (维持 fin <= safe 不变量).
// 0 兼容旧行为 (safe=latest, 仅适用于 dev 链).
func WithRequiredConfirmations(n uint64) PoolOption {
	return func(p *Pool) { p.requiredConfs = n }
}

// WithForkViewAlert A1: 日志拉取中发现节点给出与仲裁哈希不一致的证据时回调.
// 典型: 节点不支持 blockHash 参数走区间回退后, 其返回集含异哈希同高度日志
// (分叉视图). 回调应投递到运维/安全告警通道; 同步上下文内保持轻量.
func WithForkViewAlert(fn func(ForkViewAlert)) PoolOption {
	return func(p *Pool) { p.forkAlert = fn }
}

// NewPool 构造仲裁池; quorum<=0 时取多数 (n/2+1).
func NewPool(nodes []Node, quorum int, opts ...PoolOption) *Pool {
	if quorum <= 0 {
		quorum = len(nodes)/2 + 1
	}
	p := &Pool{nodes: nodes, quorum: quorum}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Quorum 返回当前仲裁阈值.
func (p *Pool) Quorum() int { return p.quorum }

// Nodes 返回池内节点 (只读视图).
func (p *Pool) Nodes() []Node { return p.nodes }

// Heights 仲裁得到的 latest/safe/finalized 高度.
type Heights struct {
	Latest    uint64
	Safe      uint64
	Finalized uint64
}

type headSample struct {
	name    string
	latest  uint64
	safe    uint64
	fin     uint64
	nodeErr error
}

// Heights 仲裁 latest/safe/finalized 高度.
//
// minSynced = chain_sync_cursors.latest_synced_height: 报告的 latest 低于该值
// 的节点被当轮剔除 (滞后节点防假 Reorg). 返回值满足 Safe<=Latest, Finalized<=Safe
// (对不合规节点响应做单调截断).
func (p *Pool) Heights(ctx context.Context, minSynced uint64) (Heights, error) {
	if len(p.nodes) == 0 {
		return Heights{}, ErrNoNodes
	}
	samples := make([]headSample, len(p.nodes))
	var wg sync.WaitGroup
	for i, n := range p.nodes {
		wg.Add(1)
		go func(i int, n Node) {
			defer wg.Done()
			var s headSample
			s.name = n.Name
			latest, err := n.Cli.BlockNumber(ctx)
			if err != nil {
				s.nodeErr = err
				samples[i] = s
				return
			}
			s.latest = latest
			if h, err := n.Cli.HeaderByTag(ctx, TagSafe); err == nil && h != nil {
				s.safe = h.Number
			} else {
				// A3: 节点不支持 safe 标签时回退 latest-required_confirmations
				// (下限 0); requiredConfs=0 时兼容旧的 latest 退化 (Anvil 等
				// 开发链). 此前恒 latest 会把"尚未满足确认数"的块当作安全头.
				s.safe = latest
				if s.safe >= p.requiredConfs {
					s.safe -= p.requiredConfs
				} else {
					s.safe = 0
				}
			}
			if h, err := n.Cli.HeaderByTag(ctx, TagFinalized); err == nil && h != nil {
				s.fin = h.Number
			} else {
				s.fin = s.safe // fin <= safe 不变量
			}
			samples[i] = s
		}(i, n)
	}
	wg.Wait()

	var healthy []headSample
	for _, s := range samples {
		if s.nodeErr != nil {
			continue
		}
		if s.latest < minSynced {
			// 滞后节点: 节点还没跟上我们已同步的高度 — 其链视图不可信, 剔除.
			continue
		}
		healthy = append(healthy, s)
	}
	if len(healthy) < p.quorum {
		return Heights{}, fmt.Errorf("%w: %d healthy nodes (need %d, minSynced=%d)",
			ErrNoQuorum, len(healthy), p.quorum, minSynced)
	}
	latests := make([]uint64, 0, len(healthy))
	safes := make([]uint64, 0, len(healthy))
	fins := make([]uint64, 0, len(healthy))
	for _, s := range healthy {
		latests = append(latests, s.latest)
		safes = append(safes, min64(s.safe, s.latest))
		fins = append(fins, min64(s.fin, min64(s.safe, s.latest)))
	}
	return Heights{
		Latest:    quorumFloor(latests, p.quorum),
		Safe:      quorumFloor(safes, p.quorum),
		Finalized: quorumFloor(fins, p.quorum),
	}, nil
}

// quorumFloor 取"至少 quorum 个样本 >= 它"的最大值: 降序第 quorum-1 位.
func quorumFloor(vals []uint64, quorum int) uint64 {
	sort.Slice(vals, func(i, j int) bool { return vals[i] > vals[j] })
	if len(vals) < quorum {
		return 0
	}
	return vals[quorum-1]
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// HeaderAt 取指定高度的区块头, 按多数哈希仲裁:
// 至少 quorum 个节点在该高度返回相同 block_hash 才接受, 否则报 ErrNoQuorum —
// 宁缺毋滥, 避免把某个节点的分叉视图当作主链.
func (p *Pool) HeaderAt(ctx context.Context, height uint64) (*BlockHeader, error) {
	type res struct {
		h   *BlockHeader
		err error
	}
	ch := make(chan res, len(p.nodes))
	for _, n := range p.nodes {
		go func(n Node) {
			h, err := n.Cli.HeaderByNumber(ctx, height)
			ch <- res{h, err}
		}(n)
	}
	votes := map[types.Hash]*BlockHeader{}
	counts := map[types.Hash]int{}
	var saw int
	for i := 0; i < len(p.nodes); i++ {
		r := <-ch
		if r.err != nil || r.h == nil {
			continue
		}
		saw++
		h := r.h.Hash
		votes[h] = r.h
		counts[h]++
		if counts[h] >= p.quorum {
			return r.h, nil
		}
	}
	return nil, fmt.Errorf("%w: header@%d divergent across %d nodes", ErrNoQuorum, height, saw)
}

// ReceiptAt 取交易回执, 按 (blockHash,status) 多数仲裁;
// 至少 quorum 个节点返回"未找到"时返回 (nil, nil).
func (p *Pool) ReceiptAt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error) {
	type res struct {
		r   *ethtypes.Receipt
		err error
	}
	ch := make(chan res, len(p.nodes))
	for _, n := range p.nodes {
		go func(n Node) {
			r, err := n.Cli.TransactionReceipt(ctx, hash)
			if err != nil {
				ch <- res{nil, err}
				return
			}
			ch <- res{r, nil}
		}(n)
	}
	type key struct {
		blockHash types.Hash
		number    uint64
		status    uint64
	}
	votes := map[key]*ethtypes.Receipt{}
	counts := map[key]int{}
	nilVotes := 0
	for i := 0; i < len(p.nodes); i++ {
		r := <-ch
		if r.err != nil || r.r == nil {
			nilVotes++
			if nilVotes >= p.quorum {
				return nil, nil
			}
			continue
		}
		k := key{blockHash: r.r.BlockHash, number: r.r.BlockNumber.Uint64(), status: r.r.Status}
		votes[k] = r.r
		counts[k]++
		if counts[k] >= p.quorum {
			return r.r, nil
		}
	}
	return nil, fmt.Errorf("%w: receipt %s divergent", ErrNoQuorum, hash.Hex())
}

// TxPresent 交易是否被任一节点看到 (mempool 或已入块).
//
// 保守策略 — 任一健康节点返回交易即视为存在, 最大限度避免把"传播中"误判为
// Mempool 驱逐 (Dropped 误标会浪费一次幂等槽重签, 但不会丢钱; 反向误判则阻塞恢复).
// isPending = 至少一个节点报告仍在 pending 池.
func (p *Pool) TxPresent(ctx context.Context, hash types.Hash) (present bool, pending bool, err error) {
	type res struct {
		found   bool
		pending bool
	}
	ch := make(chan res, len(p.nodes))
	var sawErr int
	for _, n := range p.nodes {
		go func(n Node) {
			found, isPending, e := n.Cli.TxVisibility(ctx, hash)
			if e != nil {
				ch <- res{}
				return
			}
			ch <- res{found: found, pending: isPending}
		}(n)
	}
	found := false
	pend := false
	for i := 0; i < len(p.nodes); i++ {
		r := <-ch
		if r.found {
			found = true
		}
		if r.pending {
			pend = true
		}
		if !r.found {
			sawErr++
		}
	}
	if !found && sawErr == len(p.nodes) {
		// 全部节点都查不到 (且均无错误) — 真实不可见, 不是故障.
		return false, false, nil
	}
	return found, pend, nil
}

// Nonce 仲裁账户 nonce: pending=false 用 latest, true 用 pending.
// 取中位数, 单节点毛刺不影响结论.
func (p *Pool) Nonce(ctx context.Context, addr types.Address, pending bool) (uint64, error) {
	type res struct {
		v   uint64
		err error
	}
	ch := make(chan res, len(p.nodes))
	for _, n := range p.nodes {
		go func(n Node) {
			var v uint64
			var e error
			if pending {
				v, e = n.Cli.PendingNonceAt(ctx, addr)
			} else {
				// FIX-G BUG-1: 严禁 TagLatest.Num() (-1) — 负值序列化为
				// "-0x1" 被 JSON-RPC 以 -32602 拒绝; NodeClient.NonceAt
				// 约定 nil 即 "latest" 标签.
				v, e = n.Cli.NonceAt(ctx, addr, nil)
			}
			ch <- res{v, e}
		}(n)
	}
	vals := make([]uint64, 0, len(p.nodes))
	var lastErr error
	for i := 0; i < len(p.nodes); i++ {
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		vals = append(vals, r.v)
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("chain: nonce query failed on all nodes: %w", lastErr)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	return vals[len(vals)/2], nil
}

// SendRawTx 向全部节点并发广播, 任一成功 (含 already-known 幂等) 即返回 tx_hash.
func (p *Pool) SendRawTx(ctx context.Context, raw []byte) (types.Hash, error) {
	type res struct {
		h   types.Hash
		err error
	}
	if len(p.nodes) == 0 {
		return types.Hash{}, ErrNoNodes
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan res, len(p.nodes))
	for _, n := range p.nodes {
		go func(n Node) {
			h, e := n.Cli.SendRawTransaction(ctx, raw)
			ch <- res{h, e}
		}(n)
	}
	var lastErr error
	for i := 0; i < len(p.nodes); i++ {
		r := <-ch
		if r.err == nil || IsAlreadyKnownError(r.err) {
			if r.err != nil {
				// already-known: 无法从返回值拿 hash, 由调用方按本地 keccak256 回填.
				return types.Hash{}, r.err
			}
			return r.h, nil
		}
		lastErr = r.err
	}
	return types.Hash{}, lastErr
}

// CallContract 首个可用节点的 eth_call (只读, 如 balanceOf).
func (p *Pool) CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	var lastErr error
	for _, n := range p.nodes {
		b, err := n.Cli.CallContract(ctx, msg, blockNumber)
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Receipt 与 ReceiptAt 同义 — 满足 txmanager.ChainProvider 接口命名.
func (p *Pool) Receipt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error) {
	return p.ReceiptAt(ctx, hash)
}

// NativeBalance eth_getBalance 逐节点故障转移 (水位监控读路径).
func (p *Pool) NativeBalance(ctx context.Context, addr types.Address) (*big.Int, error) {
	var lastErr error
	for _, n := range p.nodes {
		b, err := n.Cli.BalanceAt(ctx, addr, nil)
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ERC20BalanceOf eth_call balanceOf(owner) 逐节点故障转移.
func (p *Pool) ERC20BalanceOf(ctx context.Context, token, owner types.Address) (*big.Int, error) {
	data, err := EncodeBalanceOf(owner)
	if err != nil {
		return nil, err
	}
	to := common.Address(token)
	out, err := p.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return DecodeUint256(out), nil
}

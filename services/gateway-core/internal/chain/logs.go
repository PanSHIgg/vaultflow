package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"vaultflow/gateway-core/internal/types"
)

// ERC20TransferTopic ERC-20 Transfer(address,address,uint256) 事件签名哈希.
var ERC20TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// TransferEvent 一条解析后的 ERC-20 Transfer 日志.
type TransferEvent struct {
	TokenContract types.Address // 日志产生者合约 (log.Address)
	From          types.Address // topics[1]
	To            types.Address // topics[2]
	Amount        *big.Int      // data (uint256)
	TxHash        types.Hash
	BlockHash     types.Hash
	BlockNumber   uint64
	LogIndex      uint
}

// ParseTransferLog 将原始 eth Log 解析为 TransferEvent; 非标准 Transfer 结构返回错误.
func ParseTransferLog(l ethtypes.Log) (TransferEvent, error) {
	if l.Topics[0] != ERC20TransferTopic {
		return TransferEvent{}, errors.New("chain: not an ERC20 Transfer log")
	}
	if len(l.Topics) != 3 {
		return TransferEvent{}, fmt.Errorf("chain: Transfer log has %d topics, want 3", len(l.Topics))
	}
	if len(l.Data) != 32 {
		return TransferEvent{}, fmt.Errorf("chain: Transfer log data %d bytes, want 32", len(l.Data))
	}
	return TransferEvent{
		TokenContract: l.Address,
		From:          common.BytesToAddress(l.Topics[1].Bytes()),
		To:            common.BytesToAddress(l.Topics[2].Bytes()),
		Amount:        new(big.Int).SetBytes(l.Data),
		TxHash:        l.TxHash,
		BlockHash:     l.BlockHash,
		BlockNumber:   l.BlockNumber,
		LogIndex:      l.Index,
	}, nil
}

// ForkViewAlert 节点分叉视图/参数忽略告警载荷 (FIX-A/A1).
//
// 两种触发场景, 都说明该节点对"指定高度/哈希"的链视图不可信:
//   - 区间回退路径中返回了 block_hash != 仲裁哈希的同高度日志 (节点处于分叉上);
//   - blockHash 钉死查询返回了异哈希日志 (节点静默忽略 EIP-234 参数).
//
// 由 WithForkViewAlert 注入回调; 未注入仅静默剔除, 不影响主流程.
type ForkViewAlert struct {
	Node        string     // 节点标识
	BlockNumber uint64     // 查询目标高度
	WantHash    types.Hash // 仲裁认定的 canonical 块哈希
	GotHash     types.Hash // 最后被剔除日志的 block_hash (样本)
	Divergent   int        // 被剔除的异哈希日志条数
}

// TransferLogs 按 canonical 区块头钉死拉取 Transfer 日志 (FIX-A/A1, §4.1.2 / W4-1).
//
//   - 首选 EIP-234 eth_getLogs blockHash 参数 — 服务端按哈希精确钉块,
//     从根上杜绝"按高度拉到节点分叉视图日志"; 钉死查询返回空是合法结果
//     (该块无匹配日志 / 该节点暂无此哈希 — 仲裁哈希缺失也只影响本节点);
//   - 节点不支持 blockHash 参数 (报错) 或对钉死响应混入异哈希日志 (静默忽略
//     参数) 时, 退化为 fromBlock=toBlock=hdr.Number 的区间拉取, 再严格过滤
//     ev.BlockHash == hdr.Hash — 被剔除的分叉视图证据经 ForkViewAlert 上报;
//   - watch 集合大小 <= maxFilterAddrs 时, 使用 topics[2]=watch 地址列表的
//     服务端过滤 (OR 语义), 请求量最小; 超过阈值时退化为按白名单稳定币合约
//     全量 Transfer 日志拉取, 再在内存哈希表精确匹配;
//   - watch 集合的构造义务 (必含 payout_hot_wallet / relayer_hot_wallet) 由
//     调用方 (indexer) 承担 — 本函数只执行过滤.
//
// 按节点轮转 + 故障转移逐个尝试, 全部失败返回最后错误.
func (p *Pool) TransferLogs(ctx context.Context, hdr *BlockHeader, tokens []types.Address,
	watch []types.Address, maxFilterAddrs int) ([]TransferEvent, error) {

	if len(watch) == 0 {
		return nil, nil
	}
	watchSet := make(map[types.Address]struct{}, len(watch))
	topics2 := make([]common.Hash, 0, len(watch))
	for _, a := range watch {
		watchSet[a] = struct{}{}
		topics2 = append(topics2, common.BytesToHash(a.Bytes()))
	}

	serverSide := maxFilterAddrs <= 0 || len(watch) <= maxFilterAddrs
	topics := [][]common.Hash{{ERC20TransferTopic}}
	if serverSide {
		topics = [][]common.Hash{{ERC20TransferTopic}, nil, topics2}
	}

	wantHash := common.Hash(hdr.Hash)
	pinned := ethereum.FilterQuery{BlockHash: &wantHash, Addresses: tokens, Topics: topics}
	ranged := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(hdr.Number),
		ToBlock:   new(big.Int).SetUint64(hdr.Number),
		Addresses: tokens, Topics: topics,
	}

	var lastErr error
	for _, n := range p.nodes {
		// ① EIP-234 blockHash 钉死查询.
		logs, err := n.Cli.FilterLogs(ctx, pinned)
		if err == nil {
			var divergent int
			for _, l := range logs {
				if l.BlockHash != wantHash {
					divergent++
				}
			}
			if divergent == 0 {
				return parseTransferLogs(logs, watchSet, serverSide), nil
			}
			// blockHash 参数被静默忽略 (老节点不按 EIP-234 报错) —
			// 等同不支持, 走区间回退.
		} else {
			lastErr = fmt.Errorf("chain: pinned getLogs via %s: %w", n.Name, err)
		}

		// ② 区间回退 + 严格 blockHash 过滤 (分叉视图证据上报).
		rlogs, rerr := n.Cli.FilterLogs(ctx, ranged)
		if rerr != nil {
			lastErr = fmt.Errorf("chain: ranged getLogs via %s: %w", n.Name, rerr)
			continue
		}
		kept := make([]ethtypes.Log, 0, len(rlogs))
		var divergent int
		var divHash types.Hash
		for _, l := range rlogs {
			if l.BlockHash == wantHash {
				kept = append(kept, l)
			} else {
				divergent++
				divHash = l.BlockHash
			}
		}
		if divergent > 0 && p.forkAlert != nil {
			p.forkAlert(ForkViewAlert{
				Node: n.Name, BlockNumber: hdr.Number,
				WantHash: hdr.Hash, GotHash: divHash, Divergent: divergent,
			})
		}
		return parseTransferLogs(kept, watchSet, serverSide), nil
	}
	return nil, lastErr
}

// parseTransferLogs 解析 + 内存 watch 过滤 (非服务端过滤模式).
func parseTransferLogs(logs []ethtypes.Log, watchSet map[types.Address]struct{}, serverSide bool) []TransferEvent {
	out := make([]TransferEvent, 0, len(logs))
	for _, l := range logs {
		ev, err := ParseTransferLog(l)
		if err != nil {
			continue // 非标准 Transfer 结构, 跳过
		}
		if !serverSide {
			if _, ok := watchSet[ev.To]; !ok {
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

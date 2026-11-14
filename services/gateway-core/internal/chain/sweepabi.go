package chain

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// BatchSweeper ABI — 与 contracts/src/BatchSweeper.sol (W1) 对齐.
//   deployAndSweepBatch(SweepItem[] items, address token, address targetColdWallet)
//   SweepItem = (bytes32 salt, uint256 exactAmount), 单批 <= 50 (合约层强校验).
//   event VaultSwept(address indexed vault, bytes32 indexed salt, uint256 amount)
//   event VaultQuarantined(address indexed vault, bytes32 indexed salt, uint256 amount)
// ---------------------------------------------------------------------------

// VaultSweptTopic / VaultQuarantinedTopic 归集回执事件签名.
var (
	VaultSweptTopic       = crypto.Keccak256Hash([]byte("VaultSwept(address,bytes32,uint256)"))
	VaultQuarantinedTopic = crypto.Keccak256Hash([]byte("VaultQuarantined(address,bytes32,uint256)"))
)

// SweepItem 合约 SweepItem 元组.
type SweepItem struct {
	Salt        types.Hash
	ExactAmount *big.Int
}

// sweeperABIJSON deployAndSweepBatch 最小 ABI (仅打包所需片段).
const sweeperABIJSON = `[
  {"type":"function","name":"deployAndSweepBatch","stateMutability":"nonpayable",
   "inputs":[
     {"name":"items","type":"tuple[]","components":[
        {"name":"salt","type":"bytes32"},
        {"name":"exactAmount","type":"uint256"}]},
     {"name":"token","type":"address"},
     {"name":"targetColdWallet","type":"address"}],
   "outputs":[]}
]`

var sweeperABI = mustABI(sweeperABIJSON)

func mustABI(j string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(j))
	if err != nil {
		panic(err)
	}
	return a
}

// EncodeDeployAndSweepBatch 编码 deployAndSweepBatch calldata (含 4B selector).
func EncodeDeployAndSweepBatch(items []SweepItem, token, targetColdWallet types.Address) ([]byte, error) {
	return sweeperABI.Pack("deployAndSweepBatch", items, token, targetColdWallet)
}

// ---------------------------------------------------------------------------
// ERC-20 transfer ABI — PAYOUT_POLICY 出站 calldata (payout hot -> recipient).
// ---------------------------------------------------------------------------

var erc20ABI = mustABI(`[
  {"type":"function","name":"transfer","stateMutability":"nonpayable",
   "inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],
   "outputs":[{"name":"","type":"bool"}]},
  {"type":"function","name":"balanceOf","stateMutability":"view",
   "inputs":[{"name":"owner","type":"address"}],
   "outputs":[{"name":"","type":"uint256"}]}
]`)

// EncodeERC20Transfer 编码 transfer(to, amount) calldata.
func EncodeERC20Transfer(to types.Address, amount *big.Int) ([]byte, error) {
	return erc20ABI.Pack("transfer", to, amount)
}

// EncodeBalanceOf 编码 balanceOf(owner) calldata (水位监控/对账快照用).
func EncodeBalanceOf(owner types.Address) ([]byte, error) {
	return erc20ABI.Pack("balanceOf", owner)
}

// DecodeUint256 解码单 uint256 返回值 (balanceOf eth_call 结果).
func DecodeUint256(b []byte) *big.Int {
	if len(b) < 32 {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(b[:32])
}

// ---------------------------------------------------------------------------
// 归集回执事件解析 — indexer 在出站 FINALIZED 时调用 (W4-3 §4.2).
// ---------------------------------------------------------------------------

// SweepEventKind 归集事件类别.
type SweepEventKind int

const (
	// SweepEventSwept VaultSwept — 该 vault 定额归集成功.
	SweepEventSwept SweepEventKind = iota
	// SweepEventQuarantined VaultQuarantined — 划转 Revert 被外层捕获, 地址隔离.
	SweepEventQuarantined
)

// SweepEvent 一条解析后的归集回执事件 (topic1=vault, topic2=salt, data=amount).
type SweepEvent struct {
	Kind   SweepEventKind
	Vault  types.Address
	Salt   types.Hash
	Amount *big.Int
	TxHash types.Hash
	Index  uint
}

// ParseSweepEvents 从回执日志集中解析全部 VaultSwept/VaultQuarantined 事件
// (只识别 emitter==batchSweeper 的日志, 防伪造事件; batchSweeper 为零地址时
// 不做 emitter 校验 — 供单元测试便捷使用).
func ParseSweepEvents(logs []*ethtypes.Log, batchSweeper types.Address) []SweepEvent {
	var out []SweepEvent
	for _, l := range logs {
		if l == nil || len(l.Topics) != 3 || len(l.Data) != 32 {
			continue
		}
		var kind SweepEventKind
		switch l.Topics[0] {
		case VaultSweptTopic:
			kind = SweepEventSwept
		case VaultQuarantinedTopic:
			kind = SweepEventQuarantined
		default:
			continue
		}
		if batchSweeper != (types.Address{}) && l.Address != batchSweeper {
			continue
		}
		out = append(out, SweepEvent{
			Kind:   kind,
			Vault:  common.BytesToAddress(l.Topics[1].Bytes()),
			Salt:   l.Topics[2],
			Amount: new(big.Int).SetBytes(l.Data),
			TxHash: l.TxHash,
			Index:  l.Index,
		})
	}
	return out
}

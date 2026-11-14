package sim

// 越权盲签演练的伪造载荷构造器: EIP-2718 Type-2 未签名交易编码与
// deployAndSweepBatch / transfer calldata (用于 signer-penetration 场景).

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"vaultflow/gateway-core/internal/types"
)

// BuildUnsignedEIP1559 编码 0x02 || rlp([chainId, nonce, maxPriorityFeePerGas,
// maxFeePerGas, gasLimit, to, value, data, accessList]) — AGENTS.md §2.4 冻结布局.
// 与 docs/GOLDEN_VECTORS.json eip2718_unsigned_tx_example 逐字节对齐.
func BuildUnsignedEIP1559(chainID, nonce uint64, tip, maxFee *big.Int, gasLimit uint64, to types.Address, value *big.Int, data []byte) ([]byte, error) {
	if tip == nil {
		tip = new(big.Int)
	}
	if maxFee == nil {
		maxFee = new(big.Int)
	}
	if value == nil {
		value = new(big.Int)
	}
	payload, err := rlp.EncodeToBytes([]any{
		chainID, nonce, tip, maxFee, gasLimit,
		to, value, data, gethTypes.AccessList{},
	})
	if err != nil {
		return nil, fmt.Errorf("rlp encode: %w", err)
	}
	out := make([]byte, 1+len(payload))
	out[0] = 0x02
	copy(out[1:], payload)
	return out, nil
}

// sweepABI deployAndSweepBatch 方法 ABI (contracts/src/BatchSweeper.sol:79 冻结签名).
var sweepABI = mustParseABI(`[{
	"name": "deployAndSweepBatch",
	"type": "function",
	"inputs": [
		{"name": "items", "type": "tuple[]", "components": [
			{"name": "salt", "type": "bytes32"},
			{"name": "exactAmount", "type": "uint256"}
		]},
		{"name": "token", "type": "address"},
		{"name": "targetColdWallet", "type": "address"}
	],
	"outputs": []
}]`)

func mustParseABI(spec string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(spec))
	if err != nil {
		panic("sim: bad ABI spec: " + err.Error())
	}
	return a
}

// SweepItem 归集条目 (BatchSweeper.SweepItem).
type SweepItem struct {
	Salt        [32]byte `abi:"salt"`
	ExactAmount *big.Int `abi:"exactAmount"`
}

// BuildSweepCalldata 编码 deployAndSweepBatch(items, token, targetColdWallet).
func BuildSweepCalldata(items []SweepItem, token, coldWallet types.Address) ([]byte, error) {
	out, err := sweepABI.Pack("deployAndSweepBatch", items, token, coldWallet)
	if err != nil {
		return nil, fmt.Errorf("abi pack sweep: %w", err)
	}
	return out, nil
}

// erc20TransferABI transfer(address,uint256).
var erc20TransferABI = mustParseABI(`[{
	"name": "transfer",
	"type": "function",
	"inputs": [
		{"name": "to", "type": "address"},
		{"name": "amount", "type": "uint256"}
	],
	"outputs": [{"name": "", "type": "bool"}]
}]`)

// BuildTransferCalldata 编码 ERC-20 transfer(to, amount) (PAYOUT 伪造用).
func BuildTransferCalldata(to types.Address, amount *big.Int) ([]byte, error) {
	out, err := erc20TransferABI.Pack("transfer", to, amount)
	if err != nil {
		return nil, fmt.Errorf("abi pack transfer: %w", err)
	}
	return out, nil
}

// BuildRelayCalldataWithSalt 编码 payOrderWithAuthorization (手工定长,
// 292B = 4 + 9*32; 供 signer-penetration 的"篡改 salt"伪造,
// 与 api.BuildEIP3009RelayCalldata 同一冻结签名 — 双端独立实现互证).
func BuildRelayCalldataWithSalt(selector []byte, salt types.Hash, payer types.Address, amount, validAfter, validBefore *big.Int, nonce types.Hash, v uint8, r, s types.Hash) []byte {
	out := make([]byte, 4+9*32)
	copy(out[0:4], selector)
	copy(out[4:36], salt.Bytes())
	copy(out[36+12:68], payer.Bytes())
	fill256(out[68:100], amount)
	fill256(out[100:132], validAfter)
	fill256(out[132:164], validBefore)
	copy(out[164:196], nonce.Bytes())
	out[196+31] = v
	copy(out[228:260], r.Bytes())
	copy(out[260:292], s.Bytes())
	return out
}

func fill256(dst []byte, v *big.Int) {
	if v == nil {
		return
	}
	v.FillBytes(dst[:32])
}

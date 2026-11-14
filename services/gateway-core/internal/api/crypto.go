package api

// FROZEN 字节级契约实现 (AGENTS.md §2.1 / §2.3; docs/GOLDEN_VECTORS.json 仲裁).
// 本文件实现网关侧需要拼装的三类二进制载荷与 EIP-3009 relay calldata.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

const (
	// whitelistMagic ADDR_V1 载荷魔数 (17B ASCII).
	whitelistMagic = "VAULTFLOW_ADDR_V1"
	// refundMagic REFUND_V1 票据魔数 (19B ASCII).
	refundMagic = "VAULTFLOW_REFUND_V1"

	// WhitelistPayloadLen 69B 白名单租期载荷定长.
	WhitelistPayloadLen = 17 + 8 + 16 + 20 + 8
	// RefundTicketPayloadLen 127B 退款票据载荷定长.
	RefundTicketPayloadLen = 19 + 8 + 20 + 20 + 32 + 16 + 4 + 8
)

var (
	evmAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	bytes32Re    = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	ed25519SigRe = regexp.MustCompile(`^(0x)?[0-9a-fA-F]{128}$`)
)

// ---------------------------------------------------------------------------
// Ed25519 载荷拼装 (大端定长裸二进制, 拒绝任何 JSON 序列化)
// ---------------------------------------------------------------------------

// BuildWhitelistPayload 拼装 69B ADDR_V1 租期载荷:
// "VAULTFLOW_ADDR_V1"(17) || chain_id u64BE(8) || merchant_uuid raw16
// || recipient raw20 || valid_until_unix u64BE(8).
// 返回长度 != 69 即视为内部错误 (契约硬不变量).
func BuildWhitelistPayload(chainID uint64, merchantID uuid.UUID, recipient types.Address, validUntilUnix uint64) []byte {
	buf := make([]byte, 0, WhitelistPayloadLen)
	buf = append(buf, whitelistMagic...)
	var b8 [8]byte
	binary.BigEndian.PutUint64(b8[:], chainID)
	buf = append(buf, b8[:]...)
	buf = append(buf, merchantID[:]...)
	buf = append(buf, recipient.Bytes()...)
	binary.BigEndian.PutUint64(b8[:], validUntilUnix)
	buf = append(buf, b8[:]...)
	if len(buf) != WhitelistPayloadLen {
		panic(fmt.Sprintf("api: whitelist payload %dB != %dB", len(buf), WhitelistPayloadLen))
	}
	return buf
}

// BuildRefundTicketPayload 拼装 127B REFUND_V1 票据载荷:
// "VAULTFLOW_REFUND_V1"(19) || chain_id u64BE(8) || token raw20 || to raw20
// || exact_amount u256BE(32) || payout_uuid raw16 || retry_seq u32BE(4)
// || expiry_unix u64BE(8).
func BuildRefundTicketPayload(chainID uint64, token, to types.Address, exactAmount types.U256, payoutUUID uuid.UUID, retrySeq uint32, expiryUnix uint64) []byte {
	buf := make([]byte, 0, RefundTicketPayloadLen)
	buf = append(buf, refundMagic...)
	var b8 [8]byte
	binary.BigEndian.PutUint64(b8[:], chainID)
	buf = append(buf, b8[:]...)
	buf = append(buf, token.Bytes()...)
	buf = append(buf, to.Bytes()...)
	var amt [32]byte
	exactAmount.Big().FillBytes(amt[:])
	buf = append(buf, amt[:]...)
	buf = append(buf, payoutUUID[:]...)
	var b4 [4]byte
	binary.BigEndian.PutUint32(b4[:], retrySeq)
	buf = append(buf, b4[:]...)
	binary.BigEndian.PutUint64(b8[:], expiryUnix)
	buf = append(buf, b8[:]...)
	if len(buf) != RefundTicketPayloadLen {
		panic(fmt.Sprintf("api: refund ticket payload %dB != %dB", len(buf), RefundTicketPayloadLen))
	}
	return buf
}

// ---------------------------------------------------------------------------
// CREATE2 / EIP-3009 派生 (AGENTS.md §2.1 冻结公式)
// ---------------------------------------------------------------------------

// AuthNonce EIP-3009 授权 nonce = keccak256(abi.encode(uint256(chainId), bytes32(salt))).
// abi.encode 静态类型定长: 32B 大端 chainId 左填充 || 32B salt, 共 64B.
func AuthNonce(chainID uint64, salt types.Hash) types.Hash {
	var buf [64]byte
	binary.BigEndian.PutUint64(buf[24:32], chainID) // uint256 大端右对齐
	copy(buf[32:64], salt.Bytes())
	return types.Hash(crypto.Keccak256Hash(buf[:]))
}

// eip3009RelaySelector = keccak256("payOrderWithAuthorization(bytes32,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)")[:4]
// (contracts/src/EIP3009PayRouter.sol:53 冻结签名).
var eip3009RelaySelector = crypto.Keccak256(
	[]byte("payOrderWithAuthorization(bytes32,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)"),
)[:4]

// BuildEIP3009RelayCalldata ABI 编码 payOrderWithAuthorization 调用 (4B selector
// + 9 × 32B 静态参数 = 292B). 参数全部静态类型, 手工定长编码与 abi.encode 逐字节一致.
func BuildEIP3009RelayCalldata(salt types.Hash, payer types.Address, amount, validAfter, validBefore *big.Int, nonce types.Hash, v uint8, r, s types.Hash) []byte {
	out := make([]byte, 4+9*32)
	copy(out[0:4], eip3009RelaySelector)
	copy(out[4:36], salt.Bytes())
	// address: 12B 左零填充 + 20B
	copy(out[36+12:68], payer.Bytes())
	putU256(out[68:100], amount)
	putU256(out[100:132], validAfter)
	putU256(out[132:164], validBefore)
	copy(out[164:196], nonce.Bytes())
	out[196+31] = v // uint8 右对齐于 32B 槽
	copy(out[228:260], r.Bytes())
	copy(out[260:292], s.Bytes())
	return out
}

func putU256(dst []byte, v *big.Int) {
	if v == nil {
		return
	}
	v.FillBytes(dst[:32])
}

// ---------------------------------------------------------------------------
// 严格 hex 解析 (openapi pattern 校验 — 拒绝宽松截断)
// ---------------------------------------------------------------------------

// parseEVMAddress 校验 ^0x[0-9a-fA-F]{40}$ 并解析; 非法返回 error.
func parseEVMAddress(s string) (types.Address, error) {
	if !evmAddressRe.MatchString(s) {
		return types.Address{}, fmt.Errorf("invalid EVM address %q", s)
	}
	return types.HexToAddress(s), nil
}

// parseBytes32 校验 ^0x[0-9a-fA-F]{64}$ 并解析为 32B 哈希.
func parseBytes32(s string) (types.Hash, error) {
	if !bytes32Re.MatchString(s) {
		return types.Hash{}, fmt.Errorf("invalid bytes32 %q", s)
	}
	return types.HexToHash(s), nil
}

// parseEd25519Signature 校验 (0x)?128-hex 并解码为 64B 裸签名.
func parseEd25519Signature(s string) ([]byte, error) {
	if !ed25519SigRe.MatchString(s) {
		return nil, fmt.Errorf("invalid Ed25519 signature encoding")
	}
	raw := s
	if len(raw) > 2 && raw[:2] == "0x" {
		raw = raw[2:]
	}
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != 64 {
		return nil, fmt.Errorf("invalid Ed25519 signature bytes")
	}
	return b, nil
}

// lowerHexAddr 地址输出一律 0x 前缀小写 (AGENTS §2.5.3).
func lowerHexAddr(a types.Address) string { return "0x" + hex.EncodeToString(a.Bytes()) }

// lowerHexHash 哈希输出一律 0x 前缀小写.
func lowerHexHash(h types.Hash) string { return "0x" + hex.EncodeToString(h.Bytes()) }

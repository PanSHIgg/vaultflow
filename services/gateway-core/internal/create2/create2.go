// Package create2 实现 VaultFlow 离线 CREATE2 地址推导内核 (W3-1).
//
// 字节级契约 (AGENTS.md §2.1 / §2.2, FROZEN):
//
//	salt           = keccak256(abi.encode(uint256(chainId), bytes16(merchantId), bytes16(orderId)))
//	auth_nonce     = keccak256(abi.encode(uint256(chainId), bytes32(salt)))
//	init_code      = 0x3d602d80600a3d3981f3 || 363d3d373d3d3d363d73 || <impl 20B> || 5af43d82803e903d91602b57fd5bf3
//	init_code_hash = keccak256(init_code)
//	vault_address  = last20(keccak256(0xff || factory(20B) || salt(32B) || init_code_hash(32B)))
//
// bytes16 在 abi.encode 中一律右侧补零至 32B, 编码后 blob =
// chainId(32B BE) || merchantId(16B) || 0x00*16 || orderId(16B) || 0x00*16 (96B).
// 全部断言基准: docs/GOLDEN_VECTORS.json.
package create2

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// EIP-1167 最小代理字节码模板常量 (§2.2 FROZEN).
var (
	// initCodePrefix 10B creation prefix: 0x3d602d80600a3d3981f3.
	initCodePrefix = common.FromHex("0x3d602d80600a3d3981f3")
	// initCodeRuntimePrefix 10B runtime prefix: 0x363d3d373d3d3d363d73.
	initCodeRuntimePrefix = common.FromHex("0x363d3d373d3d3d363d73")
	// initCodeSuffix 15B runtime suffix: 0x5af43d82803e903d91602b57fd5bf3.
	initCodeSuffix = common.FromHex("0x5af43d82803e903d91602b57fd5bf3")
)

// InitCodeLen 55B creation code (45B runtime).
const (
	InitCodeLen       = 55
	RuntimeCodeLen    = 45
	saltPayloadLen    = 96 // 32 + (16+16) + (16+16)
	authNonceInputLen = 64 // 32 + 32
	create2PayloadLen = 85 // 1 + 20 + 32 + 32
)

// Deriver 单链 CREATE2 推导器: 固化 factory / implementation / init_code_hash,
// 供订单下单离线推导 DepositAddr 与 EIP-3009 authNonce.
// 不可变, 并发安全.
type Deriver struct {
	factory        types.Address
	implementation types.Address
	initCode       []byte // 55B
	initCodeHash   types.Hash
}

// NewDeriver 构造推导器并预计算 init_code 与 init_code_hash.
func NewDeriver(factory, implementation types.Address) *Deriver {
	initCode := BuildInitCode(implementation)
	return &Deriver{
		factory:        factory,
		implementation: implementation,
		initCode:       initCode,
		initCodeHash:   crypto.Keccak256Hash(initCode),
	}
}

// Factory 返回绑定的 VaultFactory 地址.
func (d *Deriver) Factory() types.Address { return d.factory }

// Implementation 返回绑定的 DepositVault 实现地址.
func (d *Deriver) Implementation() types.Address { return d.implementation }

// InitCode 返回 55B 确定性 creation code (只读拷贝).
func (d *Deriver) InitCode() []byte {
	out := make([]byte, len(d.initCode))
	copy(out, d.initCode)
	return out
}

// InitCodeHash 返回 keccak256(init_code). implementation 不变时永久恒定
// (热钱包/冷金库 48h Timelock 轮换不影响, ARCHITECTURE §2.1).
func (d *Deriver) InitCodeHash() types.Hash { return d.initCodeHash }

// ---------------------------------------------------------------------------
// FROZEN 公式 (包级函数, 便于无配置场景单独调用)
// ---------------------------------------------------------------------------

// DeriveSalt 计算 CREATE2 salt = keccak256(abi.encode(chainId, merchantId, orderId)).
// abi.encode(uint256, bytes16, bytes16) = 96B blob:
// chainId 32B 大端 || merchantId 16B 左对齐右补零 || orderId 16B 左对齐右补零.
// 严禁左填充 (那是 uintN 的规则), 严禁 encodePacked 紧凑编码.
func DeriveSalt(chainID types.ChainID, merchantID, orderID uuid.UUID) types.Hash {
	var blob [saltPayloadLen]byte
	new(big.Int).SetUint64(uint64(chainID)).FillBytes(blob[0:32])
	copy(blob[32:48], merchantID[:]) // bytes16: 高位对齐, [48:64] 保持 0x00
	copy(blob[64:80], orderID[:])    // [80:96] 保持 0x00
	return crypto.Keccak256Hash(blob[:])
}

// DeriveAuthNonce 计算 EIP-3009 ReceiveWithAuthorization 的 nonce =
// keccak256(abi.encode(chainId, salt)) = keccak256(chainId 32B BE || salt 32B).
// 链上 Router 以同一公式自算强校验.
func DeriveAuthNonce(chainID types.ChainID, salt types.Hash) types.Hash {
	var blob [authNonceInputLen]byte
	new(big.Int).SetUint64(uint64(chainID)).FillBytes(blob[0:32])
	copy(blob[32:64], salt[:])
	return crypto.Keccak256Hash(blob[:])
}

// BuildInitCode 拼接 55B EIP-1167 creation code (模板占位符仅允许替换为
// 20B implementation 地址, 前后缀字节严禁改动).
func BuildInitCode(implementation types.Address) []byte {
	init := make([]byte, 0, InitCodeLen)
	init = append(init, initCodePrefix...)         // 10B creation prefix
	init = append(init, initCodeRuntimePrefix...)  // 10B runtime prefix
	init = append(init, implementation.Bytes()...) // 20B
	init = append(init, initCodeSuffix...)         // 15B runtime suffix
	return init
}

// DeriveVaultAddress CREATE2 推导:
// last20(keccak256(0xff || factory 20B || salt 32B || init_code_hash 32B)).
func DeriveVaultAddress(factory types.Address, salt, initCodeHash types.Hash) types.Address {
	return crypto.CreateAddress2(factory, salt, initCodeHash[:])
}

// ---------------------------------------------------------------------------
// Deriver 便捷方法 (绑定 factory / implementation)
// ---------------------------------------------------------------------------

// Salt 计算订单专属 salt.
func (d *Deriver) Salt(chainID types.ChainID, merchantID, orderID uuid.UUID) types.Hash {
	return DeriveSalt(chainID, merchantID, orderID)
}

// AuthNonce 计算订单的 EIP-3009 auth_nonce.
func (d *Deriver) AuthNonce(chainID types.ChainID, salt types.Hash) types.Hash {
	return DeriveAuthNonce(chainID, salt)
}

// VaultAddress 由 salt 推导 DepositVault 地址.
func (d *Deriver) VaultAddress(salt types.Hash) types.Address {
	return DeriveVaultAddress(d.factory, salt, d.initCodeHash)
}

// VaultAddressForOrder 一步推导订单专属 DepositAddr.
func (d *Deriver) VaultAddressForOrder(chainID types.ChainID, merchantID, orderID uuid.UUID) types.Address {
	return d.VaultAddress(d.Salt(chainID, merchantID, orderID))
}

// ---------------------------------------------------------------------------
// 启动期链上强断言 (W3-1 / ARCHITECTURE §2.1)
// ---------------------------------------------------------------------------

// ErrFactoryHashMismatch 链上 initCodeHash 与本地配置不一致: 必须拒绝启动.
var ErrFactoryHashMismatch = errors.New("create2: on-chain VaultFactory.initCodeHash() mismatches local configuration")

// FactoryInitCodeQuerier 链上 initCodeHash() 查询抽象.
// 生产端由 W4 chain RPC 客户端实现 (eth_call VaultFactory.initCodeHash());
// 测试端可 mock. 保持在本包定义以避免反向依赖.
type FactoryInitCodeQuerier interface {
	InitCodeHash(ctx context.Context, factory types.Address) (types.Hash, error)
}

// VerifyChainFactoryHash 启动校验: 调用链上 VaultFactory.initCodeHash()
// 与本地由 implementation 推导的 init_code_hash 逐字节比对, 不一致返回
// ErrFactoryHashMismatch (调用方应立即拒绝启动).
func (d *Deriver) VerifyChainFactoryHash(ctx context.Context, q FactoryInitCodeQuerier) error {
	onchain, err := q.InitCodeHash(ctx, d.factory)
	if err != nil {
		return fmt.Errorf("create2: query on-chain initCodeHash: %w", err)
	}
	if onchain != d.initCodeHash {
		return fmt.Errorf("%w (chain=%s, local=%s)", ErrFactoryHashMismatch, onchain.Hex(), d.initCodeHash.Hex())
	}
	return nil
}

package types

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// ChainID EVM 链 ID (chain_id 列, bigint).
type ChainID uint64

// Address EVM 20 字节地址. 直接别名 go-ethereum 类型, 保证跨模块
// (W3 create2 / W4 chain / W5 api) 字节级一致, 免费获得 Hex/Bytes 等方法.
type Address = common.Address

// Hash EVM 32 字节哈希 (block_hash / tx_hash / create2_salt / audit_tip_hash).
type Hash = common.Hash

// HexToAddress 解析 0x 前缀十六进制地址 (非法字符按 go-ethereum 语义截断清零).
func HexToAddress(s string) Address { return common.HexToAddress(s) }

// HexToHash 解析 0x 前缀十六进制哈希.
func HexToHash(s string) Hash { return common.HexToHash(s) }

// SystemMerchantID 系统级账本账户 (热钱包/金库/费用/权益类科目) 统一使用的
// 全零哨兵商户 UUID, 见 P0-1 数值与哨兵约定.
var SystemMerchantID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

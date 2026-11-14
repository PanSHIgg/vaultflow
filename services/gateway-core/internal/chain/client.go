// Package chain implements W4-1: 多节点 RPC 仲裁池与自适应 Transfer 日志过滤器.
//
// NodeClient 是单节点 JSON-RPC 能力的最小抽象. 生产实现 EthNodeClient 基于
// stdlib net/http 手写 JSON-RPC — 仓库 go.sum 未包含 go-ethereum ethclient/rpc
// 的传递依赖 (otel/websocket 等), 受 go.mod 铁律约束不可引入, 故以 ~200 行
// 自实现传输层满足全部所需方法; 单测用内存 fake 注入保证零外部依赖.
package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"vaultflow/gateway-core/internal/types"
)

// BlockTag 特殊区块标签, 数值与 go-ethereum rpc.BlockNumber 对齐.
type BlockTag int64

const (
	TagLatest    BlockTag = -1 // latest (不安全, 仅用于进度探测)
	TagPending   BlockTag = -2 // pending
	TagFinalized BlockTag = -3 // finalized (PoS 最终确认)
	TagSafe      BlockTag = -4 // safe (LIFO 回滚安全线, §4.1)
)

// Num 将 tag 编码为 JSON-RPC 数字参数 (负值为特殊标签).
func (t BlockTag) Num() *big.Int { return big.NewInt(int64(t)) }

// String 将 tag 编码为 JSON-RPC 字符串标签.
func (t BlockTag) String() string {
	switch t {
	case TagLatest:
		return "latest"
	case TagPending:
		return "pending"
	case TagFinalized:
		return "finalized"
	case TagSafe:
		return "safe"
	}
	return fmt.Sprintf("0x%x", int64(t))
}

// BlockHeader 仲裁/同步所需的区块头最小投影 (hash/parent_hash 驱动 LIFO 判定).
type BlockHeader struct {
	Number     uint64
	Hash       types.Hash
	ParentHash types.Hash
}

// NodeClient 单节点 RPC 读取/广播接口. *EthNodeClient 为生产实现.
type NodeClient interface {
	// BlockNumber 最新区块高度 (eth_blockNumber).
	BlockNumber(ctx context.Context) (uint64, error)
	// HeaderByTag 按标签取区块头 (safe / finalized / latest).
	HeaderByTag(ctx context.Context, tag BlockTag) (*BlockHeader, error)
	// HeaderByNumber 按高度取区块头; 不存在返回 ethereum.NotFound.
	HeaderByNumber(ctx context.Context, num uint64) (*BlockHeader, error)
	// FilterLogs eth_getLogs.
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]ethtypes.Log, error)
	// TransactionReceipt eth_getTransactionReceipt; 未入块返回 ethereum.NotFound.
	TransactionReceipt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error)
	// TxVisibility eth_getTransactionByHash 可见性: (found, pending, err).
	// found=false 表示节点既无 mempool 副本也无入块记录.
	TxVisibility(ctx context.Context, hash types.Hash) (found bool, pending bool, err error)
	// NonceAt eth_getTransactionCount; blockNumber 为 nil 时按 latest.
	NonceAt(ctx context.Context, addr types.Address, blockNumber *big.Int) (uint64, error)
	// PendingNonceAt eth_getTransactionCount("pending") — 含 mempool 的下一可用 Nonce.
	PendingNonceAt(ctx context.Context, addr types.Address) (uint64, error)
	// BalanceAt eth_getBalance; blockNumber 为 nil 时按 latest.
	BalanceAt(ctx context.Context, addr types.Address, blockNumber *big.Int) (*big.Int, error)
	// CallContract eth_call (只读调用, 如 balanceOf); blockNumber nil 即 latest.
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	// SendRawTransaction eth_sendRawTransaction, 返回 tx_hash.
	SendRawTransaction(ctx context.Context, raw []byte) (types.Hash, error)
	// Close 释放底层连接 (无连接实现可为 no-op).
	Close()
}

// ---------------------------------------------------------------------------
// EthNodeClient — stdlib net/http JSON-RPC 实现
// ---------------------------------------------------------------------------

type rpcEnvelope struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int64         `json:"id"`
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ID int64 `json:"id"`
}

// EthNodeClient HTTP JSON-RPC 节点客户端.
type EthNodeClient struct {
	url    string
	hc     *http.Client
	nextID atomic.Int64
}

// DialEthNode 创建节点客户端 (http/https endpoint; ws 不支持 — Wave-1 轮询架构无需).
func DialEthNode(ctx context.Context, rawurl string) (*EthNodeClient, error) {
	if !strings.HasPrefix(rawurl, "http://") && !strings.HasPrefix(rawurl, "https://") {
		return nil, fmt.Errorf("chain: only http(s) rpc endpoints supported, got %q", rawurl)
	}
	c := &EthNodeClient{
		url: rawurl,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}
	// 探活: eth_chainId 一次即可, 失败立即报错 (装配期 fail-fast).
	var dead any
	if err := c.call(ctx, &dead, "eth_chainId"); err != nil {
		return nil, fmt.Errorf("chain: probe %s: %w", rawurl, err)
	}
	return c, nil
}

// URL 返回节点连接串.
func (c *EthNodeClient) URL() string { return c.url }

func (c *EthNodeClient) call(ctx context.Context, result interface{}, method string, params ...interface{}) error {
	id := c.nextID.Add(1)
	reqBody, err := json.Marshal(rpcEnvelope{JSONRPC: "2.0", Method: method, Params: params, ID: id})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	var r rpcReply
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("chain: decode rpc reply: %w (body %.200s)", err, body)
	}
	if r.Error != nil {
		return fmt.Errorf("chain: rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return ethereum.NotFound
	}
	if result != nil {
		if err := json.Unmarshal(r.Result, result); err != nil {
			return fmt.Errorf("chain: decode %s result: %w", method, err)
		}
	}
	return nil
}

// callNullable 与 call 相同但 null result 返回 (false, nil).
func (c *EthNodeClient) callNullable(ctx context.Context, result interface{}, method string, params ...interface{}) (bool, error) {
	id := c.nextID.Add(1)
	reqBody, err := json.Marshal(rpcEnvelope{JSONRPC: "2.0", Method: method, Params: params, ID: id})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return false, err
	}
	var r rpcReply
	if err := json.Unmarshal(body, &r); err != nil {
		return false, fmt.Errorf("chain: decode rpc reply: %w", err)
	}
	if r.Error != nil {
		return false, fmt.Errorf("chain: rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return false, err
	}
	return true, nil
}

func (c *EthNodeClient) BlockNumber(ctx context.Context) (uint64, error) {
	var s string
	if err := c.call(ctx, &s, "eth_blockNumber"); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(s)
}

type rpcHeader struct {
	Number     string `json:"number"`
	Hash       string `json:"hash"`
	ParentHash string `json:"parentHash"`
}

func (h *rpcHeader) hdr() (*BlockHeader, error) {
	n, err := hexutil.DecodeUint64(h.Number)
	if err != nil {
		return nil, err
	}
	return &BlockHeader{
		Number:     n,
		Hash:       types.HexToHash(h.Hash),
		ParentHash: types.HexToHash(h.ParentHash),
	}, nil
}

func (c *EthNodeClient) getBlock(ctx context.Context, tag string) (*BlockHeader, error) {
	var h rpcHeader
	ok, err := c.callNullable(ctx, &h, "eth_getBlockByNumber", tag, false)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ethereum.NotFound
	}
	return h.hdr()
}

func (c *EthNodeClient) HeaderByTag(ctx context.Context, tag BlockTag) (*BlockHeader, error) {
	return c.getBlock(ctx, tag.String())
}

func (c *EthNodeClient) HeaderByNumber(ctx context.Context, num uint64) (*BlockHeader, error) {
	return c.getBlock(ctx, hexutil.EncodeUint64(num))
}

// rpcLog JSON-RPC 日志结构.
type rpcLog struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	BlockNumber      string   `json:"blockNumber"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex string   `json:"transactionIndex"`
	BlockHash        string   `json:"blockHash"`
	LogIndex         string   `json:"logIndex"`
	Removed          bool     `json:"removed"`
}

func (l *rpcLog) toLog() (ethtypes.Log, error) {
	out := ethtypes.Log{Address: common.HexToAddress(l.Address)}
	for _, t := range l.Topics {
		out.Topics = append(out.Topics, common.HexToHash(t))
	}
	var err error
	out.Data, err = hexutil.Decode(l.Data)
	if err != nil {
		return out, err
	}
	if out.BlockNumber, err = hexutil.DecodeUint64(l.BlockNumber); err != nil {
		return out, err
	}
	out.TxHash = common.HexToHash(l.TransactionHash)
	out.BlockHash = common.HexToHash(l.BlockHash)
	if l.LogIndex != "" {
		if v, e := hexutil.DecodeUint64(l.LogIndex); e == nil {
			out.Index = uint(v)
		}
	}
	if l.TransactionIndex != "" {
		if v, e := hexutil.DecodeUint64(l.TransactionIndex); e == nil {
			out.TxIndex = uint(v)
		}
	}
	out.Removed = l.Removed
	return out, nil
}

func (c *EthNodeClient) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]ethtypes.Log, error) {
	arg := map[string]interface{}{}
	if q.BlockHash != nil {
		// EIP-234: blockHash 与 fromBlock/toBlock 互斥, 只发 blockHash.
		arg["blockHash"] = q.BlockHash.Hex()
	} else {
		if q.FromBlock != nil {
			arg["fromBlock"] = hexutil.EncodeBig(q.FromBlock)
		}
		if q.ToBlock != nil {
			arg["toBlock"] = hexutil.EncodeBig(q.ToBlock)
		}
	}
	if len(q.Addresses) > 0 {
		if len(q.Addresses) == 1 {
			arg["address"] = q.Addresses[0].Hex()
		} else {
			addrs := make([]string, len(q.Addresses))
			for i, a := range q.Addresses {
				addrs[i] = a.Hex()
			}
			arg["address"] = addrs
		}
	}
	if len(q.Topics) > 0 {
		topics := make([]interface{}, len(q.Topics))
		for i, set := range q.Topics {
			if len(set) == 0 {
				topics[i] = nil
			} else if len(set) == 1 {
				topics[i] = set[0].Hex()
			} else {
				arr := make([]string, len(set))
				for j, t := range set {
					arr[j] = t.Hex()
				}
				topics[i] = arr
			}
		}
		arg["topics"] = topics
	}
	var logs []rpcLog
	ok, err := c.callNullable(ctx, &logs, "eth_getLogs", arg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	out := make([]ethtypes.Log, 0, len(logs))
	for i := range logs {
		l, err := logs[i].toLog()
		if err != nil {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

type rpcReceipt struct {
	TransactionHash   string   `json:"transactionHash"`
	BlockHash         string   `json:"blockHash"`
	BlockNumber       string   `json:"blockNumber"`
	Status            string   `json:"status"`
	GasUsed           string   `json:"gasUsed"`
	EffectiveGasPrice string   `json:"effectiveGasPrice"`
	Logs              []rpcLog `json:"logs"`
}

func (c *EthNodeClient) TransactionReceipt(ctx context.Context, hash types.Hash) (*ethtypes.Receipt, error) {
	var r rpcReceipt
	ok, err := c.callNullable(ctx, &r, "eth_getTransactionReceipt", hash.Hex())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ethereum.NotFound
	}
	out := &ethtypes.Receipt{
		TxHash:           common.HexToHash(r.TransactionHash),
		BlockHash:        common.HexToHash(r.BlockHash),
		ContractAddress:  common.Address{},
		TransactionIndex: 0,
	}
	if v, err := hexutil.DecodeUint64(r.BlockNumber); err == nil {
		out.BlockNumber = new(big.Int).SetUint64(v)
	}
	if strings.HasPrefix(r.Status, "0x") {
		if v, err := hexutil.DecodeUint64(r.Status); err == nil {
			out.Status = v
		}
	} else if v, err := hexutil.DecodeUint64("0x" + r.Status); err == nil {
		out.Status = v
	}
	if v, err := hexutil.DecodeUint64(r.GasUsed); err == nil {
		out.GasUsed = v
	}
	if r.EffectiveGasPrice != "" {
		if v, err := hexutil.DecodeBig(r.EffectiveGasPrice); err == nil {
			out.EffectiveGasPrice = v
		}
	}
	for i := range r.Logs {
		l, err := r.Logs[i].toLog()
		if err != nil {
			continue
		}
		out.Logs = append(out.Logs, &l)
	}
	return out, nil
}

type rpcTx struct {
	BlockHash   string  `json:"blockHash"`
	BlockNumber *string `json:"blockNumber"` // null = pending
}

func (c *EthNodeClient) TxVisibility(ctx context.Context, hash types.Hash) (bool, bool, error) {
	var t rpcTx
	ok, err := c.callNullable(ctx, &t, "eth_getTransactionByHash", hash.Hex())
	if err != nil {
		return false, false, err
	}
	if !ok {
		return false, false, nil
	}
	return true, t.BlockNumber == nil, nil
}

func (c *EthNodeClient) NonceAt(ctx context.Context, addr types.Address, blockNumber *big.Int) (uint64, error) {
	tag := "latest"
	if blockNumber != nil {
		tag = hexutil.EncodeBig(blockNumber)
	}
	var s string
	if err := c.call(ctx, &s, "eth_getTransactionCount", addr.Hex(), tag); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(s)
}

func (c *EthNodeClient) PendingNonceAt(ctx context.Context, addr types.Address) (uint64, error) {
	var s string
	if err := c.call(ctx, &s, "eth_getTransactionCount", addr.Hex(), "pending"); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(s)
}

func (c *EthNodeClient) BalanceAt(ctx context.Context, addr types.Address, blockNumber *big.Int) (*big.Int, error) {
	tag := "latest"
	if blockNumber != nil {
		tag = hexutil.EncodeBig(blockNumber)
	}
	var s string
	if err := c.call(ctx, &s, "eth_getBalance", addr.Hex(), tag); err != nil {
		return nil, err
	}
	return hexutil.DecodeBig(s)
}

func (c *EthNodeClient) CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	arg := map[string]interface{}{}
	if msg.To != nil {
		arg["to"] = msg.To.Hex()
	}
	if msg.From != (common.Address{}) {
		arg["from"] = msg.From.Hex()
	}
	if len(msg.Data) > 0 {
		arg["data"] = hexutil.Encode(msg.Data)
	}
	if msg.Value != nil && msg.Value.Sign() > 0 {
		arg["value"] = hexutil.EncodeBig(msg.Value)
	}
	tag := "latest"
	if blockNumber != nil {
		tag = hexutil.EncodeBig(blockNumber)
	}
	var s string
	if err := c.call(ctx, &s, "eth_call", arg, tag); err != nil {
		return nil, err
	}
	return hexutil.Decode(s)
}

func (c *EthNodeClient) SendRawTransaction(ctx context.Context, raw []byte) (types.Hash, error) {
	var s string
	if err := c.call(ctx, &s, "eth_sendRawTransaction", hexutil.Encode(raw)); err != nil {
		return types.Hash{}, err
	}
	return types.HexToHash(s), nil
}

// IsAlreadyKnownError 判断广播错误是否为"交易已在 mempool/已知"(幂等重放场景).
func IsAlreadyKnownError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already known") ||
		strings.Contains(s, "known transaction") ||
		strings.Contains(s, "tx already in") ||
		strings.Contains(s, "transaction already imported") ||
		strings.Contains(s, "alreadyknown")
}

// IsNotFoundError 将 RPC 未找到语义归一为 ethereum.NotFound 判定.
func IsNotFoundError(err error) bool {
	return errors.Is(err, ethereum.NotFound)
}

// Close HTTP 客户端无持久连接需关闭.
func (c *EthNodeClient) Close() {
	if c.hc != nil {
		c.hc.CloseIdleConnections()
	}
}

// Package sim 实现 ports.SimulatorService: Anvil 测试链底层控制 +
// ARCHITECTURE.md §8 四大对抗演练场景编排 (仅 ENABLE_DEV_SIMULATOR=true 启用).
package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// AnvilClient — 最小 JSON-RPC 客户端 (evm_*/anvil_* 控制面 + 少量 eth_* 读取)
// ---------------------------------------------------------------------------

// AnvilClient 单实例 JSON-RPC 封装 (dev-drill 低并发, 顺序 id 即可).
type AnvilClient struct {
	url    string
	hc     *http.Client
	nextID atomic.Uint64
}

// NewAnvilClient 构造客户端; hc 为 nil 时使用默认 10s 超时 client.
func NewAnvilClient(rpcURL string, hc *http.Client) *AnvilClient {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &AnvilClient{url: rpcURL, hc: hc}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Call 通用 JSON-RPC 调用: 返回 result 原始 JSON (null-safe).
func (c *AnvilClient) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	reqBody, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0", ID: c.nextID.Add(1), Method: method, Params: params,
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anvil rpc %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var rpcResp rpcResponse
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, fmt.Errorf("anvil rpc %s: undecodable response: %w", method, err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("anvil rpc %s: [%d] %s", method, rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// ---------------------------------------------------------------------------
// evm_/anvil_ 控制面 (W5-3 冻结方法集)
// ---------------------------------------------------------------------------

// Snapshot evm_snapshot -> snapshot id (hex quantity).
func (c *AnvilClient) Snapshot(ctx context.Context) (string, error) {
	res, err := c.Call(ctx, "evm_snapshot")
	if err != nil {
		return "", err
	}
	var id string
	if err := json.Unmarshal(res, &id); err != nil {
		return "", fmt.Errorf("evm_snapshot result: %w", err)
	}
	return id, nil
}

// Revert evm_revert -> bool (回滚至快照点; 之后新挖块构成分叉链).
func (c *AnvilClient) Revert(ctx context.Context, snapshotID string) (bool, error) {
	res, err := c.Call(ctx, "evm_revert", snapshotID)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := json.Unmarshal(res, &ok); err != nil {
		return false, fmt.Errorf("evm_revert result: %w", err)
	}
	return ok, nil
}

// Mine anvil_mine — blocks 为 0 时用默认 1 块.
func (c *AnvilClient) Mine(ctx context.Context, blocks uint64) error {
	if blocks == 0 {
		blocks = 1
	}
	_, err := c.Call(ctx, "anvil_mine", hexQuantity(blocks))
	return err
}

// SetAutomine anvil_setAutomine.
func (c *AnvilClient) SetAutomine(ctx context.Context, on bool) error {
	_, err := c.Call(ctx, "anvil_setAutomine", on)
	return err
}

// DropTransaction anvil_dropTransaction — 从 mempool 驱逐一笔交易 (Dropped 演练).
func (c *AnvilClient) DropTransaction(ctx context.Context, txHash types.Hash) (bool, error) {
	res, err := c.Call(ctx, "anvil_dropTransaction", lowerHash(txHash))
	if err != nil {
		return false, err
	}
	var ok bool
	if err := json.Unmarshal(res, &ok); err != nil {
		return false, fmt.Errorf("anvil_dropTransaction result: %w", err)
	}
	return ok, nil
}

// ImpersonateAccount anvil_impersonateAccount — dev 账号无签名发交易.
func (c *AnvilClient) ImpersonateAccount(ctx context.Context, addr types.Address) error {
	_, err := c.Call(ctx, "anvil_impersonateAccount", lowerAddr(addr))
	return err
}

// StopImpersonating anvil_stopImpersonatingAccount.
func (c *AnvilClient) StopImpersonating(ctx context.Context, addr types.Address) error {
	_, err := c.Call(ctx, "anvil_stopImpersonatingAccount", lowerAddr(addr))
	return err
}

// SetBalance anvil_setBalance — 给地址注入原生币 (演练注资).
func (c *AnvilClient) SetBalance(ctx context.Context, addr types.Address, weiHex string) error {
	_, err := c.Call(ctx, "anvil_setBalance", lowerAddr(addr), weiHex)
	return err
}

// ---------------------------------------------------------------------------
// eth_* 读取 (场景断言与注入交易)
// ---------------------------------------------------------------------------

// BlockNumber eth_blockNumber.
func (c *AnvilClient) BlockNumber(ctx context.Context) (uint64, error) {
	res, err := c.Call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	return parseHexQuantity(res)
}

// SendTransaction eth_sendTransaction -> tx hash (配合 impersonated 账号).
func (c *AnvilClient) SendTransaction(ctx context.Context, tx map[string]any) (types.Hash, error) {
	res, err := c.Call(ctx, "eth_sendTransaction", tx)
	if err != nil {
		return types.Hash{}, err
	}
	var h string
	if err := json.Unmarshal(res, &h); err != nil {
		return types.Hash{}, fmt.Errorf("eth_sendTransaction result: %w", err)
	}
	return types.HexToHash(h), nil
}

// Receipt eth_getTransactionReceipt 最小投影.
type Receipt struct {
	TransactionHash   types.Hash
	BlockNumber       uint64
	Status            int32 // 1 success / 0 revert
	GasUsed           uint64
	EffectiveGasPrice *big.Int
}

// GetReceipt 查询回执; 未上链返回 (nil, nil).
func (c *AnvilClient) GetReceipt(ctx context.Context, txHash types.Hash) (*Receipt, error) {
	res, err := c.Call(ctx, "eth_getTransactionReceipt", lowerHash(txHash))
	if err != nil {
		return nil, err
	}
	if string(res) == "null" {
		return nil, nil
	}
	var raw struct {
		TransactionHash   string `json:"transactionHash"`
		BlockNumber       string `json:"blockNumber"`
		Status            string `json:"status"`
		GasUsed           string `json:"gasUsed"`
		EffectiveGasPrice string `json:"effectiveGasPrice"`
	}
	if err := json.Unmarshal(res, &raw); err != nil {
		return nil, fmt.Errorf("receipt decode: %w", err)
	}
	rc := &Receipt{TransactionHash: types.HexToHash(raw.TransactionHash)}
	rc.BlockNumber = mustHexU64(raw.BlockNumber)
	rc.Status = int32(mustHexU64(raw.Status))
	rc.GasUsed = mustHexU64(raw.GasUsed)
	if raw.EffectiveGasPrice != "" {
		v, _ := new(big.Int).SetString(strings.TrimPrefix(raw.EffectiveGasPrice, "0x"), 16)
		rc.EffectiveGasPrice = v
	}
	return rc, nil
}

// GetBalance eth_getBalance -> wei.
func (c *AnvilClient) GetBalance(ctx context.Context, addr types.Address, blockTag string) (*big.Int, error) {
	if blockTag == "" {
		blockTag = "latest"
	}
	res, err := c.Call(ctx, "eth_getBalance", lowerAddr(addr), blockTag)
	if err != nil {
		return nil, err
	}
	var hexStr string
	if err := json.Unmarshal(res, &hexStr); err != nil {
		return nil, err
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(hexStr, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("eth_getBalance undecodable %q", hexStr)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// AnvilRouter — 多链路由 + openapi simAnvil action 透传 (api.AnvilPassthrough)
// ---------------------------------------------------------------------------

// AnvilRouter 按 chain_id 选择 Anvil 实例 (chainID=0 走默认链).
type AnvilRouter struct {
	clients   map[types.ChainID]*AnvilClient
	defaultID types.ChainID
}

// NewAnvilRouter 装配多链客户端; defaultID 为 chain_id 缺省回落链.
func NewAnvilRouter(clients map[types.ChainID]*AnvilClient, defaultID types.ChainID) *AnvilRouter {
	return &AnvilRouter{clients: clients, defaultID: defaultID}
}

// For 取链对应客户端 (nil chain -> 默认链).
func (r *AnvilRouter) For(chainID types.ChainID) (*AnvilClient, error) {
	if r == nil {
		return nil, fmt.Errorf("anvil router not configured")
	}
	if c, ok := r.clients[chainID]; ok {
		return c, nil
	}
	if c, ok := r.clients[r.defaultID]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("no anvil instance for chain %d", chainID)
}

// Invoke 实现 api.AnvilPassthrough:
//
//	snapshot          -> evm_snapshot
//	mine              -> anvil_mine (params.blocks 可选)
//	revert            -> evm_revert (params.snapshot_id 必填)
//	drop-transaction  -> anvil_dropTransaction (params.tx_hash 必填)
//
// 返回值为原样 JSON-RPC result (已解码为 Go 值).
func (r *AnvilRouter) Invoke(ctx context.Context, chainID uint64, action string, params map[string]any) (any, error) {
	cli, err := r.For(types.ChainID(chainID))
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = map[string]any{}
	}
	switch action {
	case "snapshot":
		return cli.Snapshot(ctx)
	case "mine":
		var blocks uint64 = 1
		if raw, ok := params["blocks"]; ok {
			n, err := paramUint(raw)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("params.blocks must be a positive integer")
			}
			blocks = n
		}
		if err := cli.Mine(ctx, blocks); err != nil {
			return nil, err
		}
		return "0x0", nil // anvil_mine result 恒为 "0x0"
	case "revert":
		raw, ok := params["snapshot_id"]
		if !ok {
			return nil, fmt.Errorf("params.snapshot_id is required")
		}
		sid, ok := raw.(string)
		if !ok || sid == "" {
			return nil, fmt.Errorf("params.snapshot_id must be a hex string")
		}
		return cli.Revert(ctx, sid)
	case "drop-transaction":
		raw, ok := params["tx_hash"]
		if !ok {
			return nil, fmt.Errorf("params.tx_hash is required")
		}
		h, ok := raw.(string)
		if !ok || !strings.HasPrefix(h, "0x") {
			return nil, fmt.Errorf("params.tx_hash must be a 0x-prefixed hash")
		}
		return cli.DropTransaction(ctx, types.HexToHash(h))
	default:
		return nil, fmt.Errorf("unknown anvil action %q", action)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hexQuantity(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

func parseHexQuantity(res json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(res, &s); err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}

func mustHexU64(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	return v
}

func paramUint(v any) (uint64, error) {
	switch t := v.(type) {
	case float64:
		if t < 0 || t != float64(uint64(t)) {
			return 0, fmt.Errorf("not a uint")
		}
		return uint64(t), nil
	case string:
		if strings.HasPrefix(t, "0x") {
			return strconv.ParseUint(t[2:], 16, 64)
		}
		return strconv.ParseUint(t, 10, 64)
	case json.Number:
		return strconv.ParseUint(t.String(), 10, 64)
	default:
		return 0, fmt.Errorf("not a uint")
	}
}

func lowerAddr(a types.Address) string {
	return "0x" + strings.ToLower(strings.TrimPrefix(a.Hex(), "0x"))
}

func lowerHash(h types.Hash) string {
	return "0x" + strings.ToLower(strings.TrimPrefix(h.Hex(), "0x"))
}

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 响应写出
// ---------------------------------------------------------------------------

// writeJSON 统一 JSON 响应 (金额字段经 types.U256.MarshalText 序列化为十进制字符串).
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// 序列化自身失败属于服务端 bug; 退化裸写.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL","message":"marshal failure"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// errorBody API Error 信封 {"error":{"code","message",...}}.
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
}

// writeError 输出统一 Error 信封.
func writeError(w http.ResponseWriter, r *http.Request, ae *APIError) {
	if ae == nil {
		ae = internalErr("internal error")
	}
	writeJSON(w, ae.Status, errorBody{Error: errorPayload{
		Code:      ae.Code,
		Message:   ae.Message,
		Details:   ae.Details,
		RequestID: RequestIDFrom(r.Context()),
	}})
}

// ---------------------------------------------------------------------------
// 请求解码与参数解析
// ---------------------------------------------------------------------------

// decodeJSONBody 读取并解码 JSON 请求体 (大小受 maxBytes 限制,
// 空体/尾缀/语法错误一律 400 INVALID_INPUT).
func decodeJSONBody(r *http.Request, dst any, maxBytes int64) *APIError {
	body, err := readBody(r, maxBytes)
	if err != nil {
		return badRequest(err.Error())
	}
	if len(body) == 0 {
		return badRequest("request body required")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(dst); err != nil {
		return badRequest(fmt.Sprintf("malformed JSON body: %v", err))
	}
	if dec.More() {
		return badRequest("unexpected trailing JSON content")
	}
	return nil
}

// readBody 完整读取请求体 (中间件已缓存则直接复用).
func readBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if cached, ok := r.Context().Value(ctxKeyBody).([]byte); ok {
		return cached, nil
	}
	limit := maxBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read request body")
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body exceeds %d bytes", limit)
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// 标量解析
// ---------------------------------------------------------------------------

// parseUUIDParam 解析 uuid 字符串 (path/query 共用).
func parseUUIDParam(s, name string) (uuid.UUID, *APIError) {
	id, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return uuid.Nil, badRequest(fmt.Sprintf("invalid %s: not a UUID", name))
	}
	return id, nil
}

// parsePathUUID 解析 path 参数 uuid.
func parsePathUUID(r *http.Request, name string) (uuid.UUID, *APIError) {
	return parseUUIDParam(r.PathValue(name), name)
}

// parseChainIDQuery 可选 chain_id 查询参数; 空 = nil (不过滤).
func parseChainIDQuery(r *http.Request) (*types.ChainID, *APIError) {
	raw := strings.TrimSpace(r.URL.Query().Get("chain_id"))
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		return nil, badRequest("invalid chain_id")
	}
	cid := types.ChainID(n)
	return &cid, nil
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// isBlank 空串判断.
func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

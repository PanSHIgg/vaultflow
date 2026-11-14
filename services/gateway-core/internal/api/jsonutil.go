package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// errorBody openapi Error 信封.
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
}

// writeError 输出 openapi Error 信封.
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
// 请求解码与参数解析 (全部宽松进 400 VALIDATION_FAILED)
// ---------------------------------------------------------------------------

// decodeJSONBody 读取并严格解码 JSON 请求体 (大小受 MaxBodyBytes 限制,
// 多余尾缀/未知内容一律 400).
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

// readBody 完整读取请求体并复位 (幂等中间件已读过则直接复用缓存).
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

// bodySHA256Hex 请求体 SHA-256 小写 hex (Idempotency-Key 绑定指纹).
func bodySHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
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

// parseAmount 解析 openapi Amount 十进制字符串 (禁止 JSON number — 解码期已由
// types.U256.UnmarshalText 拒绝; 此校验拦截空串/负号/前导格式异常).
func parseAmount(u types.U256, name string) (types.U256, *APIError) {
	if u.Sign() <= 0 {
		return types.U256{}, badRequest(fmt.Sprintf("%s must be a positive decimal string", name))
	}
	return u, nil
}

// parseChainIDQuery 可选 int64 链 id.
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

// parseEnumCSV 解析可重复 + 逗号分隔的枚举数组查询参数.
func parseEnumCSV[T ~string](r *http.Request, name string, valid func(T) bool) ([]T, *APIError) {
	var out []T
	for _, raw := range r.URL.Query()[name] {
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			v := T(tok)
			if !valid(v) {
				return nil, badRequest(fmt.Sprintf("invalid %s value %q", name, tok))
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// orderStatusValid OrderStatus 合法性 (11 态显式枚举).
func orderStatusValid(s types.OrderStatus) bool {
	switch s {
	case types.OrderAwaitingPayment, types.OrderPartiallyPaid, types.OrderPaid,
		types.OrderOverpaid, types.OrderExpired, types.OrderUnderpaidExpiredReview,
		types.OrderLatePaidReview, types.OrderRefundInProgress, types.OrderRefunded,
		types.OrderSettledReversed, types.OrderReversalResolved:
		return true
	}
	return false
}

func payoutStatusValid(s types.PayoutStatus) bool {
	switch s {
	case types.PayoutLockedQueued, types.PayoutInFlight, types.PayoutCompleted,
		types.PayoutFailedRetryable, types.PayoutCancelledUnlocked:
		return true
	}
	return false
}

func payoutKindValid(k types.PayoutKind) bool {
	return k == types.PayoutKindWithdrawal || k == types.PayoutKindCustomerRefund
}

func withdrawalStatusValid(s types.WithdrawalAddressStatus) bool {
	switch s {
	case types.WithdrawalPendingCooldown, types.WithdrawalActive,
		types.WithdrawalExpired, types.WithdrawalRevoked:
		return true
	}
	return false
}

func intentStatusValid(s types.TxIntentStatus) bool {
	switch s {
	case types.IntentQueued, types.IntentInFlight, types.IntentIncludedPendingSafe,
		types.IntentConfirmed, types.IntentFailedReverted,
		types.IntentCancelledByFiller, types.IntentNonceStolenHalt,
		types.IntentRejectedBySigner:
		return true
	}
	return false
}

func intentTypeValid(t types.IntentType) bool {
	switch t {
	case types.IntentTypeSweepBatch, types.IntentTypeMerchantPayout,
		types.IntentTypeEIP3009Relay, types.IntentTypeNonceGapFiller:
		return true
	}
	return false
}

func directionValid(d types.Direction) bool {
	return d == types.DirectionDebit || d == types.DirectionCredit
}

// ---------------------------------------------------------------------------
// 分页 (opaque cursor = base64url({"t":unixnano,"i":uuid}))
// ---------------------------------------------------------------------------

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// parsePageParams 解析 cursor + limit 公共参数.
func parsePageParams(r *http.Request) (limit int, after *Keyset, apiErr *APIError) {
	limit = defaultPageLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageLimit {
			return 0, nil, badRequest(fmt.Sprintf("limit must be an integer in [1,%d]", maxPageLimit))
		}
		limit = n
	}
	rawCur := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if rawCur == "" {
		return limit, nil, nil
	}
	blob, err := base64.RawURLEncoding.DecodeString(rawCur)
	if err != nil {
		return 0, nil, badRequest("invalid cursor")
	}
	var cur struct {
		T int64  `json:"t"`
		I string `json:"i"`
	}
	if err := json.Unmarshal(blob, &cur); err != nil {
		return 0, nil, badRequest("invalid cursor")
	}
	id, err := uuid.Parse(cur.I)
	if err != nil {
		return 0, nil, badRequest("invalid cursor")
	}
	ks := &Keyset{Sort: time.Unix(0, cur.T).UTC(), ID: id}
	return limit, ks, nil
}

// encodeCursor 由末行生成下一页游标 (hasMore=false 时调用方应传 null).
func encodeCursor(sort time.Time, id uuid.UUID) string {
	blob, _ := json.Marshal(struct {
		T int64  `json:"t"`
		I string `json:"i"`
	}{T: sort.UnixNano(), I: id.String()})
	return base64.RawURLEncoding.EncodeToString(blob)
}

// encodeIDCursor 仅含 id 的游标 (无 created_at 列的表).
func encodeIDCursor(id uuid.UUID) string {
	blob, _ := json.Marshal(struct {
		I string `json:"i"`
	}{I: id.String()})
	return base64.RawURLEncoding.EncodeToString(blob)
}

// parseIDCursor 解析仅含 id 的游标.
func parseIDCursor(raw string) (*uuid.UUID, *APIError) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, badRequest("invalid cursor")
	}
	var cur struct {
		I string `json:"i"`
	}
	if err := json.Unmarshal(blob, &cur); err != nil {
		return nil, badRequest("invalid cursor")
	}
	id, err := uuid.Parse(cur.I)
	if err != nil {
		return nil, badRequest("invalid cursor")
	}
	return &id, nil
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

var errNoRows = errors.New("api: no rows")

// isBlank 空串判断.
func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

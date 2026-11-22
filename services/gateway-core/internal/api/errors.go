package api

import (
	"errors"
	"net/http"

	"vaultflow/gateway-core/internal/exchange"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/store"
)

// ErrorCode 交易所托管 API 的机器可读错误码 (web/src/lib/api.ts 的 ApiError
// 直接消费 code 字段 — 契约冻结, 前端按 INVALID_INPUT/EMAIL_TAKEN/
// BAD_CREDENTIALS/UNAUTHORIZED/INVALID_CURSOR 分支渲染).
type ErrorCode string

const (
	CodeInvalidInput      ErrorCode = "INVALID_INPUT"      // 400 请求参数/语义校验失败
	CodeInvalidCursor     ErrorCode = "INVALID_CURSOR"     // 400 分页游标无法解码
	CodeUnauthorized      ErrorCode = "UNAUTHORIZED"       // 401 缺失/非法 Bearer
	CodeBadCredentials    ErrorCode = "BAD_CREDENTIALS"    // 401 登录邮箱或口令错误 (不区分)
	CodeNotFound          ErrorCode = "NOT_FOUND"          // 404 资源不存在
	CodeEmailTaken        ErrorCode = "EMAIL_TAKEN"        // 409 注册邮箱已占用
	CodeConflict          ErrorCode = "CONFLICT"           // 409 通用唯一性/状态冲突
	CodeRateLimited       ErrorCode = "RATE_LIMITED"       // 429 频控
	CodeSignerUnavailable ErrorCode = "SIGNER_UNAVAILABLE" // 502 vault-signer 不可达
	CodeInternal          ErrorCode = "INTERNAL"           // 500 服务端内部错误
)

// APIError 是 handler 层返回的结构化业务错误, 统一翻译为
// {"error":{"code","message","details?","request_id?"}} 信封.
type APIError struct {
	Status  int
	Code    ErrorCode
	Message string
	Details map[string]any
}

func (e *APIError) Error() string { return string(e.Code) + ": " + e.Message }

func apiErr(status int, code ErrorCode, msg string) *APIError {
	return &APIError{Status: status, Code: code, Message: msg}
}

func badRequest(msg string) *APIError {
	return apiErr(http.StatusBadRequest, CodeInvalidInput, msg)
}
func invalidCursor(msg string) *APIError {
	return apiErr(http.StatusBadRequest, CodeInvalidCursor, msg)
}
func unauthorized(msg string) *APIError {
	return apiErr(http.StatusUnauthorized, CodeUnauthorized, msg)
}
func notFound(msg string) *APIError { return apiErr(http.StatusNotFound, CodeNotFound, msg) }
func conflict(code ErrorCode, msg string) *APIError {
	return apiErr(http.StatusConflict, code, msg)
}
func signerUnavailable(msg string) *APIError {
	return apiErr(http.StatusBadGateway, CodeSignerUnavailable, msg)
}
func internalErr(msg string) *APIError {
	return apiErr(http.StatusInternalServerError, CodeInternal, msg)
}

// mapError 将领域/store/ports 哨兵错误统一翻译为 Error 信封语义;
// 已是 *APIError 的原样透传.
func mapError(err error) *APIError {
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, exchange.ErrInvalidInput):
		return badRequest("invalid email or password format")
	case errors.Is(err, exchange.ErrEmailTaken):
		return conflict(CodeEmailTaken, "email is already registered")
	case errors.Is(err, exchange.ErrBadCredentials):
		return apiErr(http.StatusUnauthorized, CodeBadCredentials, "invalid email or password")
	case errors.Is(err, store.ErrInvalidCursor):
		return invalidCursor("invalid pagination cursor")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, ports.ErrNotFound):
		return notFound("resource not found")
	case errors.Is(err, ports.ErrNegativeBalance):
		return badRequest("insufficient balance")
	case errors.Is(err, ports.ErrSignerUnavailable), errors.Is(err, ports.ErrUnavailable):
		return signerUnavailable("vault-signer unreachable")
	default:
		return internalErr("internal error")
	}
}

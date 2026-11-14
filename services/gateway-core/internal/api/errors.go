package api

import (
	"errors"
	"net/http"

	"vaultflow/gateway-core/internal/order"
	"vaultflow/gateway-core/internal/ports"
)

// ErrorCode 与 docs/openapi.yaml components/schemas/ErrorCode 枚举逐一对齐 (冻结).
type ErrorCode string

const (
	CodeValidationFailed           ErrorCode = "VALIDATION_FAILED"
	CodeIdempotencyKeyRequired     ErrorCode = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyConflict        ErrorCode = "IDEMPOTENCY_CONFLICT"
	CodeUnauthorized               ErrorCode = "UNAUTHORIZED"
	CodeForbidden                  ErrorCode = "FORBIDDEN"
	CodeNotFound                   ErrorCode = "NOT_FOUND"
	CodeRateLimited                ErrorCode = "RATE_LIMITED"
	CodeOrderStateInvalid          ErrorCode = "ORDER_STATE_INVALID"
	CodePayoutStateInvalid         ErrorCode = "PAYOUT_STATE_INVALID"
	CodeInsufficientBalance        ErrorCode = "INSUFFICIENT_BALANCE"
	CodePayoutFrozen               ErrorCode = "PAYOUT_FROZEN"
	CodeReorgReceivableOutstanding ErrorCode = "REORG_RECEIVABLE_OUTSTANDING"
	CodeAddressNotWhitelisted      ErrorCode = "ADDRESS_NOT_WHITELISTED"
	CodeSignerRejected             ErrorCode = "SIGNER_REJECTED"
	CodeRefundTicketInvalid        ErrorCode = "REFUND_TICKET_INVALID"
	CodeAlreadyTerminal            ErrorCode = "ALREADY_TERMINAL"
	CodeReconciliationRunning      ErrorCode = "RECONCILIATION_RUNNING"
	CodeFeatureDisabled            ErrorCode = "FEATURE_DISABLED"
	CodeUpstreamUnavailable        ErrorCode = "UPSTREAM_UNAVAILABLE"
	CodeInternal                   ErrorCode = "INTERNAL"
)

// APIError 是 handler 层返回的结构化业务错误, errorHandler 映射为
// openapi Error 信封 {"error":{code,message,details?,request_id}}.
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

func apiErrDetails(status int, code ErrorCode, msg string, details map[string]any) *APIError {
	return &APIError{Status: status, Code: code, Message: msg, Details: details}
}

func badRequest(msg string) *APIError {
	return apiErr(http.StatusBadRequest, CodeValidationFailed, msg)
}
func unauthorized(msg string) *APIError {
	return apiErr(http.StatusUnauthorized, CodeUnauthorized, msg)
}
func forbidden(msg string) *APIError { return apiErr(http.StatusForbidden, CodeForbidden, msg) }
func notFound(msg string) *APIError  { return apiErr(http.StatusNotFound, CodeNotFound, msg) }
func conflict(code ErrorCode, msg string) *APIError {
	return apiErr(http.StatusConflict, code, msg)
}
func unprocessable(code ErrorCode, msg string) *APIError {
	return apiErr(http.StatusUnprocessableEntity, code, msg)
}
func upstream(msg string) *APIError {
	return apiErr(http.StatusServiceUnavailable, CodeUpstreamUnavailable, msg)
}
func internalErr(msg string) *APIError {
	return apiErr(http.StatusInternalServerError, CodeInternal, msg)
}

// mapError 将 ports 哨兵错误与 APIError 统一翻译为 openapi Error 信封语义.
// stateCode 为 handler 提供的状态机冲突上下文码 (ORDER_STATE_INVALID /
// PAYOUT_STATE_INVALID); 传空则按默认 ORDER_STATE_INVALID 处理.
func mapError(err error, stateCode ErrorCode) *APIError {
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	if stateCode == "" {
		stateCode = CodeOrderStateInvalid
	}
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return notFound("resource not found")
	case errors.Is(err, ports.ErrIdempotencyConflict):
		return conflict(CodeIdempotencyConflict, "Idempotency-Key was replayed with a different request body")
	case errors.Is(err, ports.ErrDuplicateReference):
		return conflict(CodeIdempotencyConflict, "duplicate ledger reference with mismatched payload")
	case errors.Is(err, ports.ErrInvalidTransition):
		return conflict(stateCode, "state transition not allowed from the entity's current state")
	case errors.Is(err, ports.ErrTerminalState):
		return conflict(CodeAlreadyTerminal, "entity already in terminal state")
	case errors.Is(err, ports.ErrInvalidTemplateLeg),
		errors.Is(err, ports.ErrUnbalancedEntry),
		errors.Is(err, ports.ErrNonPositiveAmount),
		errors.Is(err, ports.ErrInvalidAccountCode):
		return unprocessable(CodeValidationFailed, "ledger entry rejected by posting rules")
	case errors.Is(err, ports.ErrNegativeBalance):
		return unprocessable(CodeInsufficientBalance, "insufficient balance in the source account")
	case errors.Is(err, ports.ErrMerchantFrozen):
		return conflict(CodePayoutFrozen, "merchant payouts are frozen pending reorg receivable resolution")
	case errors.Is(err, order.ErrAddressNotWhitelisted):
		return unprocessable(CodeAddressNotWhitelisted,
			"to_address is not an ACTIVE, unexpired withdrawal whitelist entry for this chain")
	case errors.Is(err, order.ErrInvalidRefundTicket):
		return unprocessable(CodeRefundTicketInvalid,
			"refund ticket failed validation (payload/embedding mismatch or nonzero retry_seq)")
	case errors.Is(err, ports.ErrSignerRejected):
		return apiErr(http.StatusBadRequest, CodeSignerRejected, "vault-signer rejected the request")
	case errors.Is(err, ports.ErrSignerUnavailable):
		return upstream("vault-signer unreachable")
	case errors.Is(err, ports.ErrNonceHalted):
		return conflict(stateCode, "wallet outbound queue halted by NONCE_STOLEN_HALT")
	case errors.Is(err, ports.ErrUnavailable):
		return upstream("upstream dependency unavailable")
	default:
		return internalErr("internal error")
	}
}

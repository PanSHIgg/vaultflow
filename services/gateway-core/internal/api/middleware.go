package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/types"
)

// ctxKey 请求上下文私有键.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyMerchant
	ctxKeyAdminSub
	ctxKeyBody
)

// RequestIDFrom 取请求关联 ID (X-Request-Id 或生成的 uuid).
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// MerchantFromCtx 取已鉴权商户 (merchantAuth 之后可用).
func MerchantFromCtx(r *http.Request) (types.Merchant, bool) {
	m, ok := r.Context().Value(ctxKeyMerchant).(types.Merchant)
	return m, ok
}

// AdminSubFromCtx 取已鉴权管理端 JWT sub.
func AdminSubFromCtx(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyAdminSub).(string); ok {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// 基础中间件
// ---------------------------------------------------------------------------

// middleware http.Handler 装饰器签名.
type middleware func(http.Handler) http.Handler

// chainMW 按声明顺序包裹 (mw[0] 最外层).
func chainMW(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// requestID 注入/透传 X-Request-Id.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// recoverer panic 兜底 -> 500 INTERNAL 信封.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log().Error("panic in http handler",
					zap.Any("panic", rec), zap.String("path", r.URL.Path),
					zap.String("request_id", RequestIDFrom(r.Context())))
				writeError(w, r, internalErr("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

// merchantAuth X-Api-Key -> SHA-256 hex -> Store.ResolveAPIKey -> ctx merchant.
// 密钥本体不落库不落日志, 仅哈希匹配 (merchants.api_key_hash).
func (s *Server) merchantAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("X-Api-Key"))
		if key == "" {
			writeError(w, r, unauthorized("missing X-Api-Key header"))
			return
		}
		if len(key) > 256 {
			writeError(w, r, unauthorized("invalid API key"))
			return
		}
		sum := sha256.Sum256([]byte(key))
		if s.deps.Store == nil {
			writeError(w, r, upstream("merchant directory unavailable"))
			return
		}
		m, err := s.deps.Store.ResolveAPIKey(r.Context(), hex.EncodeToString(sum[:]))
		if err != nil {
			writeError(w, r, unauthorized("invalid API key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyMerchant, m)))
	})
}

// adminJWT 签发面契约: 固定 iss/aud + 强制 exp — 阻断跨系统令牌串用
// 与无期限令牌; minter 侧常量须与本文件一致 (e2e-lib.mjs / run-all.sh).
const (
	adminJWTIssuer   = "vaultflow-gateway"
	adminJWTAudience = "vaultflow-admin"
)

// adminAuth Bearer JWT (HS256, sub+exp+iss+aud 必填) -> ctx admin subject.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(hdr, "Bearer ") {
			writeError(w, r, unauthorized("missing bearer token"))
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
		if len(s.deps.AdminJWTKey) == 0 {
			writeError(w, r, upstream("admin auth not configured"))
			return
		}
		tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
			return s.deps.AdminJWTKey, nil
		},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(),
			jwt.WithIssuer(adminJWTIssuer),
			jwt.WithAudience(adminJWTAudience))
		if err != nil || !tok.Valid {
			writeError(w, r, unauthorized("invalid bearer token"))
			return
		}
		claims, ok := tok.Claims.(jwt.MapClaims)
		if !ok {
			writeError(w, r, unauthorized("invalid bearer token claims"))
			return
		}
		sub, err := claims.GetSubject()
		if err != nil || sub == "" {
			writeError(w, r, unauthorized("bearer token missing sub"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyAdminSub, sub)))
	})
}

// ---------------------------------------------------------------------------
// 幂等中间件 (openapi: 每个非 GET 端点强制 Idempotency-Key)
// ---------------------------------------------------------------------------

// idempotency 写接口幂等: 同 (scope,key) 同 body SHA-256 回放首响应,
// 异 body -> 409 IDEMPOTENCY_CONFLICT. scope 取已鉴权主体
// (merchant:<id> / admin:<sub> / public:<orderId>).
func (s *Server) idempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeError(w, r, apiErr(http.StatusBadRequest, CodeIdempotencyKeyRequired,
				"Idempotency-Key header is required on non-GET endpoints"))
			return
		}
		if !idemKeyPattern.MatchString(key) {
			writeError(w, r, badRequest("invalid Idempotency-Key format"))
			return
		}
		body, err := readBody(r, s.deps.Config.MaxBodyBytes)
		if err != nil {
			writeError(w, r, badRequest(err.Error()))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyBody, body))
		hash := bodySHA256Hex(body)
		scope := s.idemScope(r)
		store := s.deps.IdemStore
		if store == nil {
			// 防御: NewServer 已保证非 nil; 直接构造 Server{} 的调用兜底.
			writeError(w, r, upstream("idempotency store unavailable"))
			return
		}

		replay := func(rec IdempotencyRecord) {
			w.Header().Set("X-Idempotent-Replay", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.StatusCode)
			_, _ = w.Write(rec.Body)
		}

		if rec, found, lerr := store.Load(r.Context(), scope, key); lerr == nil && found {
			if rec.RequestHash != hash {
				writeError(w, r, conflict(CodeIdempotencyConflict,
					"Idempotency-Key was already used with a different request body"))
				return
			}
			replay(rec)
			return
		}

		// 同实例同键串行化: 并发重复请求等待首个完成后回放.
		lk := s.idemLocks.lockFor(scope, key)
		lk.Lock()
		defer lk.Unlock()
		if rec, found, lerr := store.Load(r.Context(), scope, key); lerr == nil && found {
			if rec.RequestHash != hash {
				writeError(w, r, conflict(CodeIdempotencyConflict,
					"Idempotency-Key was already used with a different request body"))
				return
			}
			replay(rec)
			return
		}

		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)

		// 仅缓存确定性响应: 2xx/4xx (5xx 与 429 不缓存, 便于重试恢复).
		if cw.status < 500 && cw.status != http.StatusTooManyRequests {
			rec := IdempotencyRecord{
				RequestHash: hash, StatusCode: cw.status,
				Body: cw.buf.Bytes(), StoredAt: s.deps.now().Unix(),
			}
			stored, serr := store.SaveNX(r.Context(), scope, key, rec, s.deps.Config.IdempotencyTTL)
			if serr == nil && !stored {
				// 跨实例并发: 已有首响应存档 — 以存档为准回放.
				if rec2, found2, lerr2 := store.Load(r.Context(), scope, key); lerr2 == nil && found2 && rec2.RequestHash == hash {
					replay(rec2)
					return
				}
			}
		}
		cw.flushTo(w)
	})
}

// idemScope 幂等作用域 = 已鉴权主体 (openapi IdempotencyKey 定义).
func (s *Server) idemScope(r *http.Request) string {
	if m, ok := MerchantFromCtx(r); ok {
		return "merchant:" + m.ID.String()
	}
	if sub := AdminSubFromCtx(r); sub != "" {
		return "admin:" + sub
	}
	if oid := r.PathValue("orderId"); oid != "" {
		return "public:" + oid
	}
	return "anonymous"
}

// captureWriter 捕获状态码与响应体 (幂等存档后延迟写出).
type captureWriter struct {
	http.ResponseWriter
	status     int
	buf        bytes.Buffer
	headerSunk bool
}

func (c *captureWriter) WriteHeader(code int) {
	if c.headerSunk {
		return
	}
	c.headerSunk = true
	c.status = code
}

func (c *captureWriter) Write(b []byte) (int, error) {
	return c.buf.Write(b)
}

// Flush implements http.Flusher — SSE handler 等流式响应即时透传且不参与存档.
func (c *captureWriter) Flush() {}

func (c *captureWriter) flushTo(w http.ResponseWriter) {
	w.WriteHeader(c.status)
	_, _ = w.Write(c.buf.Bytes())
}

// ---------------------------------------------------------------------------
// 频控中间件 (三种作用域桶)
// ---------------------------------------------------------------------------

// rateLimit 以 keyFn 派生桶键执行固定窗口限流; 超限 -> 429 + Retry-After.
func (s *Server) rateLimit(rule RateLimitRule, keyFn func(*http.Request) string) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.deps.Limiter == nil {
				next.ServeHTTP(w, r)
				return
			}
			key := keyFn(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			allowed, retryAfter := s.deps.Limiter.Allow(r.Context(), key, rule)
			if !allowed {
				w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
				writeError(w, r, apiErr(http.StatusTooManyRequests, CodeRateLimited,
					"rate limit exceeded"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func retryAfterSeconds(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	return strconv.Itoa(sec)
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.Index(xf, ","); i > 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

package api

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/auth"
)

// ctxKey 请求上下文私有键.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyUser
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

// UserIDFromCtx 取已鉴权终端用户 id (userAuth 之后可用).
func UserIDFromCtx(r *http.Request) (uuid.UUID, bool) {
	id, ok := r.Context().Value(ctxKeyUser).(uuid.UUID)
	return id, ok
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

// bearerToken 提取 Authorization: Bearer <token> (首尾空白容错).
func bearerToken(r *http.Request) (string, bool) {
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(hdr, "Bearer ") {
		return "", false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
	return raw, raw != ""
}

// userAuth Bearer JWT (HS256, role="user", sub=user_uuid) -> ctx user_id.
// 校验逻辑收敛在 internal/auth.ParseUserToken (与 IssueUserToken 成对).
func (s *Server) userAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			writeError(w, r, unauthorized("missing bearer token"))
			return
		}
		if len(s.deps.UserJWTKey) == 0 {
			writeError(w, r, internalErr("user auth not configured"))
			return
		}
		uid, err := auth.ParseUserToken(s.deps.UserJWTKey, raw)
		if err != nil {
			writeError(w, r, unauthorized("invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser, uid)))
	})
}

// adminAuth Bearer JWT (HS256, sub 必填, ADMIN_JWT_SECRET 独立密钥) -> ctx admin subject.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			writeError(w, r, unauthorized("missing bearer token"))
			return
		}
		if len(s.deps.AdminJWTKey) == 0 {
			writeError(w, r, internalErr("admin auth not configured"))
			return
		}
		tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
			return s.deps.AdminJWTKey, nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
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
// 频控中间件 (per-IP / per-subject 固定窗口桶)
// ---------------------------------------------------------------------------

// rateLimit 以 keyFn 派生桶键执行固定窗口限流; 超限 -> 429 RATE_LIMITED + Retry-After.
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

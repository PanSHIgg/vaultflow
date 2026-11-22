package api

import (
	"net/http"

	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/types"
)

// Server 交易所托管 REST 网关装配: Go 1.22+ net/http ServeMux 方法+路径参数路由
// (无第三方 HTTP 框架 — go.mod 铁律). 面划分:
//   - public:  /v1/auth/register, /v1/auth/login, /v1/assets
//   - user:    /v1/me/**          (USER JWT Bearer)
//   - admin:   /v1/admin/**       (ADMIN JWT Bearer, 独立密钥)
type Server struct {
	deps    Deps
	mux     *http.ServeMux
	handler http.Handler
}

// NewServer 组装路由与中间件链.
func NewServer(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = zap.NewNop()
	}
	if deps.Config.MaxBodyBytes <= 0 {
		deps.Config.MaxBodyBytes = 1 << 20
	}
	if deps.Config.Chains == nil {
		deps.Config.Chains = map[types.ChainID]ChainConfig{}
	}
	s := &Server{deps: deps}
	s.routes()
	return s
}

func (s *Server) log() *zap.Logger { return s.deps.Logger }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Handler 返回根 handler (含全局 requestID + recoverer).
func (s *Server) Handler() http.Handler { return s.handler }

// routes 注册全部端点.
//
// 中间件链序 (自外向内):
//
//	requestID -> recoverer -> [rateLimit] -> [auth] -> handler
func (s *Server) routes() {
	mux := http.NewServeMux()
	cfg := s.deps.Config

	ipKey := func(r *http.Request) string { return "ip:" + clientIP(r) }
	uKey := func(r *http.Request) string {
		if uid, ok := UserIDFromCtx(r); ok {
			return "u:" + uid.String()
		}
		return "u:unknown"
	}
	aKey := func(r *http.Request) string {
		if sub := AdminSubFromCtx(r); sub != "" {
			return "a:" + sub
		}
		return "a:unknown"
	}

	ipRate := func(rule RateLimitRule) middleware { return s.rateLimit(rule, ipKey) }
	user := func(h http.HandlerFunc) http.Handler {
		return chainMW(h, s.userAuth, s.rateLimit(cfg.RateUser, uKey))
	}
	admin := func(h http.HandlerFunc) http.Handler {
		return chainMW(h, s.adminAuth, s.rateLimit(cfg.RateAdmin, aKey))
	}

	// ---- public ----
	// auth 面按 IP 限流 (防注册爆破/登录撞库).
	mux.Handle("POST /v1/auth/register", chainMW(http.HandlerFunc(s.register), ipRate(cfg.RateAuth)))
	mux.Handle("POST /v1/auth/login", chainMW(http.HandlerFunc(s.login), ipRate(cfg.RateAuth)))
	mux.Handle("GET /v1/assets", chainMW(http.HandlerFunc(s.listAssets), ipRate(cfg.RatePublic)))

	// ---- user (USER JWT) ----
	mux.Handle("GET /v1/me/deposit-address/{chain_id}", user(s.getDepositAddress))
	mux.Handle("GET /v1/me/deposits", user(s.listDeposits))
	mux.Handle("GET /v1/me/balances", user(s.listBalances))
	mux.Handle("POST /v1/me/withdrawals", user(s.createWithdrawal))
	mux.Handle("GET /v1/me/withdrawals", user(s.listWithdrawals))

	// ---- admin (ADMIN JWT) ----
	mux.Handle("GET /v1/admin/wallets", admin(s.listWallets))
	mux.Handle("POST /v1/admin/wallets", admin(s.createWallet))
	mux.Handle("PUT /v1/admin/wallets/{id}", admin(s.updateWallet))
	mux.Handle("DELETE /v1/admin/wallets/{id}", admin(s.deleteWallet))
	mux.Handle("GET /v1/admin/signer/health", admin(s.getSignerHealth))

	s.mux = mux
	s.handler = s.requestID(s.recoverer(mux))
}

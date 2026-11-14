package api

import (
	"net/http"

	"go.uber.org/zap"
)

// Server REST 网关装配: Go 1.22+ net/http ServeMux 方法+路径参数路由
// (无第三方 HTTP 框架 — go.mod 铁律), openapi.yaml 24 paths / 27 ops 全覆盖.
type Server struct {
	deps      Deps
	recon     *ReconManager
	idemLocks idemStripedLocks
	mux       *http.ServeMux
	handler   http.Handler
}

// NewServer 组装路由与中间件链.
func NewServer(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = zap.NewNop()
	}
	if deps.Config.CashierURLFormat == "" {
		deps.Config = DefaultConfig()
	}
	if deps.IdemStore == nil {
		deps.IdemStore = NewMemoryIdempotencyStore()
	}
	s := &Server{deps: deps, recon: newReconManager(&deps)}
	s.routes()
	return s
}

func (s *Server) log() *zap.Logger { return s.deps.Logger }

// ServeHTTP implements http.Handler — Wave-2 I1 将其挂上 net/http.Server.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Handler 返回根 handler (含全局 requestID + recoverer).
func (s *Server) Handler() http.Handler { return s.handler }

// routes 按 openapi.yaml paths 注册全部端点.
//
// 中间件链序 (自外向内):
//
//	requestID -> recoverer -> [auth] -> [rateLimit] -> [idempotency] -> handler
func (s *Server) routes() {
	mux := http.NewServeMux()
	cfg := s.deps.Config

	mRate := func(key func(*http.Request) string) middleware {
		return s.rateLimit(cfg.RateMerchant, key)
	}
	pRate := func(key func(*http.Request) string) middleware {
		return s.rateLimit(cfg.RatePublic, key)
	}
	aRate := func(key func(*http.Request) string) middleware {
		return s.rateLimit(cfg.RateAdmin, key)
	}
	mKey := func(r *http.Request) string {
		if m, ok := MerchantFromCtx(r); ok {
			return "m:" + m.ID.String()
		}
		return "m:unknown"
	}
	aKey := func(r *http.Request) string {
		if sub := AdminSubFromCtx(r); sub != "" {
			return "a:" + sub
		}
		return "a:unknown"
	}
	ipKey := func(r *http.Request) string { return "p:" + clientIP(r) }

	merchant := func(h http.HandlerFunc, extra ...middleware) http.Handler {
		mw := append([]middleware{s.merchantAuth, mRate(mKey)}, extra...)
		return chainMW(h, mw...)
	}
	merchantWrite := func(h http.HandlerFunc) http.Handler {
		return merchant(h, s.idempotency)
	}
	public := func(h http.HandlerFunc, extra ...middleware) http.Handler {
		mw := append([]middleware{pRate(ipKey)}, extra...)
		return chainMW(h, mw...)
	}
	admin := func(h http.HandlerFunc, extra ...middleware) http.Handler {
		mw := append([]middleware{s.adminAuth, aRate(aKey)}, extra...)
		return chainMW(h, mw...)
	}
	adminWrite := func(h http.HandlerFunc) http.Handler {
		return admin(h, s.idempotency)
	}

	// ---- 1. merchant-orders ----
	mux.Handle("POST /v1/orders", merchantWrite(s.createOrder))
	mux.Handle("GET /v1/orders", merchant(s.listOrders))
	mux.Handle("GET /v1/orders/{orderId}", merchant(s.getOrder))
	mux.Handle("POST /v1/orders/{orderId}/review", merchantWrite(s.reviewOrder))

	// ---- 1b. merchant-payouts ----
	mux.Handle("POST /v1/payouts", merchantWrite(s.createPayout))
	mux.Handle("GET /v1/payouts", merchant(s.listPayouts))
	mux.Handle("GET /v1/payouts/{payoutId}", merchant(s.getPayout))
	mux.Handle("POST /v1/payouts/{payoutId}/cancel", merchantWrite(s.cancelPayout))
	mux.Handle("POST /v1/refunds", merchantWrite(s.createRefund))

	// ---- 1c. merchant-withdrawal-addresses ----
	mux.Handle("POST /v1/withdrawal-addresses", merchantWrite(s.registerWithdrawalAddress))
	mux.Handle("GET /v1/withdrawal-addresses", merchant(s.listWithdrawalAddresses))
	mux.Handle("DELETE /v1/withdrawal-addresses/{withdrawalAddressId}", merchantWrite(s.revokeWithdrawalAddress))

	// ---- 1d. merchant-ledger ----
	mux.Handle("GET /v1/ledger/accounts", merchant(s.listLedgerAccounts))
	mux.Handle("GET /v1/ledger/transactions", merchant(s.listLedgerTransactions))
	mux.Handle("GET /v1/ledger/entries", merchant(s.listLedgerEntries))

	// ---- 2. public-cashier (无鉴权) ----
	mux.Handle("GET /v1/public/payments/{orderId}", public(s.getPublicPayment))
	mux.Handle("GET /v1/public/payments/{orderId}/events", public(s.streamPaymentEvents))
	mux.Handle("POST /v1/public/payments/{orderId}/eip3009", public(s.submitEip3009, s.idempotency))

	// ---- 3. admin-ops (JWT) ----
	mux.Handle("GET /v1/admin/reconciliation/status", admin(s.getReconciliationStatus))
	mux.Handle("POST /v1/admin/reconciliation/run", adminWrite(s.runReconciliation))
	mux.Handle("GET /v1/admin/outbound-queue", admin(s.getOutboundQueue))
	mux.Handle("GET /v1/admin/signer/health", admin(s.getSignerHealth))
	mux.Handle("GET /v1/admin/quarantined-vaults", admin(s.listQuarantinedVaults))
	mux.Handle("POST /v1/admin/merchants/{merchantId}/payout-unfreeze", adminWrite(s.unfreezeMerchantPayouts))

	// ---- 4. dev-simulator (JWT + ENABLE_DEV_SIMULATOR gate; 路由常注册,
	// gate 返回 404 FEATURE_DISABLED, 与 openapi "404 FEATURE_DISABLED/403" 语义一致)
	mux.Handle("POST /v1/sim/anvil/{action}", adminWrite(s.simAnvil))
	mux.Handle("POST /v1/sim/scenarios/{scenario}", adminWrite(s.runSimScenario))
	mux.Handle("GET /v1/sim/runs/{runId}", admin(s.getSimRun))

	s.mux = mux
	s.handler = s.requestID(s.recoverer(mux))
}

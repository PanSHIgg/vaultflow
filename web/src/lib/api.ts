// W6-1 — typed REST/SSE client. Every function dispatches to the local mock
// engine when NEXT_PUBLIC_USE_MOCK=true (Wave-1 default) or to the real
// gateway-core endpoints from docs/openapi.yaml when false (Wave-2 I2).

import { adminToken, apiBaseUrl, demoApiKey } from "./env";
import { ApiError } from "./errors";
import { USE_MOCK } from "./mock";
import * as mock from "./mock-engine";
import type {
  CreateOrderRequest,
  CreatePayoutRequest,
  CreateRefundRequest,
  Eip3009Accepted,
  Eip3009Submission,
  LedgerAccountsResponse,
  LedgerEntryPage,
  LedgerTransactionPage,
  MerchantFreezeState,
  Order,
  OrderPage,
  OutboundQueueView,
  Payout,
  PayoutPage,
  PaymentOrderStatus,
  PublicPayment,
  QuarantinedVaultPage,
  ReconciliationReport,
  ReconciliationRunAccepted,
  RegisterWithdrawalAddressRequest,
  ReviewOrderRequest,
  ReviewOrderResponse,
  SignerHealth,
  SimAnvilAction,
  SimRun,
  SimScenario,
  WithdrawalAddress,
  WithdrawalAddressPage,
} from "./types";

const BASE = apiBaseUrl;
const API_KEY = demoApiKey;
const ADMIN_TOKEN = adminToken;

export function newIdempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `idem-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

interface ReqOpts {
  body?: unknown;
  idemKey?: string;
  apiKey?: string;
  token?: string;
}

async function request<T>(method: string, path: string, opts: ReqOpts = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (opts.body !== undefined) headers["content-type"] = "application/json";
  if (opts.idemKey) headers["Idempotency-Key"] = opts.idemKey;
  const key = opts.apiKey ?? API_KEY();
  if (key) headers["X-Api-Key"] = key;
  const token = opts.token ?? ADMIN_TOKEN();
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(`${BASE()}${path}`, {
    method,
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = undefined;
  }
  if (!res.ok) {
    const err = (data as { error?: { code?: string; message?: string; details?: Record<string, unknown> } } | undefined)
      ?.error;
    throw new ApiError(
      res.status,
      (err?.code as ApiError["code"]) ?? "INTERNAL",
      err?.message ?? `${res.status} ${res.statusText}`,
      err?.details,
    );
  }
  return data as T;
}

// ---------------------------------------------------------------------------
// Public cashier
// ---------------------------------------------------------------------------

export function getPublicPayment(orderId: string): Promise<PublicPayment> {
  return USE_MOCK
    ? mock.getPublicPayment(orderId)
    : request("GET", `/v1/public/payments/${orderId}`);
}

/** Minimal EventSource-shaped contract used by the cashier hook. */
export interface PaymentEventSource {
  addEventListener(type: string, handler: (ev: { data: string; lastEventId?: string }) => void): void;
  close(): void;
  setOnError(cb: (e?: unknown) => void): void;
}

/**
 * Open the order SSE stream. In real mode this is a native EventSource; in mock
 * mode a scripted MockPaymentStream emits the same event grammar.
 *
 * Reconnect semantics (openapi `lastEventId` contract): native EventSource
 * replays `Last-Event-ID` on its own auto-reconnects. When the caller wants an
 * explicit resume point (e.g. re-opening after a polling degrade), pass
 * `lastEventId` — it is sent as the `?lastEventId=` query fallback the server
 * accepts for browser EventSource (it cannot set the header itself; FIX-D4).
 */
export function openPaymentEvents(orderId: string, lastEventId?: string): PaymentEventSource {
  if (USE_MOCK) return mock.openPaymentStream(orderId);
  let url = `${BASE()}/v1/public/payments/${orderId}/events`;
  if (lastEventId) url += `?lastEventId=${encodeURIComponent(lastEventId)}`;
  const es = new EventSource(url);
  return {
    addEventListener: (t, h) =>
      es.addEventListener(t, (e) => {
        const m = e as MessageEvent;
        h({ data: m.data, lastEventId: m.lastEventId || undefined });
      }),
    close: () => es.close(),
    setOnError: (cb) => {
      es.onerror = cb;
    },
  };
}

export function submitEip3009(orderId: string, sub: Eip3009Submission): Promise<Eip3009Accepted> {
  return USE_MOCK
    ? mock.submitEip3009(orderId, sub)
    : request("POST", `/v1/public/payments/${orderId}/eip3009`, { body: sub, idemKey: newIdempotencyKey() });
}

// ---------------------------------------------------------------------------
// Merchant orders
// ---------------------------------------------------------------------------

export function createOrder(req: CreateOrderRequest): Promise<Order> {
  if (USE_MOCK) {
    // The mock engine has no CREATE2/order service — order creation is a
    // real-backend flow (Wave-2 I2). The seeded demo orders remain usable.
    return Promise.reject(
      new ApiError(400, "FEATURE_DISABLED", "mock engine does not implement POST /v1/orders"),
    );
  }
  return request("POST", "/v1/orders", { body: req, idemKey: newIdempotencyKey() });
}

export function listOrders(
  q: { status?: PaymentOrderStatus[]; chain_id?: number; limit?: number; cursor?: string } = {},
): Promise<OrderPage> {
  if (USE_MOCK) return mock.listOrders({ status: q.status });
  const params = new URLSearchParams();
  for (const s of q.status ?? []) params.append("status", s);
  if (q.chain_id) params.set("chain_id", String(q.chain_id));
  if (q.limit) params.set("limit", String(q.limit));
  if (q.cursor) params.set("cursor", q.cursor);
  const qs = params.toString();
  return request("GET", `/v1/orders${qs ? `?${qs}` : ""}`);
}

export function getOrder(orderId: string): Promise<Order> {
  return USE_MOCK ? mock.getOrder(orderId) : request("GET", `/v1/orders/${orderId}`);
}

export function reviewOrder(orderId: string, req: ReviewOrderRequest): Promise<ReviewOrderResponse> {
  return USE_MOCK
    ? mock.reviewOrder(orderId, req)
    : request("POST", `/v1/orders/${orderId}/review`, { body: req, idemKey: newIdempotencyKey() });
}

// ---------------------------------------------------------------------------
// Payouts / refunds
// ---------------------------------------------------------------------------

export function listPayouts(): Promise<PayoutPage> {
  return USE_MOCK ? mock.listPayouts() : request("GET", "/v1/payouts");
}

export function createPayout(req: CreatePayoutRequest): Promise<Payout> {
  return USE_MOCK
    ? mock.createPayout(req)
    : request("POST", "/v1/payouts", { body: req, idemKey: newIdempotencyKey() });
}

export function createRefund(req: CreateRefundRequest): Promise<Payout> {
  return USE_MOCK
    ? mock.createRefund(req)
    : request("POST", "/v1/refunds", { body: req, idemKey: newIdempotencyKey() });
}

export function cancelPayout(payoutId: string): Promise<Payout> {
  return USE_MOCK
    ? mock.cancelPayout(payoutId)
    : request("POST", `/v1/payouts/${payoutId}/cancel`, { idemKey: newIdempotencyKey() });
}

// ---------------------------------------------------------------------------
// Withdrawal whitelist
// ---------------------------------------------------------------------------

export function listWithdrawalAddresses(): Promise<WithdrawalAddressPage> {
  return USE_MOCK ? mock.listWithdrawalAddresses() : request("GET", "/v1/withdrawal-addresses");
}

export function registerWithdrawalAddress(req: RegisterWithdrawalAddressRequest): Promise<WithdrawalAddress> {
  return USE_MOCK
    ? mock.registerWithdrawalAddress(req)
    : request("POST", "/v1/withdrawal-addresses", { body: req, idemKey: newIdempotencyKey() });
}

export function revokeWithdrawalAddress(id: string): Promise<WithdrawalAddress> {
  return USE_MOCK
    ? mock.revokeWithdrawalAddress(id)
    : request("DELETE", `/v1/withdrawal-addresses/${id}`, { idemKey: newIdempotencyKey() });
}

// ---------------------------------------------------------------------------
// Ledger introspection
// ---------------------------------------------------------------------------

export function listLedgerAccounts(): Promise<LedgerAccountsResponse> {
  return USE_MOCK ? mock.listLedgerAccounts() : request("GET", "/v1/ledger/accounts");
}

export function listLedgerTransactions(): Promise<LedgerTransactionPage> {
  return USE_MOCK ? mock.listLedgerTransactions() : request("GET", "/v1/ledger/transactions");
}

export function listLedgerEntries(q: { transaction_id?: string } = {}): Promise<LedgerEntryPage> {
  if (USE_MOCK) return mock.listLedgerEntries(q);
  const params = new URLSearchParams();
  if (q.transaction_id) params.set("transaction_id", q.transaction_id);
  const qs = params.toString();
  return request("GET", `/v1/ledger/entries${qs ? `?${qs}` : ""}`);
}

// ---------------------------------------------------------------------------
// Admin / ops
// ---------------------------------------------------------------------------

export function getReconciliationStatus(): Promise<ReconciliationReport> {
  return USE_MOCK ? mock.getReconciliationStatus() : request("GET", "/v1/admin/reconciliation/status");
}

export function runReconciliation(chainId?: number): Promise<ReconciliationRunAccepted> {
  return USE_MOCK
    ? mock.runReconciliation()
    : request("POST", "/v1/admin/reconciliation/run", {
        body: chainId ? { chain_id: chainId } : {},
        idemKey: newIdempotencyKey(),
      });
}

export function getOutboundQueue(): Promise<OutboundQueueView> {
  return USE_MOCK ? mock.getOutboundQueue() : request("GET", "/v1/admin/outbound-queue");
}

export function getSignerHealth(): Promise<SignerHealth> {
  return USE_MOCK ? mock.getSignerHealth() : request("GET", "/v1/admin/signer/health");
}

export function listQuarantinedVaults(): Promise<QuarantinedVaultPage> {
  return USE_MOCK ? mock.listQuarantinedVaults() : request("GET", "/v1/admin/quarantined-vaults");
}

export function unfreezeMerchantPayouts(merchantId: string): Promise<MerchantFreezeState> {
  return USE_MOCK
    ? mock.unfreezeMerchantPayouts(merchantId)
    : request("POST", `/v1/admin/merchants/${merchantId}/payout-unfreeze`, { idemKey: newIdempotencyKey() });
}

// ---------------------------------------------------------------------------
// Dev simulator
// ---------------------------------------------------------------------------

export function runSimScenario(scenario: SimScenario): Promise<SimRun> {
  return USE_MOCK
    ? mock.runSimScenario(scenario)
    : request("POST", `/v1/sim/scenarios/${scenario}`, { body: {}, idemKey: newIdempotencyKey() });
}

export function getSimRun(runId: string): Promise<SimRun> {
  return USE_MOCK ? mock.getSimRun(runId) : request("GET", `/v1/sim/runs/${runId}`);
}

export function simAnvil(
  action: SimAnvilAction,
  params: Record<string, unknown> = {},
): Promise<{ action: SimAnvilAction; result: unknown }> {
  return USE_MOCK
    ? mock.simAnvil(action, params)
    : request("POST", `/v1/sim/anvil/${action}`, { body: { params }, idemKey: newIdempotencyKey() });
}

export { ApiError, USE_MOCK };

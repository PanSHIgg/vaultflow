// W6-1 — in-browser mock engine implementing the openapi.yaml surface used by
// the app, so Wave-1 runs fully detached from gateway-core. Live cashier orders
// progress on a deterministic wall-clock script: deposit DETECTED at t+2s,
// confirmations advance ~1.5s apart to the safe-head threshold, then settle.

import { isUuid } from "./bytes";
import { DEV_CONTRACTS } from "./chains";
import { apiError, ApiError } from "./errors";
import {
  ANVIL,
  DEMO_ORDER_ID,
  MOCK_INTENT_3009,
  MOCK_MERCHANT_ID,
  MOCK_MERCHANT_NAME,
  type MockOrder,
  mockCreate2Salt,
  USDC,
  USDT,
  seedLedgerAccounts,
  seedLedgerEntries,
  seedLedgerTransactions,
  seedNonceCursors,
  seedOrders,
  seedOutboundIntents,
  seedPayouts,
  seedQuarantinedVaults,
  seedSignerHealth,
  seedWhitelist,
} from "./mock-data";
import type {
  CreatePayoutRequest,
  CreateRefundRequest,
  CustodyCheck,
  Eip3009Accepted,
  Eip3009Submission,
  LedgerAccount,
  LedgerAccountsResponse,
  LedgerEntryPage,
  LedgerTransactionPage,
  MerchantFreezeState,
  Order,
  OrderPage,
  OutboundQueueView,
  OutboundTxIntentStatus,
  Payout,
  PayoutPage,
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
  SimStep,
  SseConfirmationData,
  SseDepositData,
  SseOrderStatusData,
  WithdrawalAddressPage,
} from "./types";

// ---------------------------------------------------------------------------
// Store (module singleton — client-side only)
// ---------------------------------------------------------------------------

const REQUIRED_CONFIRMATIONS = 6;
const DETECT_AT_MS = 2_000;
const CONF_INTERVAL_MS = 1_500;

const SIG64_RE = /^(0x)?[0-9a-fA-F]{128}$/;
const EVM_ADDR_RE = /^0x[0-9a-fA-F]{40}$/;

/**
 * Contract-parity guard for the client `payout_uuid` idempotency anchor
 * (openapi CreatePayoutRequest/CreateRefundRequest + RefundTicketFields):
 * the uuid becomes `merchant_payouts.id`. Replaying the same uuid with an
 * identical materialized body returns the existing row; a different body is
 * `409 IDEMPOTENCY_CONFLICT`. Returns null when the uuid is unused.
 */
function idempotentPayoutReplay(payoutUuid: string, fingerprint: Record<string, unknown>): Payout | null {
  const existing = store.payouts.get(payoutUuid.toLowerCase());
  if (!existing) return null;
  const same = Object.entries(fingerprint).every(([k, v]) => {
    const got = (existing as unknown as Record<string, unknown>)[k];
    if (typeof got === "string" && typeof v === "string") return got.toLowerCase() === v.toLowerCase();
    return (got ?? null) === (v ?? null);
  });
  if (!same) {
    throw apiError(409, "IDEMPOTENCY_CONFLICT", `payout_uuid ${payoutUuid} already exists with a different request body`);
  }
  return existing;
}

interface MockSimRunInternal extends SimRun {
  __startedMs: number;
}

interface Store {
  seeded: boolean;
  orders: Map<string, MockOrder>;
  payouts: Map<string, Payout>;
  whitelist: Map<string, import("./types").WithdrawalAddress>;
  ledgerAccounts: LedgerAccount[];
  ledgerTxs: import("./types").LedgerTransaction[];
  ledgerEntries: import("./types").LedgerEntry[];
  simRuns: Map<string, MockSimRunInternal>;
  seq: number;
}

const store: Store = {
  seeded: false,
  orders: new Map(),
  payouts: new Map(),
  whitelist: new Map(),
  ledgerAccounts: [],
  ledgerTxs: [],
  ledgerEntries: [],
  simRuns: new Map(),
  seq: 0,
};

function ensureSeeded(): void {
  if (store.seeded) return;
  for (const o of seedOrders()) store.orders.set(o.id, o);
  for (const p of seedPayouts()) store.payouts.set(p.id, p);
  for (const w of seedWhitelist()) store.whitelist.set(w.id, w);
  store.ledgerAccounts = seedLedgerAccounts();
  store.ledgerTxs = seedLedgerTransactions();
  store.ledgerEntries = seedLedgerEntries(store.ledgerAccounts, store.ledgerTxs);
  store.seeded = true;
}

function uuid(): string {
  store.seq += 1;
  const tail = String(store.seq).padStart(12, "0");
  return `00000000-mock-4000-8000-${tail}`.replace("mock", "4000");
}

function fakeTxHash(seed: string): `0x${string}` {
  let h = 0x811c9dc5;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  const chunk = h.toString(16).padStart(8, "0");
  return `0x${(chunk + chunk.split("").reverse().join("")).repeat(4)}`;
}

// ---------------------------------------------------------------------------
// Live order progression (deterministic wall-clock script)
// ---------------------------------------------------------------------------

interface LivePhase {
  depositDetected: boolean;
  confirmations: number;
  settled: boolean;
  depositAmount: string;
}

function livePhase(order: MockOrder): LivePhase {
  const elapsed = Date.now() - (order.__liveStart ?? 0);
  const mode = order.__live;
  if (!mode || mode === "idle" || elapsed < DETECT_AT_MS) {
    return { depositDetected: false, confirmations: 0, settled: false, depositAmount: "0" };
  }
  const expected = BigInt(order.expected_amount);
  const depositAmount = mode === "to_partial" ? (expected * 60n) / 100n : expected;
  const confirmations = Math.min(
    REQUIRED_CONFIRMATIONS,
    Math.floor((elapsed - DETECT_AT_MS) / CONF_INTERVAL_MS),
  );
  const settled = confirmations >= REQUIRED_CONFIRMATIONS;
  return { depositDetected: true, confirmations, settled, depositAmount: depositAmount.toString() };
}

/** Advance a live order's materialized fields from its current phase. */
function refreshLive(order: MockOrder): void {
  const phase = livePhase(order);
  if (!phase.depositDetected) return;
  order.received_amount = phase.settled ? phase.depositAmount : "0";
  if (phase.settled && order.status !== "PAID" && order.status !== "PARTIALLY_PAID") {
    order.status = order.__live === "to_partial" ? "PARTIALLY_PAID" : "PAID";
    order.settled_at = new Date().toISOString();
    order.deposit_address_deployed = true;
  }
}

function beginLive(order: MockOrder): void {
  if (order.__live === "idle" && order.status === "AWAITING_PAYMENT") {
    order.__live = Date.now() > new Date(order.expires_at).getTime() ? "idle" : pickLiveMode(order.id);
    order.__liveStart = Date.now();
  }
}

function pickLiveMode(orderId: string): "to_paid" | "to_partial" {
  return orderId === "11112222-3333-4444-5555-666677778888" ? "to_partial" : "to_paid";
}

// ---------------------------------------------------------------------------
// Public cashier
// ---------------------------------------------------------------------------

export async function getPublicPayment(orderId: string): Promise<PublicPayment> {
  ensureSeeded();
  const order = store.orders.get(orderId);
  if (!order) throw apiError(404, "NOT_FOUND", `order ${orderId} not found`);
  beginLive(order);
  refreshLive(order);
  const decimals = order.asset_symbol === "USDC" || order.asset_symbol === "USDT" ? 6 : 18;
  const expiresMs = new Date(order.expires_at).getTime();
  return {
    order_id: order.id,
    status: order.status,
    merchant_name: MOCK_MERCHANT_NAME,
    merchant_order_ref: order.merchant_order_ref,
    chain_id: order.chain_id,
    asset_symbol: order.asset_symbol,
    token_contract: order.token_contract ?? USDC,
    decimals,
    expected_amount: order.expected_amount,
    received_amount: order.received_amount,
    deposit_address: order.deposit_address,
    deposit_address_deployed: order.deposit_address_deployed,
    expires_at: order.expires_at,
    seconds_remaining: Math.max(0, Math.floor((expiresMs - Date.now()) / 1000)),
    settled_at: order.settled_at ?? null,
    safe_head_height: 1096,
    required_confirmations: REQUIRED_CONFIRMATIONS,
    eip3009: order.eip3009 ?? null,
  };
}

export interface MockSseFrame {
  id: string;
  event: string;
  data: string;
}

export interface MockPaymentStream {
  addEventListener(type: string, handler: (ev: { data: string; lastEventId?: string }) => void): void;
  close(): void;
  setOnError(cb: (e?: unknown) => void): void;
}

/**
 * Mock SSE stream — pushes frames on the same wall-clock schedule the polling
 * projection uses, so both transports converge to identical state.
 */
export function openPaymentStream(orderId: string): MockPaymentStream {
  ensureSeeded();
  const order = store.orders.get(orderId);
  const listeners = new Map<string, Set<(ev: { data: string; lastEventId?: string }) => void>>();
  const timers: ReturnType<typeof setTimeout>[] = [];
  let eventSeq = 0;
  let closed = false;
  let onError: ((e?: unknown) => void) | undefined;

  const emit = (event: string, data: unknown) => {
    if (closed) return;
    eventSeq += 1;
    const payload = { data: JSON.stringify(data), lastEventId: String(eventSeq) };
    for (const h of listeners.get(event) ?? []) h(payload);
    for (const h of listeners.get("*") ?? []) h(payload);
  };
  const after = (ms: number, fn: () => void) => {
    timers.push(setTimeout(fn, ms));
  };

  if (!order) {
    after(10, () => onError?.(apiError(404, "NOT_FOUND", `order ${orderId} not found`)));
  } else {
    beginLive(order);
    const snapshot = async () => emit("snapshot", await getPublicPayment(orderId));
    after(50, () => void snapshot());

    // Scheduled business events — mirror livePhase() boundaries exactly.
    const deposit: SseDepositData = {
      deposit_id: `dep00000-0000-4000-8000-${orderId.slice(0, 12)}`,
      chain_id: ANVIL,
      tx_hash: fakeTxHash(orderId),
      log_index: 0,
      block_number: 1090,
      from_address: "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc",
      amount: order.__live === "to_partial" ? ((BigInt(order.expected_amount) * 60n) / 100n).toString() : order.expected_amount,
      asset_symbol: order.asset_symbol,
      deposit_category: "ORDER_MATCH",
      status: "DETECTED",
      detected_at: new Date().toISOString(),
    };
    after(DETECT_AT_MS, () => {
      refreshLive(order);
      if (livePhase(order).depositDetected) emit("block_detected", deposit);
    });
    for (let c = 1; c <= REQUIRED_CONFIRMATIONS; c++) {
      after(DETECT_AT_MS + c * CONF_INTERVAL_MS, () => {
        refreshLive(order);
        if (livePhase(order).confirmations >= c && !livePhase(order).settled) {
          const conf: SseConfirmationData = {
            deposit_id: deposit.deposit_id,
            tx_hash: deposit.tx_hash,
            block_number: deposit.block_number,
            current_confirmations: c,
            required_confirmations: REQUIRED_CONFIRMATIONS,
            safe_head_height: 1090 + c,
          };
          emit("confirmation_progress", conf);
        }
      });
    }
    after(DETECT_AT_MS + REQUIRED_CONFIRMATIONS * CONF_INTERVAL_MS + 100, () => {
      refreshLive(order);
      const status: SseOrderStatusData = {
        order_id: order.id,
        status: order.status,
        expected_amount: order.expected_amount,
        received_amount: order.received_amount,
        tx_hash: deposit.tx_hash,
        settled_at: order.settled_at ?? null,
        occurred_at: new Date().toISOString(),
      };
      emit(order.status === "PAID" ? "paid" : order.status === "PARTIALLY_PAID" ? "partially_paid" : "snapshot", status);
    });
  }

  return {
    addEventListener(type, handler) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type)!.add(handler);
    },
    close() {
      closed = true;
      for (const t of timers) clearTimeout(t);
      listeners.clear();
    },
    setOnError(cb) {
      onError = cb;
    },
  };
}

export async function submitEip3009(orderId: string, sub: Eip3009Submission): Promise<Eip3009Accepted> {
  ensureSeeded();
  const order = store.orders.get(orderId);
  if (!order) throw apiError(404, "NOT_FOUND", `order ${orderId} not found`);
  const hint = order.eip3009;
  if (!hint?.supported) throw apiError(400, "VALIDATION_FAILED", "asset does not support EIP-3009");
  if (order.status !== "AWAITING_PAYMENT" && order.status !== "PARTIALLY_PAID") {
    throw apiError(409, "ORDER_STATE_INVALID", `order is ${order.status}`);
  }
  const validBefore = BigInt(Math.floor(new Date(order.expires_at).getTime() / 1000));
  if (BigInt(Math.floor(Date.now() / 1000)) >= validBefore) {
    throw apiError(400, "VALIDATION_FAILED", "authorization validBefore already expired");
  }
  if (sub.to.toLowerCase() !== hint.router_address.toLowerCase()) {
    throw apiError(400, "SIGNER_REJECTED", "to != EIP3009PayRouter");
  }
  if (sub.value !== order.expected_amount) {
    throw apiError(400, "SIGNER_REJECTED", "value != expected_amount");
  }
  if (BigInt(sub.valid_before) !== validBefore) {
    throw apiError(400, "SIGNER_REJECTED", "validBefore != expires_at");
  }
  if (sub.nonce.toLowerCase() !== hint.auth_nonce.toLowerCase()) {
    throw apiError(400, "SIGNER_REJECTED", "nonce != authNonce");
  }
  if (sub.v !== 27 && sub.v !== 28) throw apiError(400, "VALIDATION_FAILED", "v must be 27/28");
  if (!/^0x[0-9a-fA-F]{64}$/.test(sub.r) || !/^0x[0-9a-fA-F]{64}$/.test(sub.s)) {
    throw apiError(400, "VALIDATION_FAILED", "r/s must be bytes32");
  }
  // Accepted — force the live script toward PAID so the demo flow converges.
  if (order.__live === "idle" || !order.__live) {
    order.__live = "to_paid";
    order.__liveStart = Date.now() - DETECT_AT_MS; // deposit about to land
  }
  return { order_id: order.id, intent_id: MOCK_INTENT_3009, intent_status: "QUEUED" };
}

// ---------------------------------------------------------------------------
// Merchant orders / review / payouts
// ---------------------------------------------------------------------------

export async function listOrders(q: { status?: string[] } = {}): Promise<OrderPage> {
  ensureSeeded();
  let data = [...store.orders.values()];
  for (const o of data) refreshLive(o);
  if (q.status?.length) data = data.filter((o) => q.status!.includes(o.status));
  data = data.sort((a, b) => b.created_at.localeCompare(a.created_at));
  return { data: data.map(stripLive), page: { next_cursor: null, has_more: false } };
}

export async function getOrder(orderId: string): Promise<Order> {
  ensureSeeded();
  const o = store.orders.get(orderId);
  if (!o) throw apiError(404, "NOT_FOUND", `order ${orderId} not found`);
  refreshLive(o);
  return stripLive(o);
}

function stripLive(o: MockOrder): Order {
  const { __live, __liveStart, ...rest } = o;
  return rest;
}

export async function reviewOrder(orderId: string, req: ReviewOrderRequest): Promise<ReviewOrderResponse> {
  ensureSeeded();
  const order = store.orders.get(orderId);
  if (!order) throw apiError(404, "NOT_FOUND", `order ${orderId} not found`);
  const reviewable = order.status === "UNDERPAID_EXPIRED_REVIEW" || order.status === "LATE_PAID_REVIEW";
  if (!reviewable) {
    throw apiError(409, "ORDER_STATE_INVALID", `order ${orderId} is ${order.status}, not reviewable`);
  }
  if (req.action === "ACCEPT_UNDERPAID_DISCOUNT") {
    if (order.status !== "UNDERPAID_EXPIRED_REVIEW") {
      throw apiError(400, "VALIDATION_FAILED", "ACCEPT_UNDERPAID_DISCOUNT requires UNDERPAID_EXPIRED_REVIEW");
    }
    order.status = "PAID";
    order.settled_at = new Date().toISOString();
    return { order: stripLive(order) };
  }
  if (req.action === "ACCEPT_LATE_PAYMENT") {
    if (order.status !== "LATE_PAID_REVIEW") {
      throw apiError(400, "VALIDATION_FAILED", "ACCEPT_LATE_PAYMENT requires LATE_PAID_REVIEW");
    }
    order.status = "PAID";
    order.settled_at = new Date().toISOString();
    return { order: stripLive(order) };
  }
  // APPROVE_REFUND — requires the 127B RefundTicket field decomposition,
  // including the ticket-bound payout_uuid (openapi: required inside `refund`;
  // it becomes merchant_payouts.id and the signer byte-compares it).
  const refund = req.refund;
  if (!refund) {
    throw apiError(400, "VALIDATION_FAILED", "refund (RefundTicket fields) is required for APPROVE_REFUND");
  }
  if (!isUuid(refund.payout_uuid)) {
    throw apiError(400, "VALIDATION_FAILED", "refund.payout_uuid is required for APPROVE_REFUND and must be a UUID — it binds the ticket to merchant_payouts.id");
  }
  if (refund.retry_seq !== 0) {
    throw apiError(400, "VALIDATION_FAILED", "refund.retry_seq must be 0 — the new payout starts at retry_seq 0 (retries re-issue a fresh ticket)");
  }
  if (!SIG64_RE.test(refund.admin_signature ?? "")) {
    throw apiError(400, "REFUND_TICKET_INVALID", "admin_signature must be a raw 64B Ed25519 signature hex");
  }
  if (!refund.to_address || !EVM_ADDR_RE.test(refund.to_address)) {
    throw apiError(400, "VALIDATION_FAILED", "refund.to_address must be an EVM address");
  }
  if (store.payouts.has(refund.payout_uuid.toLowerCase())) {
    throw apiError(409, "IDEMPOTENCY_CONFLICT", `payout_uuid ${refund.payout_uuid} is already bound to a payout`);
  }
  order.status = "REFUND_IN_PROGRESS";
  const payout: Payout = {
    id: refund.payout_uuid.toLowerCase(),
    merchant_id: order.merchant_id,
    payout_kind: "CUSTOMER_REFUND",
    source_account_type: "SUSPENSE_REFUNDABLE_LIAB",
    source_order_id: order.id,
    chain_id: order.chain_id,
    asset_symbol: order.asset_symbol,
    to_address: refund.to_address,
    amount: order.received_amount === "0" ? order.expected_amount : order.received_amount,
    retry_seq: refund.retry_seq,
    status: "LOCKED_QUEUED",
    outbound_intent_id: null,
    tx_hash: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
  store.payouts.set(payout.id, payout);
  return { order: stripLive(order), payout };
}

export async function listPayouts(): Promise<PayoutPage> {
  ensureSeeded();
  const data = [...store.payouts.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
  return { data, page: { next_cursor: null, has_more: false } };
}

export async function createPayout(req: CreatePayoutRequest): Promise<Payout> {
  ensureSeeded();
  if (!isUuid(req.payout_uuid)) {
    throw apiError(400, "VALIDATION_FAILED", "payout_uuid is required and must be a UUID — it becomes merchant_payouts.id");
  }
  const replay = idempotentPayoutReplay(req.payout_uuid, {
    payout_kind: "WITHDRAWAL",
    source_account_type: req.source_account,
    source_order_id: req.source_order_id ?? null,
    chain_id: req.chain_id,
    asset_symbol: req.asset_symbol,
    to_address: req.to_address,
    amount: req.amount,
  });
  if (replay) return replay;
  const payout: Payout = {
    id: req.payout_uuid.toLowerCase(),
    merchant_id: MOCK_MERCHANT_ID,
    payout_kind: "WITHDRAWAL",
    source_account_type: req.source_account,
    source_order_id: req.source_order_id ?? null,
    chain_id: req.chain_id,
    asset_symbol: req.asset_symbol,
    to_address: req.to_address,
    amount: req.amount,
    retry_seq: 0,
    status: "LOCKED_QUEUED",
    outbound_intent_id: null,
    tx_hash: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
  store.payouts.set(payout.id, payout);
  return payout;
}

export async function createRefund(req: CreateRefundRequest): Promise<Payout> {
  ensureSeeded();
  // openapi CreateRefundRequest: payout_uuid is the idempotency anchor AND the
  // raw16 embedded in the RefundTicket — it becomes merchant_payouts.id.
  if (!isUuid(req.payout_uuid)) {
    throw apiError(400, "VALIDATION_FAILED", "payout_uuid is required and must be a UUID — it becomes merchant_payouts.id and must equal the ticket-embedded uuid");
  }
  const ticket = req.refund_ticket;
  if (!ticket) {
    throw apiError(400, "VALIDATION_FAILED", "refund_ticket (RefundTicket fields) is required");
  }
  if (ticket.payout_uuid != null && ticket.payout_uuid.toLowerCase() !== req.payout_uuid.toLowerCase()) {
    throw apiError(400, "VALIDATION_FAILED", "refund_ticket.payout_uuid must equal the top-level payout_uuid when both are given");
  }
  if (ticket.retry_seq !== 0) {
    throw apiError(400, "VALIDATION_FAILED", "refund_ticket.retry_seq must be 0 on creation — the row starts at 0 and retries re-issue a fresh ticket");
  }
  if (!SIG64_RE.test(ticket.admin_signature ?? "")) {
    throw apiError(400, "REFUND_TICKET_INVALID", "refund_ticket.admin_signature must be a 64B Ed25519 signature hex");
  }
  if (!ticket.to_address || !EVM_ADDR_RE.test(ticket.to_address)) {
    throw apiError(400, "VALIDATION_FAILED", "refund_ticket.to_address must be an EVM address");
  }
  const replay = idempotentPayoutReplay(req.payout_uuid, {
    payout_kind: "CUSTOMER_REFUND",
    source_account_type: req.source_account,
    source_order_id: req.source_order_id ?? null,
    chain_id: req.chain_id,
    asset_symbol: req.asset_symbol,
    to_address: ticket.to_address,
    amount: req.amount,
    retry_seq: ticket.retry_seq,
  });
  if (replay) return replay;
  const payout: Payout = {
    id: req.payout_uuid.toLowerCase(),
    merchant_id: MOCK_MERCHANT_ID,
    payout_kind: "CUSTOMER_REFUND",
    source_account_type: req.source_account,
    source_order_id: req.source_order_id ?? null,
    chain_id: req.chain_id,
    asset_symbol: req.asset_symbol,
    to_address: req.refund_ticket.to_address,
    amount: req.amount,
    retry_seq: req.refund_ticket.retry_seq,
    status: "LOCKED_QUEUED",
    outbound_intent_id: null,
    tx_hash: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
  store.payouts.set(payout.id, payout);
  return payout;
}

export async function cancelPayout(payoutId: string): Promise<Payout> {
  ensureSeeded();
  const p = store.payouts.get(payoutId);
  if (!p) throw apiError(404, "NOT_FOUND", `payout ${payoutId} not found`);
  if (p.status !== "LOCKED_QUEUED" && p.status !== "FAILED_RETRYABLE") {
    throw apiError(409, "PAYOUT_STATE_INVALID", `payout is ${p.status}; only LOCKED_QUEUED/FAILED_RETRYABLE can be cancelled`);
  }
  p.status = "CANCELLED_UNLOCKED";
  p.updated_at = new Date().toISOString();
  return p;
}

// ---------------------------------------------------------------------------
// Whitelist
// ---------------------------------------------------------------------------

export async function listWithdrawalAddresses(): Promise<WithdrawalAddressPage> {
  ensureSeeded();
  const data = [...store.whitelist.values()].sort((a, b) => a.address.localeCompare(b.address));
  return { data, page: { next_cursor: null, has_more: false } };
}

export async function registerWithdrawalAddress(
  req: RegisterWithdrawalAddressRequest,
): Promise<import("./types").WithdrawalAddress> {
  ensureSeeded();
  const sig = req.admin_signature ?? "";
  if (!/^(0x)?[0-9a-fA-F]{128}$/.test(sig)) {
    throw apiError(400, "VALIDATION_FAILED", "admin_signature must be a 64B Ed25519 signature hex");
  }
  if (!/^0x[0-9a-fA-F]{40}$/.test(req.address)) {
    throw apiError(400, "VALIDATION_FAILED", "address must be an EVM address");
  }
  if (req.valid_until_unix * 1000 <= Date.now()) {
    throw apiError(400, "VALIDATION_FAILED", "valid_until_unix must be in the future");
  }
  const entry: import("./types").WithdrawalAddress = {
    id: uuid(),
    merchant_id: MOCK_MERCHANT_ID,
    chain_id: req.chain_id,
    address: req.address.toLowerCase() as `0x${string}`,
    status: "PENDING_COOLDOWN",
    valid_until: new Date(req.valid_until_unix * 1000).toISOString(),
    first_seen_at: new Date().toISOString(),
    signer_revoked: false,
    created_at: new Date().toISOString(),
  };
  store.whitelist.set(entry.id, entry);
  return entry;
}

export async function revokeWithdrawalAddress(id: string): Promise<import("./types").WithdrawalAddress> {
  ensureSeeded();
  const w = store.whitelist.get(id);
  if (!w) throw apiError(404, "NOT_FOUND", `withdrawal address ${id} not found`);
  if (w.status === "REVOKED") throw apiError(409, "ALREADY_TERMINAL", "already revoked");
  w.status = "REVOKED";
  w.signer_revoked = true;
  return w;
}

// ---------------------------------------------------------------------------
// Ledger / reconciliation / ops
// ---------------------------------------------------------------------------

export async function listLedgerAccounts(): Promise<LedgerAccountsResponse> {
  ensureSeeded();
  return { data: store.ledgerAccounts };
}

export async function listLedgerTransactions(): Promise<LedgerTransactionPage> {
  ensureSeeded();
  const data = [...store.ledgerTxs].sort((a, b) => b.created_at.localeCompare(a.created_at));
  return { data, page: { next_cursor: null, has_more: false } };
}

export async function listLedgerEntries(q: { transaction_id?: string } = {}): Promise<LedgerEntryPage> {
  ensureSeeded();
  let data = [...store.ledgerEntries];
  if (q.transaction_id) data = data.filter((e) => e.transaction_id === q.transaction_id);
  data = data.sort((a, b) => b.created_at.localeCompare(a.created_at));
  return { data, page: { next_cursor: null, has_more: false } };
}

export async function getReconciliationStatus(): Promise<ReconciliationReport> {
  ensureSeeded();
  // Compute layer-1 trial balance honestly from the account board (per chain/asset).
  const byPair = new Map<string, { debit: bigint; credit: bigint; ok: boolean }>();
  for (const a of store.ledgerAccounts) {
    const key = `${a.chain_id}:${a.asset_symbol}`;
    const row = byPair.get(key) ?? { debit: 0n, credit: 0n, ok: true };
    const bal = BigInt(a.balance);
    if (bal < 0n) row.ok = false;
    if (a.normal_side === "DEBIT") row.debit += bal;
    else row.credit += bal;
    byPair.set(key, row);
  }
  const trial = [...byPair.entries()].map(([key, v]) => {
    const [chain_id, asset_symbol] = key.split(":");
    return {
      chain_id: Number(chain_id),
      asset_symbol,
      total_debit: v.debit.toString(),
      total_credit: v.credit.toString(),
      balanced: v.debit === v.credit,
      all_balances_non_negative: v.ok,
    };
  });
  const find = (type: LedgerAccount["account_type"], asset = "USDC") =>
    store.ledgerAccounts.find((a) => a.account_type === type && a.asset_symbol === asset)?.balance ?? "0";
  const custody: CustodyCheck[] = [
    {
      check: "UNSWEPT_DEPOSIT_VAULTS",
      chain_id: ANVIL,
      asset_symbol: "USDC",
      ledger_account: "CUSTODY_UNSWEPT",
      ledger_balance: find("CUSTODY_UNSWEPT"),
      onchain_balance: find("CUSTODY_UNSWEPT"),
      delta: "0",
      matched: true,
      safe_head_height: 1096,
    },
    {
      check: "COLD_WALLET_BALANCE",
      chain_id: ANVIL,
      asset_symbol: "USDC",
      ledger_account: "CUSTODY_COLD",
      ledger_balance: find("CUSTODY_COLD"),
      onchain_balance: find("CUSTODY_COLD"),
      delta: "0",
      matched: true,
      safe_head_height: 1096,
    },
    {
      check: "PAYOUT_HOT_WALLET_BALANCE",
      chain_id: ANVIL,
      asset_symbol: "USDC",
      ledger_account: "CUSTODY_HOT_PAYOUT",
      ledger_balance: find("CUSTODY_HOT_PAYOUT"),
      onchain_balance: find("CUSTODY_HOT_PAYOUT"),
      delta: "0",
      matched: true,
      safe_head_height: 1096,
    },
    {
      check: "HOT_WALLETS_NATIVE_BALANCE",
      chain_id: ANVIL,
      asset_symbol: "ETH",
      ledger_account: "HOT_WALLET_NATIVE",
      ledger_balance: find("HOT_WALLET_NATIVE", "ETH"),
      onchain_balance: find("HOT_WALLET_NATIVE", "ETH"),
      delta: "0",
      matched: true,
      safe_head_height: 1096,
    },
  ];
  return {
    run_id: "r0000000-0000-4000-8000-000000000001",
    trigger: "SCHEDULED",
    status: "PASSED",
    started_at: new Date(Date.now() - 120_000).toISOString(),
    finished_at: new Date(Date.now() - 118_000).toISOString(),
    trial_balances: trial,
    custody_checks: custody,
    failures: [],
  };
}

export async function runReconciliation(): Promise<ReconciliationRunAccepted> {
  return { run_id: uuid(), status: "RUNNING" };
}

export async function getOutboundQueue(): Promise<OutboundQueueView> {
  ensureSeeded();
  const intents = seedOutboundIntents();
  return {
    nonce_cursors: seedNonceCursors(),
    halted_wallets: intents
      .filter((i) => i.status === "NONCE_STOLEN_HALT")
      .map((i) => ({ chain_id: i.chain_id, wallet_address: i.wallet_address, intent_id: i.id })),
    data: intents,
    page: { next_cursor: null, has_more: false },
  };
}

export async function getSignerHealth(): Promise<SignerHealth> {
  ensureSeeded();
  return seedSignerHealth();
}

export async function listQuarantinedVaults(): Promise<QuarantinedVaultPage> {
  ensureSeeded();
  return { data: seedQuarantinedVaults(), page: { next_cursor: null, has_more: false } };
}

export async function unfreezeMerchantPayouts(merchantId: string): Promise<MerchantFreezeState> {
  ensureSeeded();
  if (merchantId !== MOCK_MERCHANT_ID) throw apiError(404, "NOT_FOUND", "merchant not found");
  return { merchant_id: merchantId, payout_frozen: false, reorg_receivable_balance: "0", unfrozen_at: new Date().toISOString() };
}

// ---------------------------------------------------------------------------
// Dev simulator
// ---------------------------------------------------------------------------

interface CannedScenario {
  steps: Array<Pick<SimStep, "name" | "detail" | "tx_hash">>;
  summary: Record<string, unknown>;
}

const SCENARIOS: Record<SimScenario, CannedScenario> = {
  "shallow-reorg": {
    steps: [
      { name: "anvil snapshot", detail: "evm_snapshot -> 0x1" },
      { name: "广播未过 safe_head 的入站充值 + 出站归集", detail: "deposit@1093, sweep intent nonce=7@1094", tx_hash: fakeTxHash("shallow-deposit") },
      { name: "回滚至快照并重挖竞争分叉", detail: "evm_revert(0x1) -> re-mine heights 1093..1096" },
      { name: "校验孤块标记", detail: "onchain_deposits -> ORPHANED_SHALLOW; outbound attempt INCLUDED_UNSAFE -> BROADCASTED" },
      { name: "新主链无冲突重打包", detail: "intent IN_FLIGHT -> INCLUDED_UNSAFE (new block_hash), 无唯一索引冲突" },
    ],
    summary: {
      fork_point_height: 1092,
      canonical_heights: [1093, 1094, 1095, 1096],
      orphan_heights: [1093, 1094],
      orphaned_deposits: [{ tx_hash: fakeTxHash("shallow-deposit"), status: "ORPHANED_SHALLOW" }],
      reincluded_intent: { nonce: 7, result: "BROADCASTED -> INCLUDED_UNSAFE" },
    },
  },
  "deep-reorg": {
    steps: [
      { name: "h=100 入站充值结算", detail: "DEPOSIT_FINALIZED + ORDER_SETTLE, 商户可用余额入账" },
      { name: "h=105 批量归集入冷金库", detail: "SWEEP_SETTLE: DR CUSTODY_COLD / CR CUSTODY_UNSWEPT" },
      { name: "商户提现消耗可用余额", detail: "PAYOUT_FLOW 锁定并广播" },
      { name: "强制 evm_revert 至 h=99", detail: "触发深度重组, 启动 LIFO 逆序回滚" },
      { name: "105 -> 100 逆序红冲", detail: "先冲归集恢复 CUSTODY_UNSWEPT, 再冲入站结算, 透支差额 -> MERCHANT_REORG_RECEIVABLE" },
      { name: "商户提现自动冻结", detail: "merchants.payout_frozen = true, 审计事件落盘" },
    ],
    summary: {
      fork_point_height: 99,
      canonical_heights: [100, 101, 102, 103, 104, 105],
      orphan_heights: [100, 101, 102, 103, 104, 105],
      payout_frozen: true,
      reorg_receivable: "200000000",
      lifo_reversals: [
        {
          height: 105,
          kind: "OUTBOUND_SWEEP_REVERSAL",
          entries: [
            { direction: "DEBIT", account_type: "CUSTODY_UNSWEPT", amount: "5000000000" },
            { direction: "CREDIT", account_type: "CUSTODY_COLD", amount: "5000000000" },
          ],
        },
        {
          height: 104,
          kind: "OUTBOUND_GAS_REVERSAL",
          entries: [
            { direction: "DEBIT", account_type: "HOT_WALLET_NATIVE", amount: "21000000000000" },
            { direction: "CREDIT", account_type: "GAS_EXPENSE", amount: "21000000000000" },
          ],
        },
        {
          height: 100,
          kind: "INBOUND_ORDER_SETTLE_REVERSAL",
          entries: [
            { direction: "DEBIT", account_type: "MERCHANT_AVAILABLE", amount: "200000000" },
            { direction: "DEBIT", account_type: "MERCHANT_REORG_RECEIVABLE", amount: "200000000" },
            { direction: "CREDIT", account_type: "CUSTODY_UNSWEPT", amount: "400000000" },
          ],
        },
      ],
    },
  },
  "blacklist-returnbomb": {
    steps: [
      { name: "MockUSDT 拉黑 1/5 归集地址", detail: "isBlackListed[vault_3] = true" },
      { name: "部署 MockReturnBombToken 并混入归集批", detail: "returndata = 2MB, 期望内层截断" },
      { name: "触发 deployAndSweepBatch", detail: "5 items, exact sweep, gas cap 120k per vault", tx_hash: fakeTxHash("sweep-batch") },
      { name: "校验隔离", detail: "4/5 SWEPT 足额入冷; 1/5 VaultQuarantined -> is_quarantined" },
      { name: "注入链上 Revert 出站交易", detail: "receipt.status=0, 仍记 GAS_SETTLE" },
      { name: "HOT_WALLET_NATIVE 1-Wei 账平", detail: "onchain ETH == ledger balance (delta=0)" },
    ],
    summary: {
      swept_count: 4,
      quarantined_count: 1,
      returnbomb_returndata_bytes: 2 * 1024 * 1024,
      returnbomb_read_bytes: 32,
      quarantined: [{ address: "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", reason: "SWEEP_REVERT_BLACKLISTED" }],
      gas_settle: { gas_used: 46231, effective_gas_price: "1200000000", matched_1wei: true },
    },
  },
  "signer-penetration": {
    steps: [
      { name: "伪造 ①: 篡改冷金库归集目标", detail: "deployAndSweepBatch(target=evil) -> SWEEP_POLICY REJECT" },
      { name: "伪造 ②: 篡改 salt 的 EIP-3009 中继", detail: "authNonce != keccak256(chain_id, salt) -> REJECT" },
      { name: "伪造 ③: 未满 24h 冷却期提现", detail: "now < first_seen_at+24h -> PAYOUT_POLICY REJECT" },
      { name: "伪造 ④: 超额 RefundTicket 退款", detail: "ticket.exact_amount != calldata.amount -> REJECT" },
      { name: "合法: 同 Nonce RBF 提价", detail: "幂等槽命中, 字段逐字节相等 -> SIGN (日限额不重复扣减)" },
      { name: "合法: GAP_FILLER 自转取消", detail: "filler_signed=true, 不覆盖原授权字段 -> SIGN" },
      { name: "VerifyAuditChain", detail: "SHA-256 哈希链完整, valid=true, entries=6" },
    ],
    summary: {
      rejected_requests: [
        { name: "篡改冷金库归集", policy: "SWEEP_POLICY", result: "REJECTED", reason: "target ∉ allowed_cold_wallets" },
        { name: "篡改 salt EIP-3009 中继", policy: "EIP3009_RELAY_POLICY", result: "REJECTED", reason: "authNonce 自算强校验失败" },
        { name: "未满 24h 冷却期提现", policy: "PAYOUT_POLICY", result: "REJECTED", reason: "now < first_seen_at + 24h" },
        { name: "超额/无签名 RefundTicket", policy: "PAYOUT_POLICY", result: "REJECTED", reason: "票据字段与 calldata 非逐字节相等" },
      ],
      legit_requests: [
        { name: "同 Nonce RBF 提价", result: "SIGNED", slot: "幂等命中, 限额仅扣一次" },
        { name: "GAP_FILLER 自转取消", result: "SIGNED", slot: "filler_signed, 授权字段不变" },
      ],
      audit_chain_valid: true,
      audit_entries: 6,
      reject_rate: "4/4",
    },
  },
};

export async function runSimScenario(scenario: SimScenario): Promise<SimRun> {
  ensureSeeded();
  const def = SCENARIOS[scenario];
  if (!def) throw apiError(404, "NOT_FOUND", `unknown scenario ${scenario}`);
  const run: MockSimRunInternal = {
    run_id: uuid(),
    scenario,
    status: "RUNNING",
    steps: def.steps.map((s, i) => ({ seq: i + 1, name: s.name, status: "PENDING", detail: s.detail, tx_hash: s.tx_hash ?? null, at: new Date().toISOString() })),
    summary: undefined,
    error: null,
    started_at: new Date().toISOString(),
    finished_at: null,
    __startedMs: Date.now(),
  };
  store.simRuns.set(run.run_id, run);
  return publicSimRun(run);
}

const STEP_MS = 900;

function publicSimRun(run: MockSimRunInternal): SimRun {
  const elapsed = Date.now() - run.__startedMs;
  const doneCount = Math.min(run.steps.length, Math.floor(elapsed / STEP_MS) + 1);
  run.steps.forEach((s, i) => {
    if (i < doneCount) {
      s.status = "OK";
      s.at = new Date(run.__startedMs + (i + 1) * STEP_MS).toISOString();
    } else if (i === doneCount) {
      s.status = "RUNNING";
    } else {
      s.status = "PENDING";
    }
  });
  const finished = doneCount >= run.steps.length;
  const { __startedMs, ...pub } = run;
  if (finished) {
    pub.status = "COMPLETED";
    pub.finished_at = new Date(run.__startedMs + run.steps.length * STEP_MS).toISOString();
    pub.summary = SCENARIOS[run.scenario].summary;
  } else {
    pub.status = "RUNNING";
    pub.finished_at = null;
  }
  return pub;
}

export async function getSimRun(runId: string): Promise<SimRun> {
  ensureSeeded();
  const run = store.simRuns.get(runId);
  if (!run) throw apiError(404, "NOT_FOUND", `sim run ${runId} not found`);
  return publicSimRun(run);
}

export async function simAnvil(
  action: SimAnvilAction,
  params: Record<string, unknown> = {},
): Promise<{ action: SimAnvilAction; result: unknown }> {
  ensureSeeded();
  switch (action) {
    case "snapshot":
      return { action, result: "0x1" };
    case "mine":
      return { action, result: "0x0" };
    case "revert":
      if (!params.snapshot_id) throw apiError(400, "VALIDATION_FAILED", "revert requires params.snapshot_id");
      return { action, result: true };
    case "drop-transaction":
      if (!params.tx_hash) throw apiError(400, "VALIDATION_FAILED", "drop-transaction requires params.tx_hash");
      return { action, result: params.tx_hash };
    default:
      throw apiError(400, "VALIDATION_FAILED", `unknown anvil action ${action}`);
  }
}

export { ApiError };

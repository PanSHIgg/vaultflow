// W6 — React hooks: react-query wrappers over api.ts plus the cashier's
// SSE-with-3s-polling-fallback live transport (openapi.yaml retry semantics).

"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import * as api from "./api";
import type {
  PaymentOrderStatus,
  PublicPayment,
  SseConfirmationData,
  SseDepositData,
  SseOrderStatusData,
} from "./types";

// ---------------------------------------------------------------------------
// Cashier live order feed — SSE primary, 3s polling fallback
// ---------------------------------------------------------------------------

const STATUS_EVENTS = [
  "paid",
  "overpaid",
  "partially_paid",
  "expired",
  "underpaid_expired_review",
  "late_paid_review",
  "refund_in_progress",
  "refunded",
  "settled_reversed",
  "reversal_resolved",
] as const;

export interface PaymentLiveState {
  payment?: PublicPayment;
  transport: "connecting" | "sse" | "polling";
  deposits: SseDepositData[];
  confirmation: SseConfirmationData | null;
  lastEvent?: { type: string; at: number };
  error?: string;
}

export function usePublicPayment(orderId: string): PaymentLiveState {
  const [state, setState] = useState<PaymentLiveState>({
    transport: "connecting",
    deposits: [],
    confirmation: null,
  });

  useEffect(() => {
    let cancelled = false;
    let stream: api.PaymentEventSource | undefined;
    let pollTimer: ReturnType<typeof setTimeout> | undefined;
    let polling = false;
    let errorCount = 0;
    // Last SSE frame id seen — supplied as ?lastEventId= if the stream is ever
    // re-opened, so the server replays missed frames from its buffer (FIX-D4;
    // native EventSource sends the Last-Event-ID header on its own reconnects).
    let lastSeq: string | undefined;

    const trackSeq = (ev: { lastEventId?: string }) => {
      if (ev.lastEventId) lastSeq = ev.lastEventId;
    };

    const parse = <T,>(ev: { data: string }): T | undefined => {
      try {
        return JSON.parse(ev.data) as T;
      } catch {
        return undefined;
      }
    };

    const startPolling = () => {
      if (polling || cancelled) return;
      polling = true;
      stream?.close();
      stream = undefined;
      setState((s) => ({ ...s, transport: "polling" }));
      const tick = async () => {
        try {
          const p = await api.getPublicPayment(orderId);
          if (cancelled) return;
          setState((s) => ({ ...s, payment: p, error: undefined }));
        } catch (e) {
          if (cancelled) return;
          setState((s) => ({
            ...s,
            error: e instanceof Error ? e.message : String(e),
          }));
        }
        if (!cancelled) pollTimer = setTimeout(tick, 3_000);
      };
      void tick();
    };

    void api
      .getPublicPayment(orderId)
      .then((p) => {
        if (cancelled) return;
        setState((s) => ({ ...s, payment: p, error: undefined }));
      })
      .catch((e) => {
        if (cancelled) return;
        setState((s) => ({
          ...s,
          error: e instanceof Error ? e.message : String(e),
        }));
      });

    stream = api.openPaymentEvents(orderId, lastSeq);
    stream.setOnError(() => {
      errorCount += 1;
      // Two consecutive errors with no successful frame => degrade to polling.
      if (errorCount >= 2) startPolling();
    });
    stream.addEventListener("snapshot", (ev) => {
      errorCount = 0;
      trackSeq(ev);
      const p = parse<PublicPayment>(ev);
      if (p) setState((s) => ({ ...s, transport: "sse", payment: p }));
    });
    stream.addEventListener("block_detected", (ev) => {
      errorCount = 0;
      trackSeq(ev);
      const d = parse<SseDepositData>(ev);
      if (d) {
        setState((s) => ({
          ...s,
          deposits: s.deposits.some((x) => x.deposit_id === d.deposit_id)
            ? s.deposits
            : [...s.deposits, d],
          lastEvent: { type: "block_detected", at: Date.now() },
        }));
      }
    });
    stream.addEventListener("confirmation_progress", (ev) => {
      errorCount = 0;
      trackSeq(ev);
      const c = parse<SseConfirmationData>(ev);
      if (c) {
        setState((s) => ({
          ...s,
          confirmation: c,
          lastEvent: { type: "confirmation_progress", at: Date.now() },
        }));
      }
    });
    for (const t of STATUS_EVENTS) {
      stream.addEventListener(t, (ev) => {
        errorCount = 0;
        trackSeq(ev);
        const d = parse<SseOrderStatusData>(ev);
        if (!d) return;
        setState((s) => ({
          ...s,
          payment: s.payment
            ? {
                ...s.payment,
                status: d.status,
                received_amount: d.received_amount,
                expected_amount: d.expected_amount,
                settled_at: d.settled_at ?? s.payment.settled_at,
                expires_at: d.expires_at ?? s.payment.expires_at,
              }
            : s.payment,
          lastEvent: { type: t, at: Date.now() },
        }));
      });
    }

    return () => {
      cancelled = true;
      stream?.close();
      if (pollTimer) clearTimeout(pollTimer);
    };
  }, [orderId]);

  return state;
}

/** Ticking countdown (seconds) to an ISO timestamp. */
export function useCountdownSeconds(expiresAt?: string): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1_000);
    return () => clearInterval(t);
  }, []);
  if (!expiresAt) return 0;
  return Math.max(0, Math.floor((Date.parse(expiresAt) - now) / 1000));
}

// ---------------------------------------------------------------------------
// react-query wrappers (dashboard polling cadences)
// ---------------------------------------------------------------------------

export function useOrders(status?: PaymentOrderStatus[]) {
  return useQuery({
    queryKey: ["orders", status?.join(",") ?? "all"],
    queryFn: () => api.listOrders({ status }),
    refetchInterval: 5_000,
  });
}

export function useOrder(orderId?: string) {
  return useQuery({
    queryKey: ["order", orderId],
    queryFn: () => api.getOrder(orderId!),
    enabled: !!orderId,
  });
}

export function usePayouts() {
  return useQuery({ queryKey: ["payouts"], queryFn: () => api.listPayouts(), refetchInterval: 6_000 });
}

export function useLedgerAccounts() {
  return useQuery({ queryKey: ["ledger-accounts"], queryFn: () => api.listLedgerAccounts(), refetchInterval: 5_000 });
}

export function useLedgerTransactions() {
  return useQuery({ queryKey: ["ledger-txs"], queryFn: () => api.listLedgerTransactions(), refetchInterval: 8_000 });
}

export function useLedgerEntries(transactionId?: string) {
  return useQuery({
    queryKey: ["ledger-entries", transactionId ?? "all"],
    queryFn: () => api.listLedgerEntries({ transaction_id: transactionId }),
    refetchInterval: 8_000,
  });
}

export function useReconciliation() {
  return useQuery({ queryKey: ["recon"], queryFn: () => api.getReconciliationStatus(), refetchInterval: 8_000 });
}

export function useOutboundQueue() {
  return useQuery({ queryKey: ["outbound"], queryFn: () => api.getOutboundQueue(), refetchInterval: 4_000 });
}

export function useSignerHealth() {
  return useQuery({ queryKey: ["signer-health"], queryFn: () => api.getSignerHealth(), refetchInterval: 10_000 });
}

export function useQuarantinedVaults() {
  return useQuery({ queryKey: ["quarantine"], queryFn: () => api.listQuarantinedVaults(), refetchInterval: 8_000 });
}

export function useWhitelist() {
  return useQuery({ queryKey: ["whitelist"], queryFn: () => api.listWithdrawalAddresses(), refetchInterval: 8_000 });
}

export function useSimRun(runId?: string) {
  return useQuery({
    queryKey: ["sim-run", runId],
    queryFn: () => api.getSimRun(runId!),
    enabled: !!runId,
    refetchInterval: (q) => (q.state.data?.status === "RUNNING" ? 800 : false),
  });
}

// --- mutations ---------------------------------------------------------------

function useInvalidator() {
  const qc = useQueryClient();
  return (...keys: string[]) => {
    for (const k of keys) void qc.invalidateQueries({ queryKey: [k] });
  };
}

export function useReviewOrder() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: ({ orderId, req }: { orderId: string; req: Parameters<typeof api.reviewOrder>[1] }) =>
      api.reviewOrder(orderId, req),
    onSuccess: () => invalidate("orders", "payouts", "ledger-accounts"),
  });
}

export function useCancelPayout() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: (payoutId: string) => api.cancelPayout(payoutId),
    onSuccess: () => invalidate("payouts", "ledger-accounts"),
  });
}

export function useRegisterAddress() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: (req: Parameters<typeof api.registerWithdrawalAddress>[0]) =>
      api.registerWithdrawalAddress(req),
    onSuccess: () => invalidate("whitelist"),
  });
}

export function useRevokeAddress() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: (id: string) => api.revokeWithdrawalAddress(id),
    onSuccess: () => invalidate("whitelist"),
  });
}

export function useCreateRefund() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: (req: Parameters<typeof api.createRefund>[0]) => api.createRefund(req),
    onSuccess: () => invalidate("payouts", "orders", "ledger-accounts"),
  });
}

export function useRunReconciliation() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: () => api.runReconciliation(),
    onSuccess: () => invalidate("recon"),
  });
}

export function useRunScenario() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: (scenario: Parameters<typeof api.runSimScenario>[0]) => api.runSimScenario(scenario),
    onSuccess: () => invalidate("sim-run"),
  });
}

export function useSimAnvil() {
  return useMutation({
    mutationFn: ({ action, params }: { action: Parameters<typeof api.simAnvil>[0]; params?: Record<string, unknown> }) =>
      api.simAnvil(action, params ?? {}),
  });
}

export function useUnfreezeMerchant() {
  const invalidate = useInvalidator();
  return useMutation({
    mutationFn: (merchantId: string) => api.unfreezeMerchantPayouts(merchantId),
    onSuccess: () => invalidate("ledger-accounts", "recon"),
  });
}

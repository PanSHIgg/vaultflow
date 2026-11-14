// W6-1/W6-2 acceptance: the mock engine must behave like the openapi.yaml
// contract it stands in for — 404 on unknown orders, EIP-3009 binding
// validation (to/value/validBefore/nonce), review state machine, payout cancel.

import { describe, expect, it } from "vitest";
import { ApiError } from "@/lib/errors";
import * as mock from "@/lib/mock-engine";
import { DEMO_ORDER_ID } from "@/lib/mock-data";
import type { Eip3009Submission } from "@/lib/types";

const VALID_SUB: Eip3009Submission = {
  from: "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc",
  to: "0x7777777777777777777777777777777777777777",
  value: "1500000",
  valid_after: 0,
  valid_before: 0, // filled per-order below
  nonce: "0x3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225",
  v: 27,
  r: `0x${"11".repeat(32)}`,
  s: `0x${"22".repeat(32)}`,
};

describe("getPublicPayment", () => {
  it("serves the seeded demo order with the golden-vector auth_nonce", async () => {
    const p = await mock.getPublicPayment(DEMO_ORDER_ID);
    expect(p.status).toBe("AWAITING_PAYMENT");
    expect(p.eip3009?.auth_nonce).toBe(
      "0x3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225",
    );
    // golden vault for (31337, 00112233…eeff, ffeeddcc…221100)
    expect(p.deposit_address).toBe("0x7fc0911310dd4b4d5cc547658724b58857e11557");
    expect(p.required_confirmations).toBe(6);
  });

  it("404s unknown orders with the Error envelope shape", async () => {
    await expect(mock.getPublicPayment("00000000-0000-4000-8000-00000000dead")).rejects.toMatchObject({
      status: 404,
      code: "NOT_FOUND",
    } satisfies Partial<ApiError>);
  });
});

describe("submitEip3009 binding validation", () => {
  async function sub(overrides: Partial<Eip3009Submission> = {}) {
    const p = await mock.getPublicPayment(DEMO_ORDER_ID);
    const validBefore = Math.floor(Date.parse(p.expires_at) / 1000);
    return mock.submitEip3009(DEMO_ORDER_ID, {
      ...VALID_SUB,
      valid_before: validBefore,
      nonce: p.eip3009!.auth_nonce,
      ...overrides,
    });
  }

  it("accepts a fully-bound authorization", async () => {
    const res = await sub();
    expect(res.intent_status).toBe("QUEUED");
    expect(res.order_id).toBe(DEMO_ORDER_ID);
  });

  it("rejects wrong router (to != EIP3009PayRouter)", async () => {
    await expect(sub({ to: "0x9999999999999999999999999999999999999999" })).rejects.toMatchObject({
      code: "SIGNER_REJECTED",
    });
  });

  it("rejects wrong value / validBefore / nonce", async () => {
    await expect(sub({ value: "1" })).rejects.toMatchObject({ code: "SIGNER_REJECTED" });
    await expect(sub({ valid_before: 1 })).rejects.toMatchObject({ code: "SIGNER_REJECTED" });
    await expect(
      sub({ nonce: `0x${"aa".repeat(32)}` }),
    ).rejects.toMatchObject({ code: "SIGNER_REJECTED" });
  });
});

describe("review + payouts", () => {
  it("APPROVE_REFUND requires a 64B Ed25519 signature hex", async () => {
    const orders = await mock.listOrders({ status: ["LATE_PAID_REVIEW"] });
    const target = orders.data[0];
    await expect(
      mock.reviewOrder(target.id, {
        action: "APPROVE_REFUND",
        refund: {
          to_address: "0x5555555555555555555555555555555555555555",
          payout_uuid: "00112233-4455-6677-8899-aabbccddeeff",
          retry_seq: 0,
          expiry_unix: 1,
          admin_signature: "0x1234",
        },
      }),
    ).rejects.toMatchObject({ code: "REFUND_TICKET_INVALID" });
  });

  it("ACCEPT_LATE_PAYMENT flips LATE_PAID_REVIEW -> PAID", async () => {
    const orders = await mock.listOrders({ status: ["LATE_PAID_REVIEW"] });
    const target = orders.data[0];
    const res = await mock.reviewOrder(target.id, { action: "ACCEPT_LATE_PAYMENT" });
    expect(res.order.status).toBe("PAID");
    // terminal now: second review rejected
    await expect(mock.reviewOrder(target.id, { action: "ACCEPT_LATE_PAYMENT" })).rejects.toMatchObject({
      code: "ORDER_STATE_INVALID",
    });
  });

  it("cancel only from LOCKED_QUEUED / FAILED_RETRYABLE", async () => {
    const payouts = await mock.listPayouts();
    const failed = payouts.data.find((p) => p.status === "FAILED_RETRYABLE")!;
    const cancelled = await mock.cancelPayout(failed.id);
    expect(cancelled.status).toBe("CANCELLED_UNLOCKED");
    const inFlight = payouts.data.find((p) => p.status === "IN_FLIGHT")!;
    await expect(mock.cancelPayout(inFlight.id)).rejects.toMatchObject({ code: "PAYOUT_STATE_INVALID" });
  });
});

// FIX-F F2 — mock must reject the same contract violations the real gateway
// does, so the UI cannot ship a guaranteed-422 refund request unnoticed.
describe("refund contract alignment (FIX-F regression)", () => {
  const REFUND_SIG = `0x${"cd".repeat(64)}`;
  const REFUND_TO = "0x5555555555555555555555555555555555555555" as const;

  function refundReq(overrides: Record<string, unknown> = {}) {
    return {
      payout_uuid: crypto.randomUUID(),
      source_account: "SUSPENSE_REFUNDABLE_LIAB",
      source_order_id: null,
      chain_id: 31337,
      asset_symbol: "USDC",
      amount: "1500000",
      refund_ticket: {
        to_address: REFUND_TO,
        retry_seq: 0,
        expiry_unix: 2_000_000_000,
        admin_signature: REFUND_SIG,
      },
      ...overrides,
    } as unknown as Parameters<typeof mock.createRefund>[0];
  }

  it("createRefund rejects a missing / malformed payout_uuid", async () => {
    await expect(
      mock.createRefund(refundReq({ payout_uuid: undefined })),
    ).rejects.toMatchObject({ code: "VALIDATION_FAILED" });
    await expect(
      mock.createRefund(refundReq({ payout_uuid: "not-a-uuid" })),
    ).rejects.toMatchObject({ code: "VALIDATION_FAILED" });
  });

  it("createRefund rejects refund_ticket.payout_uuid != top-level payout_uuid", async () => {
    const req = refundReq();
    req.refund_ticket = { ...req.refund_ticket, payout_uuid: crypto.randomUUID() };
    await expect(mock.createRefund(req)).rejects.toMatchObject({ code: "VALIDATION_FAILED" });
  });

  it("createRefund rejects retry_seq != 0 on creation", async () => {
    const req = refundReq();
    req.refund_ticket = { ...req.refund_ticket, retry_seq: 1 };
    await expect(mock.createRefund(req)).rejects.toMatchObject({ code: "VALIDATION_FAILED" });
  });

  it("createRefund binds payout_uuid as merchant_payouts.id; replays identical bodies, conflicts on drift", async () => {
    const req = refundReq();
    const created = await mock.createRefund(req);
    expect(created.id).toBe(req.payout_uuid);
    expect(created.payout_kind).toBe("CUSTOMER_REFUND");
    expect(created.retry_seq).toBe(0);
    // Idempotent replay of the identical request returns the same row.
    const replay = await mock.createRefund(req);
    expect(replay.id).toBe(created.id);
    // Same uuid + different body -> 409 IDEMPOTENCY_CONFLICT.
    await expect(mock.createRefund({ ...req, amount: "999" })).rejects.toMatchObject({
      code: "IDEMPOTENCY_CONFLICT",
      status: 409,
    });
  });

  it("APPROVE_REFUND requires refund.payout_uuid, rejects retry_seq != 0, and binds the uuid as payout.id", async () => {
    const orders = await mock.listOrders({ status: ["UNDERPAID_EXPIRED_REVIEW"] });
    const target = orders.data[0];
    const base = {
      to_address: REFUND_TO,
      retry_seq: 0,
      expiry_unix: 2_000_000_000,
      admin_signature: REFUND_SIG,
    };
    // non-zero retry_seq on a fresh payout -> rejected (uuid present so we reach the retry check)
    await expect(
      mock.reviewOrder(target.id, {
        action: "APPROVE_REFUND",
        refund: { ...base, payout_uuid: crypto.randomUUID(), retry_seq: 1 },
      }),
    ).rejects.toMatchObject({ code: "VALIDATION_FAILED" });
    // missing payout_uuid -> rejected the same way the real API 422s
    await expect(
      mock.reviewOrder(target.id, { action: "APPROVE_REFUND", refund: { ...base } }),
    ).rejects.toMatchObject({ code: "VALIDATION_FAILED" });
    // happy path — the ticket-bound uuid becomes merchant_payouts.id
    const uuid = crypto.randomUUID();
    const res = await mock.reviewOrder(target.id, {
      action: "APPROVE_REFUND",
      refund: { ...base, payout_uuid: uuid },
    });
    expect(res.order.status).toBe("REFUND_IN_PROGRESS");
    expect(res.payout?.id).toBe(uuid);
    expect(res.payout?.payout_kind).toBe("CUSTOMER_REFUND");
  });
});

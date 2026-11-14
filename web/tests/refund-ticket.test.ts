// FIX-F F1 — the exceptions refund drawer must submit a RefundTicketFields
// body whose payout_uuid is byte-identical to the raw16 embedded in the
// signed 127B payload (it becomes merchant_payouts.id, which vault-signer
// byte-compares). assembleRefundTicketFields enforces that binding plus the
// stale-field guard, so a drift is blocked client-side instead of 422ing.

import { describe, expect, it } from "vitest";
import { assembleRefundTicketFields } from "@/lib/refund-ticket";

const SNAP = {
  payout_uuid: "00112233-4455-6677-8899-aabbccddeeff",
  to_address: "0x5555555555555555555555555555555555555555",
  retry_seq: 0,
  expiry_unix: 2_000_000_000,
};

const BASE = { ...SNAP, admin_signature: `0x${"ab".repeat(64)}` };

describe("assembleRefundTicketFields", () => {
  it("emits a complete RefundTicketFields incl. the signed payout_uuid", () => {
    const fields = assembleRefundTicketFields({ ...BASE, signed: SNAP });
    expect(fields).toEqual({
      to_address: SNAP.to_address,
      payout_uuid: SNAP.payout_uuid,
      retry_seq: 0,
      expiry_unix: SNAP.expiry_unix,
      admin_signature: BASE.admin_signature,
    });
  });

  it("blocks when the submitted payout_uuid drifts from the signed-payload uuid", () => {
    expect(() =>
      assembleRefundTicketFields({
        ...BASE,
        payout_uuid: "ffffffff-ffff-ffff-ffff-ffffffffffff",
        signed: SNAP,
      }),
    ).toThrow(/payout_uuid/);
  });

  it("blocks missing / malformed payout_uuid even without a snapshot", () => {
    for (const bad of ["", "not-a-uuid", "0x00112233445566778899aabbccddeeff"]) {
      expect(() =>
        assembleRefundTicketFields({ ...BASE, payout_uuid: bad, signed: null }),
      ).toThrow(/payout_uuid/);
    }
  });

  it("blocks stale signed fields edited after signing (to_address / retry_seq / expiry_unix)", () => {
    expect(() =>
      assembleRefundTicketFields({
        ...BASE,
        to_address: "0x6666666666666666666666666666666666666666",
        signed: SNAP,
      }),
    ).toThrow(/to_address/);
    expect(() =>
      assembleRefundTicketFields({ ...BASE, retry_seq: 1, signed: SNAP }),
    ).toThrow(/retry_seq/);
    expect(() =>
      assembleRefundTicketFields({ ...BASE, expiry_unix: SNAP.expiry_unix + 1, signed: SNAP }),
    ).toThrow(/expiry_unix/);
  });

  it("accepts externally-pasted signatures (no local snapshot) once formats hold", () => {
    const fields = assembleRefundTicketFields({ ...BASE, signed: null });
    expect(fields.payout_uuid).toBe(SNAP.payout_uuid);
    expect(fields.admin_signature).toBe(BASE.admin_signature);
  });

  it("rejects malformed signature / address / retry_seq regardless of snapshot", () => {
    expect(() =>
      assembleRefundTicketFields({ ...BASE, admin_signature: "0x1234", signed: null }),
    ).toThrow(/admin_signature/);
    expect(() =>
      assembleRefundTicketFields({ ...BASE, to_address: "0x1234", signed: null }),
    ).toThrow(/to_address/);
    expect(() =>
      assembleRefundTicketFields({ ...BASE, retry_seq: -1, signed: null }),
    ).toThrow(/retry_seq/);
  });
});

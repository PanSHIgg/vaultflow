// FIX-F — RefundTicket wire assembler shared by the exception-order drawer
// and the approvals panel. The real API (docs/openapi.yaml `RefundTicketFields`
// + `CreateRefundRequest`) hard-requires the submitted `payout_uuid` to equal
// the raw16 UUID embedded in the signed 127-byte payload: it becomes
// `merchant_payouts.id` and vault-signer byte-compares it against the ticket
// (a drift is an unconditional REFUND_TICKET_INVALID). We therefore keep the
// exact field snapshot that was signed and refuse to submit on any drift —
// failing fast in the UI instead of shipping a guaranteed-422 request.

import { isUuid } from "./bytes";
import type { RefundTicketFields } from "./types";

const EVM_RE = /^0x[0-9a-fA-F]{40}$/;
const SIG_RE = /^(0x)?[0-9a-fA-F]{128}$/;

/** Field values exactly as embedded into the signed 127B REFUND_V1 payload. */
export interface RefundTicketSigningSnapshot {
  payout_uuid: string;
  to_address: string;
  retry_seq: number;
  expiry_unix: number;
}

export interface AssembleRefundTicketInput extends RefundTicketSigningSnapshot {
  admin_signature: string;
  /**
   * Snapshot captured by the in-page signer at sign time. When present, every
   * submitted field MUST equal it. A pasted signature (snapshot `null`) skips
   * the drift check — a raw Ed25519 blob cannot be introspected client-side,
   * so the UI displays the bound payout_uuid for the external signer to use.
   */
  signed?: RefundTicketSigningSnapshot | null;
}

/**
 * Validate + assemble the `refund` / `refund_ticket` wire object. Throws an
 * Error with an explicit message on any contract violation so the caller can
 * surface it in-place rather than letting the API answer 422/INVALID_TICKET.
 */
export function assembleRefundTicketFields(input: AssembleRefundTicketInput): RefundTicketFields {
  if (!isUuid(input.payout_uuid)) {
    throw new Error(
      `payout_uuid 缺失或不是合法 UUID（它同时是 merchant_payouts.id 与 127B 票据内嵌 raw16）：${input.payout_uuid || "(empty)"}`,
    );
  }
  if (input.signed) {
    if (input.signed.payout_uuid !== input.payout_uuid) {
      throw new Error(
        `提交的 payout_uuid(${input.payout_uuid}) ≠ 签名载荷内嵌的 payout_uuid(${input.signed.payout_uuid})` +
          ` — 签名器逐字节比对必拒签，请用同一 uuid 重新签署`,
      );
    }
    if (input.signed.to_address.toLowerCase() !== input.to_address.toLowerCase()) {
      throw new Error("to_address 在签名后被修改 — 提交字段必须与所签 127B 载荷逐字节一致，请重新签署");
    }
    if (input.signed.retry_seq !== input.retry_seq) {
      throw new Error(
        `retry_seq 在签名后被修改（${input.signed.retry_seq} → ${input.retry_seq}）— 请重新签署`,
      );
    }
    if (input.signed.expiry_unix !== input.expiry_unix) {
      throw new Error(
        `expiry_unix 在签名后被修改（${input.signed.expiry_unix} → ${input.expiry_unix}）— 请重新签署`,
      );
    }
  }
  if (!EVM_RE.test(input.to_address)) {
    throw new Error("to_address 必须是 0x 前缀 20B EVM 地址");
  }
  if (!Number.isInteger(input.retry_seq) || input.retry_seq < 0) {
    throw new Error("retry_seq 必须是非负整数（新票据首签为 0）");
  }
  if (!Number.isInteger(input.expiry_unix) || input.expiry_unix <= 0) {
    throw new Error("expiry_unix 必须是正整数 Unix 秒");
  }
  if (!SIG_RE.test(input.admin_signature)) {
    throw new Error("admin_signature 必须是 64B Ed25519 签名 hex（0x 可选，128 hex）");
  }
  return {
    to_address: input.to_address as RefundTicketFields["to_address"],
    payout_uuid: input.payout_uuid,
    retry_seq: input.retry_seq,
    expiry_unix: input.expiry_unix,
    admin_signature: input.admin_signature,
  };
}

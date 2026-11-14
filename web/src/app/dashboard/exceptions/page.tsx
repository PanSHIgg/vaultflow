// W6-3 — exception-order workbench: underpaid-expired & late-paid manual
// review (discount-accept / late-accept / RefundTicket-approved refund),
// plus payout lifecycle with cancel.

"use client";

import Link from "next/link";
import { useState } from "react";
import {
  AddressText,
  AmountText,
  Badge,
  btnCls,
  btnDangerCls,
  btnGhostCls,
  Card,
  CopyButton,
  ErrorBox,
  Field,
  inputCls,
  Spinner,
} from "@/components/ui";
import { useAdminKey } from "@/lib/admin-key";
import { buildRefundV1Payload } from "@/lib/ed25519";
import { assembleRefundTicketFields, type RefundTicketSigningSnapshot } from "@/lib/refund-ticket";
import {
  fmtDateTime,
  ORDER_STATUS_LABEL,
  ORDER_STATUS_TONE,
  PAYOUT_STATUS_LABEL,
  shortAddr,
} from "@/lib/format";
import { useCancelPayout, useOrders, usePayouts, useReviewOrder } from "@/lib/hooks";
import type { Order, PaymentOrderStatus, RefundTicketFields } from "@/lib/types";

const WATCHED: PaymentOrderStatus[] = [
  "UNDERPAID_EXPIRED_REVIEW",
  "LATE_PAID_REVIEW",
  "REFUND_IN_PROGRESS",
  "OVERPAID",
  "SETTLED_REVERSED",
  "PARTIALLY_PAID",
];

interface RefundDraft {
  order: Order;
  /**
   * Generated once when the drawer opens and LOCKED for the draft's lifetime:
   * it is embedded into the signed 127B ticket AND submitted as
   * `refund.payout_uuid` (becoming `merchant_payouts.id`). Never regenerated
   * per-sign — the signed payload and the submitted field must stay identical.
   */
  payout_uuid: string;
  to_address: string;
  retry_seq: string;
  expiry_unix: string;
  signature: string;
  /**
   * Exact field snapshot our in-page signer embedded into the payload
   * (`null` for a manually-pasted signature — cannot be introspected).
   */
  signed: RefundTicketSigningSnapshot | null;
}

export default function ExceptionsPage() {
  const orders = useOrders(WATCHED);
  const payouts = usePayouts();
  const review = useReviewOrder();
  const cancel = useCancelPayout();
  const adminKey = useAdminKey();

  const [draft, setDraft] = useState<RefundDraft | null>(null);
  const [note, setNote] = useState("");
  const [rowErr, setRowErr] = useState<Record<string, unknown>>({});

  const act = async (order: Order, action: "ACCEPT_UNDERPAID_DISCOUNT" | "ACCEPT_LATE_PAYMENT" | "APPROVE_REFUND", refund?: RefundTicketFields) => {
    try {
      await review.mutateAsync({ orderId: order.id, req: { action, note: note || undefined, refund } });
      setRowErr((e) => ({ ...e, [order.id]: undefined }));
      setDraft(null);
    } catch (e) {
      setRowErr((er) => ({ ...er, [order.id]: e }));
    }
  };

  const openRefund = (order: Order) => {
    setDraft({
      order,
      payout_uuid: crypto.randomUUID(),
      to_address: "",
      retry_seq: "0",
      expiry_unix: String(Math.floor(Date.now() / 1000) + 3600),
      signature: "",
      signed: null,
    });
  };

  const signTicket = async () => {
    if (!draft) return;
    if (!adminKey.hasKey) throw new Error("无内存密钥 — 请先在「离线 Ed25519 审批」页生成/导入管理员密钥");
    const token = draft.order.token_contract;
    if (!token) throw new Error("订单缺少 token_contract — 无法构造退款票据（拒绝以错误地址签名）");
    // Snapshot BEFORE signing: the payload embeds draft.payout_uuid (locked at
    // drawer open) — the same value submitted later as refund.payout_uuid.
    const snapshot: RefundTicketSigningSnapshot = {
      payout_uuid: draft.payout_uuid,
      to_address: draft.to_address,
      retry_seq: Number(draft.retry_seq || "0"),
      expiry_unix: Number(draft.expiry_unix || "0"),
    };
    const payload = buildRefundV1Payload({
      chainId: BigInt(draft.order.chain_id),
      tokenAddress: token,
      toAddress: draft.to_address,
      exactAmount: BigInt(draft.order.received_amount === "0" ? draft.order.expected_amount : draft.order.received_amount),
      payoutUuid: draft.payout_uuid,
      retrySeq: BigInt(draft.retry_seq || "0"),
      expiryUnix: BigInt(draft.expiry_unix || "0"),
    });
    const sig = await adminKey.signHex(payload);
    setDraft({ ...draft, signature: sig, signed: snapshot });
  };

  const submitRefund = () => {
    if (!draft) return;
    let refund: RefundTicketFields;
    try {
      // Fail fast if the submitted fields drifted from the signed 127B payload
      // (incl. payout_uuid) — the signer byte-compares and would 422 anyway.
      refund = assembleRefundTicketFields({
        payout_uuid: draft.payout_uuid,
        to_address: draft.to_address,
        retry_seq: Number(draft.retry_seq || "0"),
        expiry_unix: Number(draft.expiry_unix || "0"),
        admin_signature: draft.signature,
        signed: draft.signed,
      });
    } catch (e) {
      setRowErr((er) => ({ ...er, [draft.order.id]: e }));
      return;
    }
    void act(draft.order, "APPROVE_REFUND", refund);
  };

  return (
    <div className="space-y-6">
      <section>
        <h1 className="mb-1 text-xl font-bold">异常单与退款工作台</h1>
        <p className="text-sm text-neutral-500">
          少付超时 / 迟付人工审核（折让接受、履约接受、RefundTicket 退款）· 禁止自动原路退回
          — 一律经 SUSPENSE_*_LIAB 暂挂 + 管理员离线票据
        </p>
      </section>

      <Card title="待处理与异常订单">
        {orders.error && <ErrorBox error={orders.error} />}
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-neutral-500">
            <tr>
              <th className="py-1">订单</th>
              <th>状态</th>
              <th>应付 / 实收</th>
              <th>过期时间</th>
              <th className="text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {(orders.data?.data ?? []).map((o) => (
              <tr key={o.id} className="border-t border-neutral-800 align-top">
                <td className="py-2">
                  <Link href={`/pay/${o.id}`} className="font-mono text-xs text-sky-400 hover:underline">
                    {shortAddr(o.id, 10, 6)}
                  </Link>
                  <div className="text-[11px] text-neutral-500">{o.merchant_order_ref}</div>
                </td>
                <td>
                  <Badge tone={ORDER_STATUS_TONE[o.status]}>{ORDER_STATUS_LABEL[o.status]}</Badge>
                </td>
                <td className="font-mono text-xs">
                  <AmountText amount={o.expected_amount} decimals={6} symbol={o.asset_symbol} />
                  <br />
                  <span className="text-neutral-500">
                    实收 <AmountText amount={o.received_amount} decimals={6} />
                  </span>
                </td>
                <td className="text-xs text-neutral-500">{fmtDateTime(o.expires_at)}</td>
                <td className="py-2 text-right">
                  <div className="flex flex-wrap justify-end gap-1.5">
                    {o.status === "UNDERPAID_EXPIRED_REVIEW" && (
                      <button
                        type="button"
                        className={btnGhostCls}
                        disabled={review.isPending}
                        onClick={() => void act(o, "ACCEPT_UNDERPAID_DISCOUNT")}
                      >
                        接受折让→PAID
                      </button>
                    )}
                    {o.status === "LATE_PAID_REVIEW" && (
                      <button
                        type="button"
                        className={btnGhostCls}
                        disabled={review.isPending}
                        onClick={() => void act(o, "ACCEPT_LATE_PAYMENT")}
                      >
                        接受迟付→PAID
                      </button>
                    )}
                    {(o.status === "UNDERPAID_EXPIRED_REVIEW" || o.status === "LATE_PAID_REVIEW") && (
                      <button type="button" className={btnDangerCls} onClick={() => openRefund(o)}>
                        批准退款
                      </button>
                    )}
                  </div>
                  {rowErr[o.id] != null && (
                    <div className="mt-1 max-w-xs text-right">
                      <ErrorBox error={rowErr[o.id]} />
                    </div>
                  )}
                </td>
              </tr>
            ))}
            {(orders.data?.data.length ?? 0) === 0 && (
              <tr>
                <td colSpan={5} className="py-6 text-center text-sm text-neutral-600">
                  当前无异常/待审核订单
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </Card>

      {/* RefundTicket drawer */}
      {draft && (
        <Card title={`签发退款票据 — 订单 ${shortAddr(draft.order.id, 10, 6)}`} className="border-amber-800">
          <div className="mb-3 text-xs text-neutral-400">
            全额退款语义：exact_amount = 实收{" "}
            <AmountText
              amount={draft.order.received_amount === "0" ? draft.order.expected_amount : draft.order.received_amount}
              decimals={6}
              symbol={draft.order.asset_symbol}
            />
            （atomic{" "}
            {draft.order.received_amount === "0" ? draft.order.expected_amount : draft.order.received_amount}）。
            退款收款地址为客户地址，不走白名单 — 由 127B RefundTicket 授权。
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="payout_uuid（幂等锚 → merchant_payouts.id；内嵌 127B 票据，粘贴签名必须用此 uuid 签发）">
              <div className="flex items-center gap-2">
                <input className={inputCls} value={draft.payout_uuid} readOnly />
                <CopyButton text={draft.payout_uuid} label="复制" />
                <button
                  type="button"
                  className={btnGhostCls}
                  title="重新生成 payout_uuid（会使已签/已粘贴的签名失效，需重新签署）"
                  onClick={() =>
                    setDraft({ ...draft, payout_uuid: crypto.randomUUID(), signature: "", signed: null })
                  }
                >
                  换新
                </button>
              </div>
            </Field>
            <Field label="客户收款地址 to_address (0x…)">
              <input
                className={inputCls}
                value={draft.to_address}
                onChange={(e) => setDraft({ ...draft, to_address: e.target.value })}
                placeholder="0x…"
              />
            </Field>
            <Field label="retry_seq（换号重试递增，首签为 0）">
              <input className={inputCls} value={draft.retry_seq} onChange={(e) => setDraft({ ...draft, retry_seq: e.target.value })} />
            </Field>
            <Field label="expiry_unix（签名者时钟票据失效点）">
              <input className={inputCls} value={draft.expiry_unix} onChange={(e) => setDraft({ ...draft, expiry_unix: e.target.value })} />
            </Field>
            <Field label="admin_signature（Ed25519 64B hex；可用下方一键签署或去审批页签发后粘贴）">
              <input
                className={inputCls}
                value={draft.signature}
                onChange={(e) =>
                  // A hand-edited signature is no longer provably bound to our
                  // signing snapshot — drop it so submit only format-checks.
                  setDraft({ ...draft, signature: e.target.value, signed: null })
                }
                placeholder="0x…128 hex"
              />
            </Field>
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <button
              type="button"
              className={btnGhostCls}
              disabled={!adminKey.hasKey}
              title={adminKey.hasKey ? "用内存中的管理员密钥签署 127B 票据" : "先到审批页加载密钥"}
              onClick={() => void signTicket().catch((e) => setRowErr((er) => ({ ...er, [draft.order.id]: e })))}
            >
              {adminKey.hasKey ? "用内存密钥签署 127B 票据" : "未加载管理员密钥"}
            </button>
            <Link href="/dashboard/approvals" className={btnGhostCls}>
              去审批页签发 →
            </Link>
            <button
              type="button"
              className={btnCls}
              disabled={review.isPending || !/^(0x)?[0-9a-fA-F]{128}$/.test(draft.signature) || !/^0x[0-9a-fA-F]{40}$/.test(draft.to_address)}
              onClick={submitRefund}
            >
              {review.isPending && <Spinner />}提交 APPROVE_REFUND
            </button>
            <button type="button" className={btnGhostCls} onClick={() => setDraft(null)}>
              取消
            </button>
          </div>
        </Card>
      )}

      {/* Payout lifecycle */}
      <Card title="提现 / 退款单（merchant_payouts）">
        {payouts.error && <ErrorBox error={payouts.error} />}
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-neutral-500">
            <tr>
              <th className="py-1">ID / 类型</th>
              <th>来源科目</th>
              <th>收款地址</th>
              <th>金额</th>
              <th>状态</th>
              <th className="text-right">操作</th>
            </tr>
          </thead>
          <tbody className="text-xs">
            {(payouts.data?.data ?? []).map((p) => (
              <tr key={p.id} className="border-t border-neutral-800">
                <td className="py-2">
                  <span className="font-mono">{shortAddr(p.id, 10, 6)}</span>
                  <div className="text-neutral-500">
                    {p.payout_kind === "WITHDRAWAL" ? "商户提现" : "客户退款"} · retry_seq={p.retry_seq}
                  </div>
                </td>
                <td className="font-mono">{p.source_account_type}</td>
                <td>
                  <AddressText value={p.to_address} copy />
                </td>
                <td className="font-mono">
                  <AmountText amount={p.amount} decimals={6} symbol={p.asset_symbol} />
                </td>
                <td>
                  <Badge tone={p.status === "COMPLETED" ? "ok" : p.status === "FAILED_RETRYABLE" ? "bad" : p.status === "CANCELLED_UNLOCKED" ? "muted" : "info"}>
                    {PAYOUT_STATUS_LABEL[p.status]}
                  </Badge>
                  {p.tx_hash && <div className="mt-1 font-mono text-[10px] text-neutral-600">{shortAddr(p.tx_hash, 10, 6)}</div>}
                </td>
                <td className="py-2 text-right">
                  {(p.status === "LOCKED_QUEUED" || p.status === "FAILED_RETRYABLE") && (
                    <button type="button" className={btnGhostCls} disabled={cancel.isPending} onClick={() => void cancel.mutateAsync(p.id).catch(() => undefined)}>
                      取消并解冻
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
    </div>
  );
}

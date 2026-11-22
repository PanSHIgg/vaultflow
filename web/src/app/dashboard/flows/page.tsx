"use client";

import Link from "next/link";
import { useState } from "react";
import {
  AddressText,
  AmountText,
  Badge,
  type BadgeTone,
  btnDangerCls,
  btnGhostCls,
  btnPrimaryCls,
  Card,
  EmptyState,
  ErrorBox,
  Field,
  inputCls,
  SegmentedTabs,
  Spinner,
} from "@/components/ui";
import { useAdminKey } from "@/lib/admin-key";
import { assetInfo, chainName } from "@/lib/chains";
import { buildRefundV1Payload } from "@/lib/ed25519";
import {
  fmtDateTime,
  ORDER_STATUS_LABEL,
  ORDER_STATUS_TONE,
  PAYOUT_STATUS_LABEL,
  shortAddr,
} from "@/lib/format";
import {
  useCancelPayout,
  useOpenDepositAddresses,
  useOrders,
  usePayouts,
  useReviewOrder,
} from "@/lib/hooks";
import { assembleRefundTicketFields, type RefundTicketSigningSnapshot } from "@/lib/refund-ticket";
import type {
  MerchantPayoutStatus,
  Order,
  PaymentOrderStatus,
  RefundTicketFields,
} from "@/lib/types";

type FlowTab = "orders" | "deposits" | "payouts";

const EXCEPTION_STATUSES: PaymentOrderStatus[] = [
  "UNDERPAID_EXPIRED_REVIEW",
  "LATE_PAID_REVIEW",
  "REFUND_IN_PROGRESS",
  "OVERPAID",
  "SETTLED_REVERSED",
  "PARTIALLY_PAID",
];

const PAYOUT_TONE: Record<MerchantPayoutStatus, BadgeTone> = {
  LOCKED_QUEUED: "info",
  IN_FLIGHT: "warn",
  COMPLETED: "ok",
  FAILED_RETRYABLE: "bad",
  CANCELLED_UNLOCKED: "muted",
};

interface RefundDraft {
  order: Order;
  payout_uuid: string;
  to_address: string;
  retry_seq: string;
  expiry_unix: string;
  signature: string;
  signed: RefundTicketSigningSnapshot | null;
}

export default function DashboardFlowsAndExceptionsPage() {
  const exceptionOrders = useOrders(EXCEPTION_STATUSES);
  const allOrders = useOrders();
  const openDeps = useOpenDepositAddresses();
  const payouts = usePayouts();
  const reviewMut = useReviewOrder();
  const cancelPayoutMut = useCancelPayout();
  const adminKey = useAdminKey();

  const [tab, setTab] = useState<FlowTab>("orders");
  const [draft, setDraft] = useState<RefundDraft | null>(null);
  const [note, setNote] = useState<string>("");
  const [rowErr, setRowErr] = useState<Record<string, unknown>>({});
  const [cancelErr, setCancelErr] = useState<unknown>();

  const executeReview = async (
    order: Order,
    action: "ACCEPT_UNDERPAID_DISCOUNT" | "ACCEPT_LATE_PAYMENT" | "APPROVE_REFUND",
    refund?: RefundTicketFields,
  ) => {
    try {
      await reviewMut.mutateAsync({
        orderId: order.id,
        req: { action, note: note.trim() || undefined, refund },
      });
      setRowErr((prev) => ({ ...prev, [order.id]: undefined }));
      setDraft(null);
    } catch (err) {
      setRowErr((prev) => ({ ...prev, [order.id]: err }));
    }
  };

  const openRefundDrawer = (order: Order) => {
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

  const signRefundInPlace = async () => {
    if (!draft) return;
    if (!adminKey.hasKey) {
      throw new Error("未加载管理员密钥 — 请先前往「离线 Ed25519 审批」标签页生成或导入密钥");
    }
    const token =
      draft.order.token_contract ??
      assetInfo(draft.order.chain_id, draft.order.asset_symbol)?.tokenContract;
    if (!token) {
      throw new Error("订单缺少 token_contract 元数据，拒绝构造可能失配的 127B 票据");
    }
    const snapshot: RefundTicketSigningSnapshot = {
      payout_uuid: draft.payout_uuid,
      to_address: draft.to_address,
      retry_seq: Number(draft.retry_seq || "0"),
      expiry_unix: Number(draft.expiry_unix || "0"),
    };
    const exactAmount = BigInt(
      draft.order.received_amount === "0" ? draft.order.expected_amount : draft.order.received_amount,
    );
    const payload = buildRefundV1Payload({
      chainId: BigInt(draft.order.chain_id),
      tokenAddress: token,
      toAddress: draft.to_address,
      exactAmount,
      payoutUuid: draft.payout_uuid,
      retrySeq: BigInt(draft.retry_seq || "0"),
      expiryUnix: BigInt(draft.expiry_unix || "0"),
    });
    const sig = await adminKey.signHex(payload);
    setDraft({ ...draft, signature: sig, signed: snapshot });
  };

  const submitRefundApproval = () => {
    if (!draft) return;
    try {
      const refund = assembleRefundTicketFields({
        payout_uuid: draft.payout_uuid,
        to_address: draft.to_address,
        retry_seq: Number(draft.retry_seq || "0"),
        expiry_unix: Number(draft.expiry_unix || "0"),
        admin_signature: draft.signature,
        signed: draft.signed,
      });
      void executeReview(draft.order, "APPROVE_REFUND", refund);
    } catch (err) {
      setRowErr((prev) => ({ ...prev, [draft.order.id]: err }));
    }
  };

  return (
    <div className="space-y-6">
      {/* Exception Orders Workbench */}
      <Card
        title={`异常单人工审核工作台 (${exceptionOrders.data?.data.length ?? 0})`}
        subtitle="严禁自动原路退回 tx.from_address（防打入交易所公共热钱包丢失）— 少付超时/迟付一律经 SUSPENSE_REFUNDABLE_LIAB 人工裁决"
      >
        {exceptionOrders.error && <ErrorBox error={exceptionOrders.error} />}
        {(exceptionOrders.data?.data.length ?? 0) === 0 ? (
          <EmptyState title="当前无待审核或挂起异常订单" hint="所有收款单均处于正常待付或完结状态" />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-xs">
              <thead className="border-b border-slate-800 text-slate-400">
                <tr>
                  <th className="py-2.5 pr-3">订单号</th>
                  <th className="py-2.5 pr-3">状态</th>
                  <th className="py-2.5 pr-3">链 · 资产</th>
                  <th className="py-2.5 pr-3">应付 / 实收</th>
                  <th className="py-2.5 pr-3">过期时间</th>
                  <th className="py-2.5 text-right">裁决操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-800/70">
                {(exceptionOrders.data?.data ?? []).map((o) => {
                  const canRefund =
                    o.status === "UNDERPAID_EXPIRED_REVIEW" || o.status === "LATE_PAID_REVIEW";
                  return (
                    <tr key={o.id} className="align-top hover:bg-slate-900/50">
                      <td className="py-3 pr-3">
                        <Link href={`/pay/${o.id}`} className="font-mono text-sky-400 hover:underline">
                          {shortAddr(o.id, 10, 6)}
                        </Link>
                        {o.merchant_order_ref && (
                          <div className="text-[11px] text-slate-500">{o.merchant_order_ref}</div>
                        )}
                        {rowErr[o.id] != null && (
                          <div className="mt-2 max-w-md">
                            <ErrorBox error={rowErr[o.id]} />
                          </div>
                        )}
                      </td>
                      <td className="py-3 pr-3">
                        <Badge tone={ORDER_STATUS_TONE[o.status]}>{ORDER_STATUS_LABEL[o.status]}</Badge>
                      </td>
                      <td className="py-3 pr-3 text-slate-300">
                        {chainName(o.chain_id)} · {o.asset_symbol}
                      </td>
                      <td className="py-3 pr-3">
                        <AmountText amount={o.expected_amount} decimals={6} symbol={o.asset_symbol} />
                        <div className="text-[11px] text-slate-400">
                          实收: <AmountText amount={o.received_amount} decimals={6} symbol={o.asset_symbol} />
                        </div>
                      </td>
                      <td className="py-3 pr-3 text-slate-400">{fmtDateTime(o.expires_at)}</td>
                      <td className="py-3 text-right">
                        <div className="inline-flex flex-wrap justify-end gap-1.5">
                          {o.status === "UNDERPAID_EXPIRED_REVIEW" && (
                            <button
                              type="button"
                              disabled={reviewMut.isPending}
                              onClick={() => void executeReview(o, "ACCEPT_UNDERPAID_DISCOUNT")}
                              className={btnGhostCls}
                            >
                              折让接受 → PAID
                            </button>
                          )}
                          {o.status === "LATE_PAID_REVIEW" && (
                            <button
                              type="button"
                              disabled={reviewMut.isPending}
                              onClick={() => void executeReview(o, "ACCEPT_LATE_PAYMENT")}
                              className={btnGhostCls}
                            >
                              接受迟付履约 → PAID
                            </button>
                          )}
                          {canRefund && (
                            <button
                              type="button"
                              onClick={() => openRefundDrawer(o)}
                              className={btnDangerCls}
                            >
                              签批 RefundTicket 退款
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        {/* RefundTicket inline drawer */}
        {draft && (
          <div className="mt-4 rounded-2xl border border-amber-500/40 bg-slate-950/90 p-4 space-y-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <h3 className="text-sm font-semibold text-amber-200">
                  签发 127B VAULTFLOW_REFUND_V1 退款票据 — 订单 {shortAddr(draft.order.id, 10, 6)}
                </h3>
                <p className="text-xs text-slate-400">
                  退回实收额{" "}
                  <AmountText
                    amount={draft.order.received_amount === "0" ? draft.order.expected_amount : draft.order.received_amount}
                    decimals={6}
                    symbol={draft.order.asset_symbol}
                    className="text-white"
                  />{" "}
                  · 绑定幂等锚 payout_uuid: <span className="font-mono text-slate-300">{draft.payout_uuid}</span>
                </p>
              </div>
              <button type="button" onClick={() => setDraft(null)} className={btnGhostCls}>
                关闭
              </button>
            </div>

            <div className="grid gap-3 sm:grid-cols-3">
              <div className="sm:col-span-2">
                <Field label="经核实的客户收款地址 to_address (bytes20)">
                  <input
                    className={`${inputCls} font-mono text-xs`}
                    placeholder="0x…"
                    value={draft.to_address}
                    onChange={(e) => setDraft({ ...draft, to_address: e.target.value, signed: null })}
                  />
                </Field>
              </div>
              <Field label="票据过期 Unix 秒 (expiry_unix)">
                <input
                  className={`${inputCls} font-mono text-xs`}
                  value={draft.expiry_unix}
                  onChange={(e) => setDraft({ ...draft, expiry_unix: e.target.value, signed: null })}
                />
              </Field>
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="审核备注 (选填)">
                <input
                  className={inputCls}
                  value={note}
                  onChange={(e) => setNote(e.target.value)}
                  placeholder="已与客户工单核对收款地址"
                />
              </Field>
              <Field label="管理员 Ed25519 64B 签名 hex">
                <input
                  className={`${inputCls} font-mono text-xs`}
                  placeholder="点击下方「本页 WebCrypto 签署」自动生成或粘贴离线签名"
                  value={draft.signature}
                  onChange={(e) => setDraft({ ...draft, signature: e.target.value, signed: null })}
                />
              </Field>
            </div>

            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                onClick={() =>
                  void signRefundInPlace().catch((err) =>
                    setRowErr((prev) => ({ ...prev, [draft.order.id]: err })),
                  )
                }
                className={btnGhostCls}
              >
                {adminKey.hasKey ? "使用内存 Ed25519 密钥签署 127B 载荷" : "需先加载 Ed25519 密钥"}
              </button>
              <button
                type="button"
                disabled={!draft.signature || reviewMut.isPending}
                onClick={submitRefundApproval}
                className={btnPrimaryCls}
              >
                {reviewMut.isPending && <Spinner />}
                <span>确认审批并发起链上退款</span>
              </button>
            </div>
          </div>
        )}
      </Card>

      {/* Full Business Flows */}
      <Card
        title="全量业务单据流水"
        actions={
          <SegmentedTabs<FlowTab>
            size="xs"
            value={tab}
            onChange={setTab}
            options={[
              {
                value: "orders",
                label: "收款订单 (payment_orders)",
                badge: <span className="font-mono text-[10px]">{allOrders.data?.data.length ?? 0}</span>,
              },
              {
                value: "deposits",
                label: "常驻充币地址 (open_deposit_addresses)",
                badge: <span className="font-mono text-[10px]">{openDeps.data?.data.length ?? 0}</span>,
              },
              {
                value: "payouts",
                label: "提现与退款单 (merchant_payouts)",
                badge: <span className="font-mono text-[10px]">{payouts.data?.data.length ?? 0}</span>,
              },
            ]}
          />
        }
      >
        {cancelErr != null && (
          <div className="mb-3">
            <ErrorBox error={cancelErr} />
          </div>
        )}

        {tab === "orders" && (
          <div className="overflow-x-auto">
            {(allOrders.data?.data.length ?? 0) === 0 ? (
              <EmptyState title="暂无收款订单" />
            ) : (
              <table className="w-full text-left text-xs">
                <thead className="border-b border-slate-800 text-slate-400">
                  <tr>
                    <th className="py-2.5 pr-3">订单</th>
                    <th className="py-2.5 pr-3">状态</th>
                    <th className="py-2.5 pr-3">链 · 币种</th>
                    <th className="py-2.5 pr-3">应收 → 实收</th>
                    <th className="py-2.5 pr-3">金库地址</th>
                    <th className="py-2.5 pr-3">有效期至</th>
                    <th className="py-2.5 text-right">收银台</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-800/70">
                  {(allOrders.data?.data ?? []).map((o) => (
                    <tr key={o.id} className="hover:bg-slate-900/50">
                      <td className="py-2.5 pr-3">
                        <div className="font-mono text-slate-200">{shortAddr(o.id, 8, 4)}</div>
                        {o.merchant_order_ref && (
                          <div className="text-[11px] text-slate-500">{o.merchant_order_ref}</div>
                        )}
                      </td>
                      <td className="py-2.5 pr-3">
                        <Badge tone={ORDER_STATUS_TONE[o.status]}>{ORDER_STATUS_LABEL[o.status]}</Badge>
                      </td>
                      <td className="py-2.5 pr-3 text-slate-300">
                        {chainName(o.chain_id)} · {o.asset_symbol}
                      </td>
                      <td className="py-2.5 pr-3">
                        <AmountText amount={o.expected_amount} decimals={6} /> →{" "}
                        <AmountText amount={o.received_amount} decimals={6} symbol={o.asset_symbol} />
                      </td>
                      <td className="py-2.5 pr-3">
                        <AddressText value={o.deposit_address} copy />
                      </td>
                      <td className="py-2.5 pr-3 text-slate-400">{fmtDateTime(o.expires_at)}</td>
                      <td className="py-2.5 text-right">
                        <Link href={`/pay/${o.id}`} className="text-sky-400 hover:underline">
                          收银台 →
                        </Link>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        )}

        {tab === "deposits" && (
          <div className="overflow-x-auto">
            {(openDeps.data?.data.length ?? 0) === 0 ? (
              <EmptyState title="暂无常驻开放充值地址" />
            ) : (
              <table className="w-full text-left text-xs">
                <thead className="border-b border-slate-800 text-slate-400">
                  <tr>
                    <th className="py-2.5 pr-3">地址 ID</th>
                    <th className="py-2.5 pr-3">用户标识 (customer_ref)</th>
                    <th className="py-2.5 pr-3">链 · 币种</th>
                    <th className="py-2.5 pr-3">CREATE2 金库地址</th>
                    <th className="py-2.5 pr-3">最小充币额</th>
                    <th className="py-2.5 pr-3">累计已入账</th>
                    <th className="py-2.5 text-right">创建时间</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-800/70">
                  {(openDeps.data?.data ?? []).map((d) => (
                    <tr key={d.id} className="hover:bg-slate-900/50">
                      <td className="py-2.5 pr-3 font-mono text-slate-300">{shortAddr(d.id, 8, 4)}</td>
                      <td className="py-2.5 pr-3">
                        {d.customer_ref ? <Badge tone="info">{d.customer_ref}</Badge> : <Badge tone="muted">商户级</Badge>}
                      </td>
                      <td className="py-2.5 pr-3 text-slate-300">
                        {chainName(d.chain_id)} · {d.asset_symbol}
                      </td>
                      <td className="py-2.5 pr-3">
                        <AddressText value={d.deposit_address} copy />
                      </td>
                      <td className="py-2.5 pr-3 text-slate-400">
                        <AmountText amount={d.min_deposit} decimals={6} symbol={d.asset_symbol} />
                      </td>
                      <td className="py-2.5 pr-3 text-emerald-300 font-medium">
                        <AmountText amount={d.received_total} decimals={6} symbol={d.asset_symbol} />
                      </td>
                      <td className="py-2.5 text-right text-slate-400">{fmtDateTime(d.created_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        )}

        {tab === "payouts" && (
          <div className="overflow-x-auto">
            {(payouts.data?.data.length ?? 0) === 0 ? (
              <EmptyState title="暂无提现与退款单" />
            ) : (
              <table className="w-full text-left text-xs">
                <thead className="border-b border-slate-800 text-slate-400">
                  <tr>
                    <th className="py-2.5 pr-3">单号</th>
                    <th className="py-2.5 pr-3">类型 / 来源科目</th>
                    <th className="py-2.5 pr-3">状态</th>
                    <th className="py-2.5 pr-3">金额</th>
                    <th className="py-2.5 pr-3">收款方</th>
                    <th className="py-2.5 pr-3">TxHash</th>
                    <th className="py-2.5 text-right">操作</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-800/70">
                  {(payouts.data?.data ?? []).map((p) => {
                    const canCancel = p.status === "LOCKED_QUEUED" || p.status === "FAILED_RETRYABLE";
                    return (
                      <tr key={p.id} className="hover:bg-slate-900/50">
                        <td className="py-2.5 pr-3 font-mono text-slate-200">{shortAddr(p.id, 8, 4)}</td>
                        <td className="py-2.5 pr-3">
                          <Badge tone={p.payout_kind === "WITHDRAWAL" ? "info" : "warn"}>
                            {p.payout_kind === "WITHDRAWAL" ? "商户提现" : "客户退款"}
                          </Badge>
                          <div className="mt-0.5 font-mono text-[10px] text-slate-500">{p.source_account_type}</div>
                        </td>
                        <td className="py-2.5 pr-3">
                          <Badge tone={PAYOUT_TONE[p.status]}>{PAYOUT_STATUS_LABEL[p.status]}</Badge>
                        </td>
                        <td className="py-2.5 pr-3">
                          <AmountText amount={p.amount} decimals={6} symbol={p.asset_symbol} />
                          <div className="text-[10px] text-slate-500">{chainName(p.chain_id)}</div>
                        </td>
                        <td className="py-2.5 pr-3">
                          <AddressText value={p.to_address} copy />
                        </td>
                        <td className="py-2.5 pr-3">
                          {p.tx_hash ? <AddressText value={p.tx_hash} copy /> : <span className="text-slate-600">—</span>}
                        </td>
                        <td className="py-2.5 text-right">
                          {canCancel && (
                            <button
                              type="button"
                              disabled={cancelPayoutMut.isPending}
                              onClick={() =>
                                void cancelPayoutMut
                                  .mutateAsync(p.id)
                                  .then(() => setCancelErr(undefined))
                                  .catch(setCancelErr)
                              }
                              className={btnDangerCls}
                            >
                              取消解冻
                            </button>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>
        )}
      </Card>
    </div>
  );
}

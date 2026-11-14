// W6-2 — Hosted Checkout. Order status + chain/asset badge + exact amount +
// 1:1 CREATE2 deposit address QR/copy + countdown + SSE confirmation progress
// (auto-degrades to 3s polling) + dual payment rails:
//   ① ERC-20 transfer via wallet
//   ② USDC EIP-3009 gasless ReceiveWithAuthorization (valid-until bound)

"use client";

import QRCode from "react-qr-code";
import { useMemo, useState } from "react";
import { erc20Abi, type Hex } from "viem";
import {
  useAccount,
  useConnect,
  useDisconnect,
  useSignTypedData,
  useWriteContract,
} from "wagmi";
import {
  AddressText,
  AmountText,
  Badge,
  btnCls,
  btnGhostCls,
  Card,
  CopyButton,
  ErrorBox,
  ProgressBar,
  Spinner,
} from "@/components/ui";
import { parseAmount } from "@/lib/amount";
import { assetInfo, chainName } from "@/lib/chains";
import {
  buildReceiveWithAuthorization,
  signatureToSubmission,
} from "@/lib/eip3009";
import { submitEip3009, USE_MOCK } from "@/lib/api";
import { fmtCountdown, ORDER_STATUS_LABEL, ORDER_STATUS_TONE } from "@/lib/format";
import { useCountdownSeconds, usePublicPayment } from "@/lib/hooks";
import { CLOSED_ORDER_STATUSES } from "@/lib/types";

/** Deterministic demo signature for USE_MOCK when the mock connector cannot sign. */
function demoSignature(nonce: string): Hex {
  let h = 0x9e3779b9;
  const mix = (s: string) => {
    for (let i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i);
      h = Math.imul(h, 0x01000193) >>> 0;
    }
    return h.toString(16).padStart(8, "0");
  };
  const r = (mix(nonce) + mix("r" + nonce) + mix("rr" + nonce) + mix("rrr" + nonce)).slice(0, 64);
  const s = (mix("s" + nonce) + mix("ss" + nonce) + mix("sss" + nonce) + mix("ssss" + nonce)).slice(0, 64);
  return `0x${r}${s}1b` as Hex;
}

export function Cashier({ orderId }: { orderId: string }) {
  const live = usePublicPayment(orderId);
  const { payment, transport, deposits, confirmation, error } = live;
  const countdown = useCountdownSeconds(payment?.expires_at);

  const account = useAccount();
  const { connect, connectors } = useConnect();
  const { disconnect } = useDisconnect();
  const { writeContractAsync } = useWriteContract();
  const { signTypedDataAsync } = useSignTypedData();

  const [payNote, setPayNote] = useState<string>();
  const [payErr, setPayErr] = useState<unknown>();
  const [busy, setBusy] = useState<"transfer" | "eip3009" | null>(null);

  const asset = payment ? assetInfo(payment.chain_id, payment.asset_symbol) : undefined;
  const expired = payment ? countdown <= 0 : false;
  const closed = payment ? CLOSED_ORDER_STATUSES.includes(payment.status) : false;
  const payable = payment
    ? !expired && (payment.status === "AWAITING_PAYMENT" || payment.status === "PARTIALLY_PAID")
    : false;

  const eip3009Reason = useMemo(() => {
    if (!payment) return "加载中";
    if (!payment.eip3009?.supported) return "该币种不支持 EIP-3009 免 Gas 支付";
    if (closed) return `订单已${ORDER_STATUS_LABEL[payment.status]}`;
    if (expired) return "订单已过期，授权签名 validBefore 已失效";
    return null;
  }, [payment, closed, expired]);

  const progressPct = useMemo(() => {
    if (!payment) return 0n;
    const exp = parseAmount(payment.expected_amount);
    const rec = parseAmount(payment.received_amount);
    return exp === 0n ? 0n : (rec * 100n) / exp;
  }, [payment]);

  const doTransfer = async () => {
    if (!payment) return;
    setPayErr(undefined);
    setPayNote(undefined);
    setBusy("transfer");
    try {
      const hash = await writeContractAsync({
        address: payment.token_contract,
        abi: erc20Abi,
        functionName: "transfer",
        args: [payment.deposit_address, parseAmount(payment.expected_amount)],
        chainId: payment.chain_id,
      });
      setPayNote(`ERC-20 transfer 已广播：${hash ?? "（模拟连接器未返回哈希）"}`);
    } catch (e) {
      if (USE_MOCK) {
        setPayNote("演示模式：已模拟广播 ERC-20 transfer（链上确认进度由 Mock 引擎推进）");
      } else {
        setPayErr(e);
      }
    } finally {
      setBusy(null);
    }
  };

  const doEip3009 = async () => {
    if (!payment || !asset || !account.address) return;
    setPayErr(undefined);
    setPayNote(undefined);
    setBusy("eip3009");
    try {
      const td = buildReceiveWithAuthorization(payment, asset, account.address);
      let sig: Hex | undefined;
      try {
        sig = await signTypedDataAsync({
          domain: td.domain,
          types: td.types,
          primaryType: "ReceiveWithAuthorization",
          message: td.message,
        } as unknown as Parameters<typeof signTypedDataAsync>[0]);
      } catch (e) {
        if (!USE_MOCK) throw e;
      }
      if (!sig) {
        if (!USE_MOCK) throw new Error("钱包未返回签名");
        sig = demoSignature(td.message.nonce);
      }
      const sub = signatureToSubmission(td.message, sig);
      const res = await submitEip3009(orderId, sub);
      setPayNote(
        `EIP-3009 授权已被网关受理 → EIP3009_RELAY intent ${res.intent_id.slice(0, 8)}… (${res.intent_status})，等待 Router 链上入账`,
      );
    } catch (e) {
      setPayErr(e);
    } finally {
      setBusy(null);
    }
  };

  return (
    <main className="mx-auto max-w-2xl px-4 py-10">
      <header className="mb-6 flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-bold">VaultFlow 收银台</h1>
          <p className="mt-1 text-sm text-neutral-400">
            {payment?.merchant_name ?? "…"} {payment?.merchant_order_ref ? `· ${payment.merchant_order_ref}` : ""}
          </p>
        </div>
        <div className="flex flex-col items-end gap-1">
          {payment && <Badge tone={ORDER_STATUS_TONE[payment.status]}>{ORDER_STATUS_LABEL[payment.status]}</Badge>}
          <span className="text-[11px] text-neutral-500">
            {transport === "sse" && "● SSE 实时推送中"}
            {transport === "polling" && "● 已降级 3s 轮询"}
            {transport === "connecting" && "○ 连接中…"}
          </span>
        </div>
      </header>

      {error && !payment && <ErrorBox error={error} />}

      {payment && (
        <div className="space-y-4">
          {/* 应付金额 + 链币标识 */}
          <Card>
            <div className="flex flex-wrap items-end justify-between gap-3">
              <div>
                <div className="text-xs text-neutral-500">应付金额（原子单位 {payment.expected_amount}）</div>
                <AmountText
                  amount={payment.expected_amount}
                  decimals={payment.decimals}
                  symbol={payment.asset_symbol}
                  className="mt-1 block text-3xl font-bold"
                />
              </div>
              <div className="flex gap-2">
                <Badge tone="info">{chainName(payment.chain_id)}</Badge>
                <Badge tone="muted">{payment.asset_symbol}</Badge>
              </div>
            </div>
            <div className="mt-3">
              <div className="mb-1 flex justify-between text-xs text-neutral-500">
                <span>
                  已到账 <AmountText amount={payment.received_amount} decimals={payment.decimals} />
                </span>
                <span>{progressPct.toString()}%</span>
              </div>
              <ProgressBar value={progressPct} max={100n} tone={progressPct >= 100n ? "ok" : "info"} />
            </div>
          </Card>

          {/* 专属 DepositAddr + 倒计时 */}
          <Card title="专属收款地址（CREATE2 1:1 金库，切勿跨单复用）">
            <div className="flex flex-col items-center gap-4 sm:flex-row">
              <div className="rounded-lg bg-white p-3">
                <QRCode value={payment.deposit_address} size={140} />
              </div>
              <div className="min-w-0 flex-1">
                <div className="break-all font-mono text-sm text-sky-300">{payment.deposit_address}</div>
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <CopyButton text={payment.deposit_address} label="复制地址" />
                  <CopyButton
                    text={`ethereum:${payment.token_contract}@${payment.chain_id}/transfer?address=${payment.deposit_address}&uint256=${payment.expected_amount}`}
                    label="复制支付 URI"
                  />
                  {payment.deposit_address_deployed === false && (
                    <Badge tone="muted">金库代理将在首次归集时懒部署</Badge>
                  )}
                </div>
                <div className="mt-3 text-sm">
                  {expired ? (
                    <Badge tone="bad">订单已过期</Badge>
                  ) : (
                    <>
                      剩余有效时间{" "}
                      <span className="font-mono text-lg font-semibold text-amber-300">{fmtCountdown(countdown)}</span>
                    </>
                  )}
                </div>
              </div>
            </div>
          </Card>

          {/* 确认进度 */}
          {(deposits.length > 0 || confirmation || payment.status === "PARTIALLY_PAID") && (
            <Card title="链上确认进度">
              {deposits.map((d) => (
                <div key={d.deposit_id} className="mb-2 rounded-lg border border-neutral-800 p-3 text-sm">
                  <div className="flex items-center justify-between">
                    <span className="font-mono text-xs text-neutral-400">{d.tx_hash}</span>
                    <Badge tone={d.status === "FINALIZED" ? "ok" : "info"}>{d.status}</Badge>
                  </div>
                  <div className="mt-1 text-xs text-neutral-500">
                    区块 #{d.block_number} · <AmountText amount={d.amount} decimals={payment.decimals} symbol={d.asset_symbol} />
                  </div>
                </div>
              ))}
              {confirmation && (
                <div className="mt-2">
                  <div className="mb-1 flex justify-between text-xs text-neutral-500">
                    <span>
                      确认数 {confirmation.current_confirmations} / {confirmation.required_confirmations}
                    </span>
                    <span>safe_head #{confirmation.safe_head_height}</span>
                  </div>
                  <ProgressBar
                    value={confirmation.current_confirmations}
                    max={confirmation.required_confirmations}
                    tone={confirmation.current_confirmations >= confirmation.required_confirmations ? "ok" : "info"}
                  />
                </div>
              )}
            </Card>
          )}

          {/* 支付双通道 */}
          {payable && (
            <Card title="钱包支付">
              {!account.address ? (
                <div className="flex flex-wrap gap-2">
                  {connectors.map((c) => (
                    <button key={c.uid} type="button" className={btnCls} onClick={() => connect({ connector: c })}>
                      连接 {c.name}
                    </button>
                  ))}
                </div>
              ) : (
                <div className="space-y-3">
                  <div className="flex items-center justify-between text-sm">
                    <span className="text-neutral-400">
                      已连接 <AddressText value={account.address} />
                    </span>
                    <button type="button" className="text-xs text-neutral-500 hover:text-neutral-300" onClick={() => disconnect()}>
                      断开
                    </button>
                  </div>
                  <div className="grid gap-2 sm:grid-cols-2">
                    <button type="button" className={btnCls} disabled={busy !== null} onClick={doTransfer}>
                      {busy === "transfer" && <Spinner />}
                      ① ERC-20 transfer 支付
                    </button>
                    <div>
                      <button
                        type="button"
                        className={btnCls + " w-full"}
                        disabled={busy !== null || eip3009Reason !== null}
                        onClick={doEip3009}
                        title={eip3009Reason ?? "签署 ReceiveWithAuthorization，由网关代付 Gas 中继上链"}
                      >
                        {busy === "eip3009" && <Spinner />}② USDC 免 Gas 签名支付
                      </button>
                      {eip3009Reason && <div className="mt-1 text-[11px] text-neutral-500">{eip3009Reason}</div>}
                    </div>
                  </div>
                </div>
              )}
              {payNote && <div className="mt-3 rounded-lg border border-emerald-800 bg-emerald-950/40 px-3 py-2 text-xs text-emerald-300">{payNote}</div>}
              {payErr != null && <div className="mt-3"><ErrorBox error={payErr} /></div>}
              <p className="mt-3 text-[11px] leading-relaxed text-neutral-500">
                EIP-3009 严格绑定：to = EIP3009PayRouter、value = 应付额、validBefore = 订单 expires_at、
                nonce = authNonce = keccak256(abi.encode(chainId, salt))。链上 Router 自算 vault 收款地址，
                双重防劫持。订单过期后签名自动失效，只能走普通转账。
              </p>
            </Card>
          )}

          {!payable && payment.status === "AWAITING_PAYMENT" && expired && (
            <Card>
              <p className="text-sm text-neutral-400">订单已过期，请勿再向该地址转账；若已迟付将由商户人工审核处理。</p>
            </Card>
          )}

          {payment.status === "PAID" && (
            <Card className="border-emerald-800">
              <p className="text-lg font-semibold text-emerald-300">✓ 支付完成</p>
              <p className="mt-1 text-sm text-neutral-400">订单已全额清分结算，可以安全关闭本页。</p>
            </Card>
          )}
          {payment.status === "OVERPAID" && (
            <Card className="border-amber-800">
              <p className="text-lg font-semibold text-amber-300">✓ 支付完成（超额）</p>
              <p className="mt-1 text-sm text-neutral-400">超额部分已挂入 SUSPENSE_OVERPAY_LIAB，商户核实后将通过退款流程退回。</p>
            </Card>
          )}
          {(payment.status === "UNDERPAID_EXPIRED_REVIEW" || payment.status === "LATE_PAID_REVIEW") && (
            <Card className="border-red-800">
              <p className="text-lg font-semibold text-red-300">⚠ {ORDER_STATUS_LABEL[payment.status]}</p>
              <p className="mt-1 text-sm text-neutral-400">款项已转入可退暂挂科目，等待商户人工审核（接受折让/履约或退款）。</p>
            </Card>
          )}
        </div>
      )}
    </main>
  );
}

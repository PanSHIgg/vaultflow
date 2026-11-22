"use client";

import Link from "next/link";
import QRCode from "react-qr-code";
import { useMemo, useState } from "react";
import { erc20Abi } from "viem";
import {
  useAccount,
  useConnect,
  useDisconnect,
  useSignTypedData,
  useSwitchChain,
  useWriteContract,
} from "wagmi";
import {
  AddressText,
  AmountText,
  Badge,
  btnGhostCls,
  btnPrimaryCls,
  btnSecondaryCls,
  CopyButton,
  ErrorBox,
  ProgressBar,
  Spinner,
} from "@/components/ui";
import { parseAmount } from "@/lib/amount";
import { submitEip3009 } from "@/lib/api";
import { assetInfo, chainName } from "@/lib/chains";
import { buildReceiveWithAuthorization, signatureToSubmission } from "@/lib/eip3009";
import { fmtCountdown, ORDER_STATUS_LABEL, ORDER_STATUS_TONE } from "@/lib/format";
import { useCountdownSeconds, usePublicPayment } from "@/lib/hooks";
import type { EvmAddress, Hex } from "@/lib/types";

export function CashierPanel({
  orderId,
  compact = false,
}: {
  orderId: string;
  compact?: boolean;
}) {
  const { payment, transport, deposits, confirmation, error } = usePublicPayment(orderId);
  const countdown = useCountdownSeconds(payment?.expires_at);

  const account = useAccount();
  const { connectors, connectAsync, isPending: isConnecting } = useConnect();
  const { disconnect } = useDisconnect();
  const { switchChainAsync } = useSwitchChain();
  const { signTypedDataAsync, isPending: isSigning } = useSignTypedData();
  const { writeContractAsync, isPending: isSendingTx } = useWriteContract();

  const [actionErr, setActionErr] = useState<unknown>();
  const [relayResult, setRelayResult] = useState<{ intentId: string; status: string } | null>(null);
  const [erc20TxHash, setErc20TxHash] = useState<Hex | null>(null);
  const [showCliHint, setShowCliHint] = useState(false);

  const expired = payment ? countdown <= 0 : false;
  const payable = payment
    ? !expired && (payment.status === "AWAITING_PAYMENT" || payment.status === "PARTIALLY_PAID")
    : false;

  const progressPct = useMemo(() => {
    if (!payment) return 0n;
    const exp = parseAmount(payment.expected_amount);
    const rec = parseAmount(payment.received_amount);
    return exp === 0n ? 0n : (rec * 100n) / exp;
  }, [payment]);

  const remaining = useMemo(() => {
    if (!payment) return 0n;
    const left = parseAmount(payment.expected_amount) - parseAmount(payment.received_amount);
    return left > 0n ? left : 0n;
  }, [payment]);

  const asset = payment ? assetInfo(payment.chain_id, payment.asset_symbol) : undefined;
  const eip3009Eligible = Boolean(payment?.eip3009?.supported && asset?.supportsEip3009);

  const ensureWalletOnChain = async (targetChainId: number): Promise<EvmAddress> => {
    let addr = account.address as EvmAddress | undefined;
    if (!addr) {
      const connector = connectors[0];
      if (!connector) throw new Error("未检测到可用钱包连接器（MetaMask / Mock Connector）");
      const res = await connectAsync({ connector, chainId: targetChainId });
      addr = res.accounts[0] as EvmAddress | undefined;
    } else if (account.chainId !== targetChainId && switchChainAsync) {
      await switchChainAsync({ chainId: targetChainId });
    }
    if (!addr) throw new Error("未能获取钱包账户地址");
    return addr;
  };

  const handleEip3009Gasless = async () => {
    if (!payment || !asset) return;
    setActionErr(undefined);
    try {
      const payer = await ensureWalletOnChain(payment.chain_id);
      const built = buildReceiveWithAuthorization(payment, asset, payer);
      const sig = await signTypedDataAsync({
        domain: built.domain,
        types: built.types,
        primaryType: built.primaryType,
        message: built.message,
      });
      const sub = signatureToSubmission(built.message, sig);
      const res = await submitEip3009(payment.order_id, sub);
      setRelayResult({ intentId: res.intent_id, status: res.intent_status });
    } catch (e) {
      setActionErr(e);
    }
  };

  const handleDirectErc20 = async () => {
    if (!payment || remaining <= 0n) return;
    setActionErr(undefined);
    try {
      await ensureWalletOnChain(payment.chain_id);
      const hash = await writeContractAsync({
        address: payment.token_contract,
        abi: erc20Abi,
        functionName: "transfer",
        args: [payment.deposit_address, remaining],
        chainId: payment.chain_id,
      });
      setErc20TxHash(hash);
    } catch (e) {
      setActionErr(e);
    }
  };

  if (error && !payment) {
    return <ErrorBox error={error} />;
  }

  if (!payment) {
    return (
      <div className="flex items-center justify-center gap-2 rounded-xl border border-slate-800 bg-slate-950/60 py-10 text-xs text-slate-400">
        <Spinner />
        <span>正在同步收款单与实时确认通道…</span>
      </div>
    );
  }

  const rpcPort = payment.chain_id === 31338 ? "8546" : "8545";
  const castCmd = `cast send ${payment.token_contract} "transfer(address,uint256)" ${payment.deposit_address} ${remaining > 0n ? remaining.toString() : payment.expected_amount} --rpc-url http://127.0.0.1:${rpcPort} --unlocked --from 0xa0Ee7A142d267C1f36714E4a8F75612F20a79720`;

  return (
    <div className="space-y-4">
      <div className="rounded-xl border border-slate-800/90 bg-slate-950/70 p-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <div className="flex flex-wrap items-center gap-2">
              <Badge tone={ORDER_STATUS_TONE[payment.status]}>{ORDER_STATUS_LABEL[payment.status]}</Badge>
              <Badge tone="info">{chainName(payment.chain_id)}</Badge>
              <Badge tone="muted">{payment.asset_symbol}</Badge>
              {payment.merchant_order_ref && (
                <span className="font-mono text-xs text-slate-400">{payment.merchant_order_ref}</span>
              )}
            </div>
            <div className="mt-2.5 flex items-baseline gap-2">
              <AmountText
                amount={payment.expected_amount}
                decimals={payment.decimals}
                symbol={payment.asset_symbol}
                className={compact ? "text-2xl font-bold text-white" : "text-3xl font-bold text-white"}
              />
              <span className="font-mono text-[11px] text-slate-500">
                ({payment.expected_amount} atomic)
              </span>
            </div>
          </div>

          <div className="flex flex-col items-end gap-1.5 text-right">
            <span className="inline-flex items-center gap-1.5 text-[11px] text-slate-400">
              <span
                className={`h-2 w-2 rounded-full ${
                  transport === "sse"
                    ? "bg-emerald-400"
                    : transport === "polling"
                      ? "bg-amber-400"
                      : "bg-slate-500"
                }`}
              />
              {transport === "sse" && "SSE 实时流"}
              {transport === "polling" && "3s 短轮询降级"}
              {transport === "connecting" && "连接推送流…"}
            </span>
            {!expired ? (
              <span className="font-mono text-xs text-amber-300 tabular-nums">
                有效倒计时 {fmtCountdown(countdown)}
              </span>
            ) : (
              <Badge tone="bad">已逾有效期</Badge>
            )}
            {compact && (
              <Link href={`/pay/${payment.order_id}`} className="text-[11px] text-sky-400 hover:underline">
                打开独立收银台 →
              </Link>
            )}
          </div>
        </div>

        <div className="mt-3">
          <div className="mb-1.5 flex justify-between text-xs text-slate-400">
            <span>
              已确认入账{" "}
              <AmountText
                amount={payment.received_amount}
                decimals={payment.decimals}
                symbol={payment.asset_symbol}
                className="text-slate-200"
              />
            </span>
            <span className="font-mono tabular-nums">{progressPct.toString()}%</span>
          </div>
          <ProgressBar value={progressPct} max={100n} tone={progressPct >= 100n ? "ok" : "info"} />
        </div>
      </div>

      <div className="rounded-xl border border-slate-800/90 bg-slate-950/70 p-4">
        <div className="flex flex-col items-center gap-4 sm:flex-row sm:items-start">
          <div className="shrink-0 rounded-xl bg-white p-2.5 shadow-sm">
            <QRCode value={payment.deposit_address} size={compact ? 124 : 148} />
          </div>
          <div className="min-w-0 flex-1 space-y-2.5">
            <div>
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-xs font-medium text-slate-400">
                  1:1 订单专属 CREATE2 金库地址
                </span>
                {payment.deposit_address_deployed === false && (
                  <Badge tone="muted">EIP-1167 归集时懒部署</Badge>
                )}
              </div>
              <div className="mt-1 break-all font-mono text-xs text-emerald-300 sm:text-sm">
                {payment.deposit_address}
              </div>
            </div>

            <div className="flex flex-wrap items-center gap-2">
              <CopyButton text={payment.deposit_address} label="复制收款地址" />
              <CopyButton
                text={`ethereum:${payment.token_contract}@${payment.chain_id}/transfer?address=${payment.deposit_address}&uint256=${payment.expected_amount}`}
                label="复制 EIP-681 URI"
              />
              <button
                type="button"
                onClick={() => setShowCliHint((v) => !v)}
                className={btnGhostCls}
              >
                {showCliHint ? "收起演示命令" : "Anvil 演示打款命令"}
              </button>
            </div>

            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-slate-400">
              <span>
                代币合约: <AddressText value={payment.token_contract} copy />
              </span>
              {payment.safe_head_height != null && (
                <span className="font-mono">safe_head #{payment.safe_head_height}</span>
              )}
            </div>
          </div>
        </div>

        {showCliHint && (
          <div className="mt-3 rounded-lg border border-slate-800 bg-slate-900/90 p-2.5">
            <div className="mb-1 flex items-center justify-between text-[11px] text-slate-400">
              <span>本地 Anvil 测试账户直接打款命令（越过 safe_head 自动入账）</span>
              <CopyButton text={castCmd} label="复制命令" />
            </div>
            <pre className="overflow-x-auto font-mono text-[11px] text-slate-300">{castCmd}</pre>
          </div>
        )}
      </div>

      {(payable || eip3009Eligible) && (
        <div className="rounded-xl border border-slate-800/90 bg-slate-950/70 p-4 space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs font-semibold text-slate-200">钱包快捷交互演示</span>
            {account.isConnected ? (
              <div className="flex items-center gap-2 text-xs">
                <Badge tone="ok">已连接 { account.address ? `${account.address.slice(0, 6)}…${account.address.slice(-4)}` : "" }</Badge>
                <button type="button" onClick={() => disconnect()} className="text-[11px] text-slate-400 hover:text-white">
                  断开
                </button>
              </div>
            ) : (
              <span className="text-[11px] text-slate-500">点击下方按钮自动唤起钱包或演示签名器</span>
            )}
          </div>

          <div className="flex flex-wrap gap-2.5">
            {payable && (
              <button
                type="button"
                disabled={isConnecting || isSendingTx || remaining <= 0n}
                onClick={() => void handleDirectErc20()}
                className={btnGhostCls}
              >
                {(isConnecting || isSendingTx) && <Spinner />}
                <span>
                  钱包直转 ERC-20 (
                  <AmountText amount={remaining} decimals={payment.decimals} symbol={payment.asset_symbol} />)
                </span>
              </button>
            )}

            {payment.asset_symbol === "USDC" && (
              <button
                type="button"
                disabled={!payable || !eip3009Eligible || isConnecting || isSigning}
                onClick={() => void handleEip3009Gasless()}
                className={btnSecondaryCls}
              >
                {(isConnecting || isSigning) && <Spinner />}
                <span>USDC 免 Gas 签名支付 (EIP-3009)</span>
              </button>
            )}
          </div>

          {payment.asset_symbol === "USDC" && expired && (
            <p className="text-[11px] text-amber-300/90">
              订单已过有效期（validBefore == expires_at），EIP-3009 免 Gas 路由已自动禁用；迟到转账将挂入 LATE_PAID_REVIEW 待审。
            </p>
          )}

          {relayResult && (
            <div className="rounded-lg border border-sky-500/30 bg-sky-950/30 px-3 py-2 text-xs text-sky-200">
              EIP-3009 离线授权已提交网关中继队列：intent{" "}
              <span className="font-mono">{relayResult.intentId.slice(0, 10)}…</span> ({relayResult.status})
            </div>
          )}

          {erc20TxHash && (
            <div className="rounded-lg border border-emerald-500/30 bg-emerald-950/30 px-3 py-2 text-xs text-emerald-200">
              链上转账已广播：<AddressText value={erc20TxHash} copy />
            </div>
          )}

          {actionErr != null && <ErrorBox error={actionErr} />}
        </div>
      )}

      {(deposits.length > 0 || confirmation) && (
        <div className="rounded-xl border border-slate-800/90 bg-slate-950/70 p-4 space-y-2.5">
          <div className="text-xs font-semibold text-slate-200">链上捕获与安全高度确认进度</div>
          {deposits.map((d) => (
            <div
              key={d.deposit_id}
              className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-slate-800 bg-slate-900/70 px-3 py-2 text-xs"
            >
              <div className="flex items-center gap-2">
                <Badge tone={d.status === "FINALIZED" ? "ok" : "info"}>{d.status}</Badge>
                <AddressText value={d.tx_hash} copy />
              </div>
              <div className="text-slate-400">
                区块 #{d.block_number} ·{" "}
                <AmountText amount={d.amount} decimals={payment.decimals} symbol={d.asset_symbol} className="text-slate-200" />
              </div>
            </div>
          ))}
          {confirmation && (
            <div className="space-y-1.5 pt-1">
              <div className="flex justify-between text-xs text-slate-400">
                <span>
                  确认进度 {confirmation.current_confirmations} / {confirmation.required_confirmations}
                </span>
                <span className="font-mono">safe_head #{confirmation.safe_head_height}</span>
              </div>
              <ProgressBar
                value={confirmation.current_confirmations}
                max={confirmation.required_confirmations}
                tone={confirmation.current_confirmations >= confirmation.required_confirmations ? "ok" : "info"}
              />
            </div>
          )}
        </div>
      )}

      {payment.status === "PAID" && (
        <div className="rounded-xl border border-emerald-500/40 bg-emerald-950/30 px-4 py-3 text-xs text-emerald-200">
          <div className="font-semibold text-emerald-300">订单已足额确认结算 (PAID)</div>
          <p className="mt-0.5 text-emerald-200/80">
            资金已越过链上安全确认高度，扣减手续费后已自动计入商户可提现余额 (MERCHANT_AVAILABLE)。
          </p>
        </div>
      )}

      {payment.status === "OVERPAID" && (
        <div className="rounded-xl border border-amber-500/40 bg-amber-950/30 px-4 py-3 text-xs text-amber-200">
          <div className="font-semibold text-amber-300">超额支付完结 (OVERPAID)</div>
          <p className="mt-0.5 text-amber-200/80">
            应付金额已入账，超付差额已挂入 SUSPENSE_OVERPAY_LIAB，可在管理后台签发 127B RefundTicket 退回。
          </p>
        </div>
      )}
    </div>
  );
}

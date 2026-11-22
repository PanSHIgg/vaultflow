"use client";

import Link from "next/link";
import { useMemo, useState } from "react";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import * as api from "@/lib/api";
import { formatTokenAmount, parseUnits } from "@/lib/amount";
import { btnCls, Card, ErrorBox, Field, inputCls, Spinner } from "@/components/ui";
import { Shell } from "@/components/shell";

export default function AssetsPage() {
  return (
    <Shell>
      <AssetsContent />
      <WithdrawCard />
      <WithdrawalHistory />
    </Shell>
  );
}

function AssetsContent() {
  const balances = useQuery({ queryKey: ["balances"], queryFn: api.listBalances });

  const rows = balances.data ?? [];

  return (
    <>
      <h2 className="py-6 text-xl font-semibold">我的资产</h2>
      <Card>
        {balances.isLoading ? (
          <div className="flex justify-center py-8"><Spinner className="text-[#FCD535]" /></div>
        ) : balances.isError ? (
          <p className="py-8 text-center text-sm text-red-300">加载资产失败</p>
        ) : rows.length === 0 ? (
          <p className="py-8 text-center text-sm text-[#848E9C]">暂无资产</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead>
                <tr className="border-b border-[#2B3139] text-xs text-[#848E9C]">
                  <th className="py-2 pr-4 font-medium">币种</th>
                  <th className="py-2 pr-4 font-medium">可用余额</th>
                  <th className="py-2 pr-4 font-medium">待入账</th>
                  <th className="py-2 font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((b) => (
                  <tr
                    key={b.token_symbol}
                    className="border-b border-[#2B3139]/60 last:border-0"
                  >
                    <td className="py-3 pr-4 font-medium">{b.token_symbol}</td>
                    <td className="py-3 pr-4 font-mono">
                      {formatTokenAmount(b.available, b.decimals)}
                    </td>
                    <td className="py-3 pr-4 font-mono">
                      {formatTokenAmount(b.pending, b.decimals)}
                    </td>
                    <td className="py-3">
                      <Link
                        href={`/?coin=${encodeURIComponent(b.token_symbol)}`}
                        className="text-[#FCD535] hover:underline"
                      >
                        去充值
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}

const WD_STATUS_LABEL: Record<string, string> = {
  QUEUED: "排队中",
  WHITELISTED: "白名单冷却",
  IN_FLIGHT: "链上确认中",
  CONFIRMED: "已完成",
  FAILED: "失败(已退款)",
  REJECTED: "已拒绝(已退款)",
};

function WithdrawCard() {
  const qc = useQueryClient();
  const assets = useQuery({ queryKey: ["assets"], queryFn: api.listAssets });
  const [chainId, setChainId] = useState<number | "">("");
  const [symbol, setSymbol] = useState("");
  const [toAddr, setToAddr] = useState("");
  const [amountText, setAmountText] = useState("");
  const [done, setDone] = useState("");

  const chains = useMemo(() => {
    const m = new Map<number, string>();
    for (const a of assets.data ?? []) m.set(a.chain_id, a.chain_name ?? `Chain ${a.chain_id}`);
    return [...m.entries()];
  }, [assets.data]);
  const symbols = useMemo(
    () => [...new Set((assets.data ?? []).filter((a) => a.chain_id === chainId).map((a) => a.token_symbol))],
    [assets.data, chainId],
  );
  const decimals = useMemo(
    () => (assets.data ?? []).find((a) => a.chain_id === chainId && a.token_symbol === symbol)?.decimals ?? 6,
    [assets.data, chainId, symbol],
  );

  const mut = useMutation({
    mutationFn: api.createWithdrawal,
    onSuccess: (wd) => {
      setDone(`提现已受理 ${wd.id.slice(0, 8)}… (冷却与确认需数分钟)`);
      setAmountText("");
      qc.invalidateQueries({ queryKey: ["balances"] });
      qc.invalidateQueries({ queryKey: ["withdrawals"] });
    },
  });

  const submit = () => {
    let atomic: string;
    try {
      atomic = parseUnits(amountText, decimals).toString();
      if (atomic === "0") throw new Error("amount must be > 0");
    } catch {
      setDone("");
      mut.reset();
      alert("金额格式不正确");
      return;
    }
    mut.mutate({
      chain_id: Number(chainId),
      token_symbol: symbol,
      to_address: toAddr.trim(),
      amount: atomic,
      idempotency_key: crypto.randomUUID(),
    });
  };

  return (
    <Card title="提现">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="网络">
          <select
            className={inputCls}
            value={chainId}
            onChange={(e) => {
              setChainId(e.target.value === "" ? "" : Number(e.target.value));
              setSymbol("");
            }}
          >
            <option value="">选择网络</option>
            {chains.map(([id, name]) => (
              <option key={id} value={id}>{name}</option>
            ))}
          </select>
        </Field>
        <Field label="币种">
          <select className={inputCls} value={symbol} onChange={(e) => setSymbol(e.target.value)}>
            <option value="">选择币种</option>
            {symbols.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </Field>
        <Field label="收款地址" hint="EVM 地址; 首次提现需等待地址白名单冷却">
          <input className={inputCls} value={toAddr} onChange={(e) => setToAddr(e.target.value)} placeholder="0x…" />
        </Field>
        <Field label={`金额 (${symbol || "—"})`}>
          <input className={inputCls} value={amountText} onChange={(e) => setAmountText(e.target.value)} placeholder="0.0" />
        </Field>
      </div>
      {mut.isError ? <div className="mt-3"><ErrorBox error={mut.error} /></div> : null}
      {done ? <p className="mt-3 text-sm text-[#FCD535]">{done}</p> : null}
      <button
        className={btnCls + " mt-4"}
        disabled={mut.isPending || chainId === "" || symbol === "" || !toAddr.trim() || !amountText.trim()}
        onClick={submit}
      >
        {mut.isPending ? "提交中…" : "提交提现"}
      </button>
    </Card>
  );
}

function WithdrawalHistory() {
  const wds = useQuery({ queryKey: ["withdrawals"], queryFn: api.listWithdrawals, refetchInterval: 10_000 });
  const assets = useQuery({ queryKey: ["assets"], queryFn: api.listAssets });
  const decimalsOf = (w: { chain_id: number; token_symbol: string }) =>
    (assets.data ?? []).find((a) => a.chain_id === w.chain_id && a.token_symbol === w.token_symbol)?.decimals ?? 6;
  const rows = wds.data ?? [];
  if (rows.length === 0) return null;
  return (
    <Card title="提现记录" className="mt-6">
      <div className="overflow-x-auto">
        <table className="w-full text-left text-sm">
          <thead>
            <tr className="border-b border-[#2B3139] text-xs text-[#848E9C]">
              <th className="py-2 pr-4 font-medium">时间</th>
              <th className="py-2 pr-4 font-medium">币种</th>
              <th className="py-2 pr-4 font-medium">金额</th>
              <th className="py-2 pr-4 font-medium">收款地址</th>
              <th className="py-2 pr-4 font-medium">状态</th>
              <th className="py-2 font-medium">Tx</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((w) => (
              <tr key={w.id} className="border-b border-[#2B3139]/60 last:border-0">
                <td className="py-3 pr-4 whitespace-nowrap">{new Date(w.created_at).toLocaleString()}</td>
                <td className="py-3 pr-4">{w.token_symbol}</td>
                <td className="py-3 pr-4 font-mono">{formatTokenAmount(w.amount, decimalsOf(w))}</td>
                <td className="py-3 pr-4 font-mono text-xs">
                  {w.to_address.slice(0, 6)}…{w.to_address.slice(-4)}
                </td>
                <td className="py-3 pr-4">
                  {WD_STATUS_LABEL[w.status] ?? w.status}
                  {w.fail_reason ? <span className="block text-xs text-red-300">{w.fail_reason}</span> : null}
                </td>
                <td className="py-3 font-mono text-xs">
                  {w.tx_hash ? `${w.tx_hash.slice(0, 10)}…` : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

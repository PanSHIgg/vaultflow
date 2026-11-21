"use client";

import { useRouter } from "next/navigation";
import { useEffect, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import QRCode from "react-qr-code";
import * as api from "@/lib/api";
import { formatTokenAmount } from "@/lib/amount";
import { clearSession, getEmail, getToken } from "@/lib/auth";
import { DEPOSIT_STATUS_LABEL, DEPOSIT_STATUS_TONE, fmtDateTime, shortAddr } from "@/lib/format";
import type { Asset } from "@/lib/types";
import { Badge, Card, CopyButton, Spinner } from "@/components/ui";

export default function DepositPage() {
  const router = useRouter();
  const qc = useQueryClient();
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [coin, setCoin] = useState<string | null>(null);
  const [chainId, setChainId] = useState<number | null>(null);

  useEffect(() => {
    if (!getToken()) {
      router.replace("/login");
      setAuthed(false);
    } else {
      setAuthed(true);
    }
  }, [router]);

  const assets = useQuery({ queryKey: ["assets"], queryFn: api.listAssets, enabled: authed === true });

  const coins = useMemo(() => {
    const seen = new Map<string, Asset>();
    for (const a of assets.data ?? []) if (!seen.has(a.token_symbol)) seen.set(a.token_symbol, a);
    return [...seen.keys()];
  }, [assets.data]);

  const networks = useMemo(
    () => (assets.data ?? []).filter((a) => a.token_symbol === coin),
    [assets.data, coin],
  );
  const network = networks.find((n) => n.chain_id === chainId) ?? networks[0] ?? null;

  useEffect(() => {
    if (coin == null && coins.length > 0) setCoin(coins[0]);
  }, [coins, coin]);
  useEffect(() => {
    if (networks.length > 0 && (chainId == null || !networks.some((n) => n.chain_id === chainId))) {
      setChainId(networks[0].chain_id);
    }
  }, [networks, chainId]);

  const address = useQuery({
    queryKey: ["deposit-address", network?.chain_id],
    queryFn: () => api.getDepositAddress(network!.chain_id),
    enabled: authed === true && network != null,
  });

  const deposits = useInfiniteQuery({
    queryKey: ["deposits", network?.chain_id],
    queryFn: ({ pageParam }) =>
      api.listDeposits({ ...(network ? { chain_id: network.chain_id } : {}), ...(pageParam ? { cursor: pageParam } : {}) }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: authed === true,
  });
  const depositItems = useMemo(
    () => (deposits.data?.pages ?? []).flatMap((p) => p.items),
    [deposits.data],
  );

  if (authed !== true) {
    return (
      <main className="flex min-h-screen items-center justify-center">
        <Spinner className="h-6 w-6 text-[#FCD535]" />
      </main>
    );
  }

  return (
    <main className="mx-auto min-h-screen max-w-5xl px-4 pb-16">
      {/* ---- top nav ---- */}
      <header className="flex items-center justify-between border-b border-[#2B3139] py-4">
        <h1 className="text-lg font-bold tracking-tight">
          Vault<span className="text-[#FCD535]">Flow</span>
        </h1>
        <div className="flex items-center gap-3 text-sm text-[#848E9C]">
          <span>{getEmail()}</span>
          <button
            type="button"
            className="hover:text-[#FCD535]"
            onClick={() => {
              qc.clear();
              clearSession();
              router.replace("/login");
            }}
          >
            退出
          </button>
        </div>
      </header>

      <h2 className="py-6 text-xl font-semibold">充值数字货币</h2>

      <div className="grid gap-4 lg:grid-cols-[1fr_1.2fr]">
        {/* ---- left: coin + network pickers ---- */}
        <div className="flex flex-col gap-4">
          <Card title="选择币种">
            {assets.isLoading ? (
              <div className="flex justify-center py-4"><Spinner className="text-[#FCD535]" /></div>
            ) : assets.isError ? (
              <p className="py-2 text-sm text-red-300">加载币种失败</p>
            ) : (
              <ul className="flex flex-col gap-1">
                {coins.map((c) => (
                  <li key={c}>
                    <button
                      type="button"
                      onClick={() => setCoin(c)}
                      className={`flex w-full items-center justify-between rounded-lg border px-3 py-2.5 text-sm transition ${
                        coin === c
                          ? "border-[#FCD535] bg-[#FCD535]/10 text-[#FCD535]"
                          : "border-transparent text-[#EAECEF] hover:bg-[#2B3139]/60"
                      }`}
                    >
                      <span className="font-medium">{c}</span>
                      {coin === c && <span>✓</span>}
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card title="选择网络">
            <ul className="flex flex-col gap-1">
              {networks.map((n) => (
                <li key={n.chain_id}>
                  <button
                    type="button"
                    onClick={() => setChainId(n.chain_id)}
                    className={`flex w-full items-center justify-between rounded-lg border px-3 py-2.5 text-sm transition ${
                      network?.chain_id === n.chain_id
                        ? "border-[#FCD535] bg-[#FCD535]/10 text-[#FCD535]"
                        : "border-transparent text-[#EAECEF] hover:bg-[#2B3139]/60"
                    }`}
                  >
                    <span className="font-medium">chain {n.chain_id}</span>
                    <span className="text-xs text-[#848E9C]">≈{n.required_confirmations} 确认</span>
                  </button>
                </li>
              ))}
            </ul>
          </Card>
        </div>

        {/* ---- right: deposit address card ---- */}
        <Card title={coin ? `${coin} 充值地址` : "充值地址"} className="self-start">
          {address.isLoading ? (
            <div className="flex justify-center py-10"><Spinner className="h-6 w-6 text-[#FCD535]" /></div>
          ) : address.isError ? (
            <p className="py-6 text-center text-sm text-red-300">获取充值地址失败</p>
          ) : address.data ? (
            <div className="flex flex-col items-center gap-5">
              <div className="rounded-xl bg-white p-3">
                <QRCode value={address.data.qr_payload || address.data.address} size={168} />
              </div>
              <div className="flex w-full items-center justify-between gap-2 rounded-lg border border-[#2B3139] bg-[#0B0E11] px-3 py-2.5">
                <span className="break-all font-mono text-sm text-[#EAECEF]">{address.data.address}</span>
                <CopyButton text={address.data.address} label="复制地址" />
              </div>
              <dl className="w-full space-y-2 text-sm">
                <div className="flex justify-between">
                  <dt className="text-[#848E9C]">最小充值金额</dt>
                  <dd className="font-mono">
                    {network ? formatTokenAmount(network.min_deposit, network.decimals, { symbol: coin ?? undefined }) : "—"}
                  </dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-[#848E9C]">预计到账</dt>
                  <dd>{network ? `${network.required_confirmations} 个网络确认后` : "—"}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-[#848E9C]">充值地址</dt>
                  <dd>您的专属地址，可重复使用</dd>
                </div>
              </dl>
              <p className="w-full rounded-lg bg-[#FCD535]/10 px-3 py-2 text-xs leading-5 text-[#FCD535]">
                请勿向该地址充值 {coin}（chain {network?.chain_id}）以外的任何资产，否则资产将无法找回。
              </p>
            </div>
          ) : (
            <p className="py-6 text-center text-sm text-[#848E9C]">请选择币种和网络</p>
          )}
        </Card>
      </div>

      {/* ---- deposit history ---- */}
      <Card title="充值记录" className="mt-4">
        {deposits.isLoading ? (
          <div className="flex justify-center py-6"><Spinner className="text-[#FCD535]" /></div>
        ) : depositItems.length === 0 ? (
          <p className="py-6 text-center text-sm text-[#848E9C]">暂无充值记录</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead>
                <tr className="border-b border-[#2B3139] text-xs text-[#848E9C]">
                  <th className="py-2 pr-4 font-medium">币种</th>
                  <th className="py-2 pr-4 font-medium">数量</th>
                  <th className="py-2 pr-4 font-medium">状态</th>
                  <th className="py-2 pr-4 font-medium">交易哈希</th>
                  <th className="py-2 font-medium">时间</th>
                </tr>
              </thead>
              <tbody>
                {depositItems.map((d) => (
                  <tr key={d.tx_hash} className="border-b border-[#2B3139]/60 last:border-0">
                    <td className="py-2.5 pr-4 font-medium">{d.token_symbol}</td>
                    <td className="py-2.5 pr-4 font-mono">
                      {formatTokenAmount(d.amount, d.decimals)}
                    </td>
                    <td className="py-2.5 pr-4">
                      <Badge tone={DEPOSIT_STATUS_TONE[d.status]}>
                        {d.status === "PENDING"
                          ? `确认中 ${d.confirmations}/${d.required_confirmations}`
                          : DEPOSIT_STATUS_LABEL[d.status]}
                      </Badge>
                    </td>
                    <td className="py-2.5 pr-4 font-mono text-xs text-[#848E9C]">{shortAddr(d.tx_hash, 10, 8)}</td>
                    <td className="py-2.5 text-xs text-[#848E9C]">{fmtDateTime(d.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {deposits.hasNextPage && (
              <div className="flex justify-center pt-3">
                <button
                  type="button"
                  onClick={() => deposits.fetchNextPage()}
                  disabled={deposits.isFetchingNextPage}
                  className="text-sm text-[#848E9C] transition hover:text-[#FCD535]"
                >
                  {deposits.isFetchingNextPage ? "加载中…" : "加载更多"}
                </button>
              </div>
            )}
          </div>
        )}
      </Card>
    </main>
  );
}

"use client";

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import QRCode from "react-qr-code";
import * as api from "@/lib/api";
import { formatTokenAmount } from "@/lib/amount";
import {
  DEPOSIT_STATUS_LABEL,
  DEPOSIT_STATUS_TONE,
  estDuration,
  fmtDateTime,
  shortAddr,
} from "@/lib/format";
import type { Asset } from "@/lib/types";
import { Badge, Card, CopyButton, Spinner } from "@/components/ui";
import { Shell } from "@/components/shell";

function Step({ n, done, last = false, title, children }: {
  n: number;
  done: boolean;
  last?: boolean;
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="flex gap-3">
      <div className="flex flex-col items-center">
        <span
          className={`grid h-6 w-6 shrink-0 place-items-center rounded-full text-xs font-bold ${
            done
              ? "bg-[#FCD535] text-[#0B0E11]"
              : "border border-[#FCD535] text-[#FCD535]"
          }`}
        >
          {done ? "✓" : n}
        </span>
        {!last && <span className="w-px flex-1 bg-[#2B3139]" />}
      </div>
      <div className={`min-w-0 flex-1 ${last ? "" : "pb-7"}`}>
        <p className="mb-3 pt-0.5 font-semibold">{title}</p>
        {children}
      </div>
    </div>
  );
}

function Select({ value, onChange, children }: {
  value: string;
  onChange: (value: string) => void;
  children: ReactNode;
}) {
  return (
    <div className="relative max-w-sm">
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full appearance-none rounded-lg border border-[#2B3139] bg-[#1E2329] px-3 py-2.5 pr-9 text-sm text-[#EAECEF] outline-none transition focus:border-[#FCD535]"
      >
        {children}
      </select>
      <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-[#848E9C]">
        ▾
      </span>
    </div>
  );
}

function InfoRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-6">
      <dt className="shrink-0 text-[#848E9C]">{label}</dt>
      <dd className="text-right">{value}</dd>
    </div>
  );
}

export default function DepositPage() {
  return (
    <Shell>
      <DepositContent />
    </Shell>
  );
}

function DepositContent() {
  const [coin, setCoin] = useState<string | null>(null);
  const [chainId, setChainId] = useState<number | null>(null);

  // Deep-link preselection from the assets page (/?coin=…&chain_id=…).
  useEffect(() => {
    const p = new URLSearchParams(window.location.search);
    const c = p.get("coin");
    const ch = p.get("chain_id");
    if (c) setCoin(c);
    if (ch != null && ch !== "") setChainId(Number(ch));
  }, []);

  const assets = useQuery({ queryKey: ["assets"], queryFn: api.listAssets });

  const coins = useMemo(() => {
    const seen = new Set<string>();
    for (const a of assets.data ?? []) seen.add(a.token_symbol);
    return [...seen];
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
    enabled: network != null,
  });

  const deposits = useInfiniteQuery({
    queryKey: ["deposits", network?.chain_id],
    queryFn: ({ pageParam }) =>
      api.listDeposits({
        ...(network ? { chain_id: network.chain_id } : {}),
        ...(pageParam ? { cursor: pageParam } : {}),
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  const depositItems = useMemo(
    () => (deposits.data?.pages ?? []).flatMap((p) => p.items),
    [deposits.data],
  );

  const addr = address.data?.address ?? "";
  const chainName = (a: Asset) => a.chain_name ?? `Chain ${a.chain_id}`;
  const estSeconds = network
    ? network.required_confirmations * (network.block_time_seconds ?? 12)
    : 0;

  return (
    <>
      <h2 className="py-6 text-xl font-semibold">充值数字货币</h2>

      <div className="max-w-xl">
        <Step n={1} done={coin != null} title="选择币种">
          {assets.isLoading ? (
            <Spinner className="text-[#FCD535]" />
          ) : assets.isError ? (
            <p className="text-sm text-red-300">加载币种失败</p>
          ) : (
            <Select value={coin ?? ""} onChange={setCoin}>
              {coins.map((c) => (
                <option key={c} value={c}>{c}</option>
              ))}
            </Select>
          )}
        </Step>

        <Step n={2} done={network != null} title="选择网络">
          <Select
            value={network ? String(network.chain_id) : ""}
            onChange={(v) => setChainId(Number(v))}
          >
            {networks.map((n) => (
              <option key={n.chain_id} value={n.chain_id}>
                {chainName(n)}
              </option>
            ))}
          </Select>
        </Step>

        <Step n={3} done={false} last title="充值详情">
          {address.isLoading ? (
            <div className="flex justify-center py-10">
              <Spinner className="h-6 w-6 text-[#FCD535]" />
            </div>
          ) : address.isError ? (
            <p className="py-6 text-sm text-red-300">获取充值地址失败</p>
          ) : address.data && network ? (
            <div className="flex flex-col gap-6">
              <div>
                <p className="mb-3 text-sm text-[#848E9C]">充币地址</p>
                <div className="inline-block rounded-xl bg-white p-3">
                  <QRCode value={address.data.qr_payload || addr} size={168} />
                </div>
                <div className="mt-3 flex items-center gap-2 rounded-lg border border-[#2B3139] bg-[#0B0E11] px-3 py-2.5">
                  <span className="break-all font-mono text-sm">
                    <span className="text-[#FCD535]">{addr.slice(0, 6)}</span>
                    <span className="text-[#EAECEF]">{addr.slice(6, -6)}</span>
                    <span className="text-[#FCD535]">{addr.slice(-6)}</span>
                  </span>
                  <CopyButton text={addr} label="复制" />
                </div>
              </div>

              <dl className="space-y-2.5 text-sm">
                <InfoRow label="充币账户" value="资金账户" />
                <InfoRow
                  label="最小充币额"
                  value={formatTokenAmount(network.min_deposit, network.decimals, {
                    symbol: coin ?? undefined,
                  })}
                />
                <InfoRow label="充币到账时间" value={estDuration(estSeconds)} />
                <InfoRow
                  label="代币合约"
                  value={
                    network.token_explorer_url ? (
                      <a
                        href={network.token_explorer_url}
                        target="_blank"
                        rel="noreferrer"
                        className="hover:text-[#FCD535]"
                      >
                        <span className="font-mono">{shortAddr(network.token_contract)}</span>{" "}
                        <span className="text-[#848E9C]">查看详情 ›</span>
                      </a>
                    ) : (
                      <span className="font-mono text-xs">{shortAddr(network.token_contract)}</span>
                    )
                  }
                />
              </dl>

              <div>
                <h3 className="mb-2 text-sm font-semibold">充币说明</h3>
                <ul className="list-disc space-y-1.5 pl-5 text-xs leading-5 text-[#848E9C]">
                  <li>
                    该地址仅支持通过 {chainName(network)} 网络充值 {coin}
                    ，请勿向此地址充值其他资产或选择其他网络，否则资产将无法找回。
                  </li>
                  <li>
                    最小充值金额为{" "}
                    {formatTokenAmount(network.min_deposit, network.decimals, {
                      symbol: coin ?? undefined,
                    })}
                    ，低于该金额的充值将无法入账。
                  </li>
                  <li>充值需 {network.required_confirmations} 个网络确认后方可到账。</li>
                  <li>该地址为您的专属充值地址，可重复使用。</li>
                </ul>
              </div>
            </div>
          ) : (
            <p className="py-6 text-sm text-[#848E9C]">请选择币种和网络</p>
          )}
        </Step>
      </div>

      {/* ---- deposit history ---- */}
      <Card title="充值记录" className="mt-8">
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
    </>
  );
}

"use client";

import Link from "next/link";
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import * as api from "@/lib/api";
import { formatTokenAmount } from "@/lib/amount";
import { Card, Spinner } from "@/components/ui";
import { Shell } from "@/components/shell";

export default function AssetsPage() {
  return (
    <Shell>
      <AssetsContent />
    </Shell>
  );
}

function AssetsContent() {
  const balances = useQuery({ queryKey: ["balances"], queryFn: api.listBalances });
  const assets = useQuery({ queryKey: ["assets"], queryFn: api.listAssets });

  const chainName = useMemo(() => {
    const m = new Map<number, string>();
    for (const a of assets.data ?? []) m.set(a.chain_id, a.chain_name ?? `Chain ${a.chain_id}`);
    return (id: number) => m.get(id) ?? `Chain ${id}`;
  }, [assets.data]);

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
                  <th className="py-2 pr-4 font-medium">网络</th>
                  <th className="py-2 pr-4 font-medium">可用余额</th>
                  <th className="py-2 pr-4 font-medium">待入账</th>
                  <th className="py-2 font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((b) => (
                  <tr
                    key={`${b.token_symbol}-${b.chain_id}`}
                    className="border-b border-[#2B3139]/60 last:border-0"
                  >
                    <td className="py-3 pr-4 font-medium">{b.token_symbol}</td>
                    <td className="py-3 pr-4 text-[#848E9C]">{chainName(b.chain_id)}</td>
                    <td className="py-3 pr-4 font-mono">
                      {formatTokenAmount(b.available, b.decimals)}
                    </td>
                    <td className="py-3 pr-4 font-mono">
                      {formatTokenAmount(b.pending, b.decimals)}
                    </td>
                    <td className="py-3">
                      <Link
                        href={`/?coin=${encodeURIComponent(b.token_symbol)}&chain_id=${b.chain_id}`}
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

// W6-3 — 14-account double-entry ledger board + double-layer reconciliation.

"use client";

import { useMemo } from "react";
import {
  ACCOUNT_TYPE_LABEL,
  fmtDateTime,
} from "@/lib/format";
import {
  ACCOUNT_NORMAL_SIDE,
  LEDGER_CREDIT_ACCOUNTS,
  LEDGER_DEBIT_ACCOUNTS,
  ZERO_SENTINEL_UUID,
  type LedgerAccount,
} from "@/lib/types";
import { formatTokenAmount, parseAmount } from "@/lib/amount";
import { assetInfo, chainName } from "@/lib/chains";
import { useLedgerAccounts, useReconciliation, useRunReconciliation } from "@/lib/hooks";
import { AmountText, Badge, btnCls, Card, ErrorBox, Spinner } from "@/components/ui";

const DECIMALS: Record<string, number> = { USDC: 6, USDT: 6, ETH: 18 };

function decimalsOf(a: LedgerAccount): number {
  return assetInfo(a.chain_id, a.asset_symbol)?.decimals ?? DECIMALS[a.asset_symbol] ?? 6;
}

function AccountCard({ a }: { a: LedgerAccount }) {
  const system = a.owner_merchant_id === ZERO_SENTINEL_UUID;
  return (
    <div className="rounded-lg border border-neutral-800 bg-neutral-950/60 p-3">
      <div className="flex items-center justify-between gap-2">
        <span className="font-mono text-xs text-neutral-400">{a.account_type}</span>
        <Badge tone={system ? "muted" : "info"}>{system ? "系统科目" : "商户科目"}</Badge>
      </div>
      <div className="mt-1 text-xs text-neutral-500">{ACCOUNT_TYPE_LABEL[a.account_type]}</div>
      <AmountText
        amount={a.balance}
        decimals={decimalsOf(a)}
        symbol={a.asset_symbol}
        className="mt-2 block text-lg font-semibold"
        maxFrac={6}
      />
      <div className="mt-1 text-[10px] text-neutral-600">{chainName(a.chain_id)}</div>
    </div>
  );
}

export default function DashboardPage() {
  const accounts = useLedgerAccounts();
  const recon = useReconciliation();
  const runRecon = useRunReconciliation();

  const grouped = useMemo(() => {
    const list = accounts.data?.data ?? [];
    const byKey = new Map<string, LedgerAccount[]>();
    for (const a of list) {
      const key = `${a.chain_id}:${a.asset_symbol}`;
      if (!byKey.has(key)) byKey.set(key, []);
      byKey.get(key)!.push(a);
    }
    return [...byKey.entries()].map(([key, rows]) => {
      const [chainId, symbol] = key.split(":");
      const debit = rows.filter((r) => ACCOUNT_NORMAL_SIDE[r.account_type] === "DEBIT");
      const credit = rows.filter((r) => ACCOUNT_NORMAL_SIDE[r.account_type] === "CREDIT");
      const dSum = debit.reduce((acc, r) => acc + parseAmount(r.balance), 0n);
      const cSum = credit.reduce((acc, r) => acc + parseAmount(r.balance), 0n);
      return { chainId: Number(chainId), symbol, rows, debit, credit, dSum, cSum, balanced: dSum === cSum };
    });
  }, [accounts.data]);

  const r = recon.data;

  return (
    <div className="space-y-6">
      <section>
        <h1 className="mb-1 text-xl font-bold">14 科目复式账本大盘</h1>
        <p className="text-sm text-neutral-500">
          借方 6 科目 / 贷方 8 科目 · NUMERIC(78,0) 十进制字符串 · 全科目非负不变量 · 每 5s 刷新
        </p>
      </section>

      {accounts.error && <ErrorBox error={accounts.error} />}

      {grouped.map((g) => (
        <Card
          key={`${g.chainId}-${g.symbol}`}
          title={`${chainName(g.chainId)} · ${g.symbol} 科目余额`}
          actions={
            <Badge tone={g.balanced ? "ok" : "bad"}>
              {g.balanced ? "试算平衡 Σ借=Σ贷 ✓" : "试算不平衡 ✗"}
            </Badge>
          }
        >
          <div className="grid gap-4 lg:grid-cols-2">
            <div>
              <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-red-400">
                借方科目（{g.debit.length}/6）
              </h3>
              <div className="grid gap-2 sm:grid-cols-2">
                {LEDGER_DEBIT_ACCOUNTS.map((t) => {
                  const a = g.debit.find((x) => x.account_type === t);
                  return a ? (
                    <AccountCard key={a.id} a={a} />
                  ) : (
                    <div key={t} className="rounded-lg border border-dashed border-neutral-800 p-3 text-xs text-neutral-600">
                      {t}
                      <div className="mt-1 text-[10px]">该币对下无科目行</div>
                    </div>
                  );
                })}
              </div>
              <div className="mt-2 text-right font-mono text-sm text-red-300">
                Σ借 = {formatTokenAmount(g.dSum, DECIMALS[g.symbol] ?? 6, { symbol: g.symbol, maxFrac: 6 })}
              </div>
            </div>
            <div>
              <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-emerald-400">
                贷方科目（{g.credit.length}/8）
              </h3>
              <div className="grid gap-2 sm:grid-cols-2">
                {LEDGER_CREDIT_ACCOUNTS.map((t) => {
                  const a = g.credit.find((x) => x.account_type === t);
                  return a ? (
                    <AccountCard key={a.id} a={a} />
                  ) : (
                    <div key={t} className="rounded-lg border border-dashed border-neutral-800 p-3 text-xs text-neutral-600">
                      {t}
                      <div className="mt-1 text-[10px]">该币对下无科目行</div>
                    </div>
                  );
                })}
              </div>
              <div className="mt-2 text-right font-mono text-sm text-emerald-300">
                Σ贷 = {formatTokenAmount(g.cSum, DECIMALS[g.symbol] ?? 6, { symbol: g.symbol, maxFrac: 6 })}
              </div>
            </div>
          </div>
        </Card>
      ))}

      {/* 双层对账巡检 */}
      <Card
        title="双层对账巡检（InvariantChecker 最近一次运行）"
        actions={
          <div className="flex items-center gap-2">
            {r && (
              <Badge tone={r.status === "PASSED" ? "ok" : r.status === "FAILED" ? "bad" : "info"}>{r.status}</Badge>
            )}
            <button type="button" className={btnCls} disabled={runRecon.isPending} onClick={() => runRecon.mutate()}>
              {runRecon.isPending ? <Spinner /> : null}手动触发巡检
            </button>
          </div>
        }
      >
        {recon.error && <ErrorBox error={recon.error} />}
        {r && (
          <div className="space-y-4">
            <div className="text-xs text-neutral-500">
              run {r.run_id ?? "—"} · {r.trigger ?? ""} · {fmtDateTime(r.finished_at ?? r.started_at)}
            </div>

            <div>
              <h3 className="mb-2 text-xs font-semibold text-neutral-300">Layer 1 — 账本内试算平衡</h3>
              <table className="w-full text-left text-sm">
                <thead className="text-xs text-neutral-500">
                  <tr>
                    <th className="py-1">链/币对</th>
                    <th>Σ DEBIT</th>
                    <th>Σ CREDIT</th>
                    <th>平衡</th>
                    <th>全科目 ≥ 0</th>
                  </tr>
                </thead>
                <tbody className="font-mono text-xs">
                  {(r.trial_balances ?? []).map((t) => (
                    <tr key={`${t.chain_id}-${t.asset_symbol}`} className="border-t border-neutral-800">
                      <td className="py-1.5">
                        {t.chain_id} / {t.asset_symbol}
                      </td>
                      <td>{formatTokenAmount(t.total_debit, DECIMALS[t.asset_symbol] ?? 6, { maxFrac: 6 })}</td>
                      <td>{formatTokenAmount(t.total_credit, DECIMALS[t.asset_symbol] ?? 6, { maxFrac: 6 })}</td>
                      <td>{t.balanced ? <Badge tone="ok">✓</Badge> : <Badge tone="bad">✗</Badge>}</td>
                      <td>{t.all_balances_non_negative ? <Badge tone="ok">✓</Badge> : <Badge tone="bad">✗</Badge>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div>
              <h3 className="mb-2 text-xs font-semibold text-neutral-300">
                Layer 2 — safe_head 链上快照 vs 账本（1-Wei 逐项核对）
              </h3>
              <table className="w-full text-left text-sm">
                <thead className="text-xs text-neutral-500">
                  <tr>
                    <th className="py-1">核对项</th>
                    <th>账本科目 / 余额</th>
                    <th>链上余额@safe_head</th>
                    <th>Δ</th>
                    <th>匹配</th>
                  </tr>
                </thead>
                <tbody className="font-mono text-xs">
                  {(r.custody_checks ?? []).map((c) => (
                    <tr key={c.check} className="border-t border-neutral-800">
                      <td className="py-1.5">{c.check}</td>
                      <td>
                        {c.ledger_account}
                        <br />
                        {formatTokenAmount(c.ledger_balance, DECIMALS[c.asset_symbol] ?? 6, { maxFrac: 6 })} {c.asset_symbol}
                      </td>
                      <td>
                        {formatTokenAmount(c.onchain_balance, DECIMALS[c.asset_symbol] ?? 6, { maxFrac: 6 })} {c.asset_symbol}
                        <br />#{c.safe_head_height}
                      </td>
                      <td>{c.delta}</td>
                      <td>{c.matched ? <Badge tone="ok">0-Wei 差 ✓</Badge> : <Badge tone="bad">Δ≠0 ✗</Badge>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {(r.failures?.length ?? 0) > 0 && (
              <div className="rounded-lg border border-red-800 bg-red-950/40 p-3 text-xs text-red-300">
                {r.failures!.map((f, i) => (
                  <div key={i}>• {f}</div>
                ))}
              </div>
            )}
          </div>
        )}
      </Card>
    </div>
  );
}

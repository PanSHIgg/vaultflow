// W6-4 — interactive adversarial-drill console: 4 ARCHITECTURE §8 scenarios,
// canonical-vs-orphan fork height tree, LIFO red-reversal journal replay, and
// the signer penetration audit card.

"use client";

import { useState } from "react";
import {
  AddressText,
  AmountText,
  Badge,
  btnCls,
  btnGhostCls,
  Card,
  ErrorBox,
  Field,
  inputCls,
  Spinner,
} from "@/components/ui";
import { formatTokenAmount } from "@/lib/amount";
import { fmtDateTime, shortAddr } from "@/lib/format";
import { useRunScenario, useSimAnvil, useSimRun } from "@/lib/hooks";
import type { SimRun, SimScenario, SimStep } from "@/lib/types";

const SCENARIOS: Array<{ id: SimScenario; title: string; desc: string }> = [
  {
    id: "shallow-reorg",
    title: "① 浅分叉回滚",
    desc: "未过 safe_head 的入站充值 → ORPHANED_SHALLOW；在途归集 INCLUDED_PENDING_SAFE → BROADCASTED/IN_FLIGHT，新主链无冲突重打包",
  },
  {
    id: "deep-reorg",
    title: "② 极端深分叉 LIFO 回滚",
    desc: "h=100 结算 → h=105 归集 → 商户提现，强制回滚至 h=99，105→100 逆序红冲，透支记 MERCHANT_REORG_RECEIVABLE 并自动冻结提现",
  },
  {
    id: "blacklist-returnbomb",
    title: "③ 黑名单隔离 + ReturnBomb",
    desc: "MockUSDT 拉黑 1/5 归集地址 + 2MB returndata 测试币，验证 is_quarantined 精准隔离与 Revert Gas 1-Wei 账平",
  },
  {
    id: "signer-penetration",
    title: "④ Signer 越权拦截",
    desc: "模拟被攻破 gateway-core 发送 4 类伪造请求 100% 拒签 + 同 Nonce RBF/GAP_FILLER 幂等槽平滑签发 + 审计哈希链校验",
  },
];

// ---------------------------------------------------------------------------
// Fork tree visualization
// ---------------------------------------------------------------------------

function ForkTree({ summary }: { summary: Record<string, unknown> }) {
  const forkPoint = Number(summary.fork_point_height ?? 0);
  const canonical = (summary.canonical_heights as number[] | undefined) ?? [];
  const orphans = (summary.orphan_heights as number[] | undefined) ?? [];
  if (!forkPoint && canonical.length === 0 && orphans.length === 0) return null;
  const maxH = Math.max(forkPoint, ...canonical, ...orphans);
  const rows: number[] = [];
  for (let h = maxH; h >= forkPoint - 1 && rows.length < 12; h--) rows.push(h);
  const orphanSet = new Set(orphans);
  const canonicalSet = new Set(canonical);
  return (
    <div>
      <h3 className="mb-2 text-xs font-semibold text-neutral-300">主链 / 孤块分叉高度树</h3>
      <div className="flex gap-8 font-mono text-xs">
        <div className="flex flex-col items-center gap-1">
          <div className="mb-1 text-emerald-400">CANONICAL</div>
          {rows.map((h) => (
            <div
              key={h}
              className={`flex h-8 w-24 items-center justify-center rounded border ${
                h === forkPoint
                  ? "border-amber-600 bg-amber-950/50 text-amber-300"
                  : canonicalSet.has(h)
                    ? "border-emerald-700 bg-emerald-950/50 text-emerald-300"
                    : "border-neutral-800 bg-neutral-900 text-neutral-500"
              }`}
            >
              #{h} {h === forkPoint ? "⤤ fork" : ""}
            </div>
          ))}
          <div className="text-neutral-600">…</div>
        </div>
        <div className="flex flex-col items-center gap-1">
          <div className="mb-1 text-red-400">ORPHANED</div>
          {rows.map((h) =>
            orphanSet.has(h) ? (
              <div key={h} className="flex h-8 w-24 items-center justify-center rounded border border-dashed border-red-700 bg-red-950/40 text-red-300">
                #{h} ✕
              </div>
            ) : (
              <div key={h} className="h-8 w-24" />
            ),
          )}
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// LIFO red-reversal replay
// ---------------------------------------------------------------------------

interface Reversal {
  height: number;
  kind: string;
  entries: Array<{ direction: string; account_type: string; amount: string }>;
}

function LifoReplay({ reversals }: { reversals: Reversal[] }) {
  const [idx, setIdx] = useState(0);
  const shown = reversals.slice(0, idx + 1);
  return (
    <div>
      <div className="mb-2 flex items-center justify-between">
        <h3 className="text-xs font-semibold text-neutral-300">
          LIFO 红冲分录流水回放（高→低 {reversals.map((r) => r.height).join(" → ")}）
        </h3>
        <div className="flex gap-2">
          <button type="button" className={btnGhostCls} disabled={idx <= 0} onClick={() => setIdx(idx - 1)}>
            ◀ 上一步
          </button>
          <button type="button" className={btnGhostCls} disabled={idx >= reversals.length - 1} onClick={() => setIdx(idx + 1)}>
            下一步 ▶
          </button>
        </div>
      </div>
      <div className="space-y-2">
        {shown.map((r) => (
          <div key={r.height} className="rounded-lg border border-red-900/60 bg-red-950/20 p-3">
            <div className="flex items-center justify-between text-xs">
              <span className="font-mono text-red-300">h={r.height} · {r.kind}</span>
              <Badge tone="bad">REORG_REVERSAL</Badge>
            </div>
            <table className="mt-2 w-full font-mono text-[11px]">
              <tbody>
                {r.entries.map((e, i) => (
                  <tr key={i} className="border-t border-red-950/50">
                    <td className={`py-1 w-16 ${e.direction === "DEBIT" ? "text-red-300" : "text-emerald-300"}`}>
                      {e.direction}
                    </td>
                    <td>{e.account_type}</td>
                    <td className="text-right">
                      <AmountText amount={e.amount} decimals={6} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
        {idx === reversals.length - 1 && (
          <p className="text-xs text-neutral-500">回放完毕 — 出站先冲回恢复 CUSTODY_UNSWEPT，再冲入站结算；LIFO 顺序保证余额充足。</p>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Signer penetration audit card
// ---------------------------------------------------------------------------

function SignerAudit({ summary }: { summary: Record<string, unknown> }) {
  const rejected = (summary.rejected_requests as Array<{ name: string; policy?: string; result: string; reason: string }> | undefined) ?? [];
  const legit = (summary.legit_requests as Array<{ name: string; result: string; slot?: string }> | undefined) ?? [];
  return (
    <div>
      <h3 className="mb-2 text-xs font-semibold text-neutral-300">Signer 策略防火墙拦截审计</h3>
      <table className="w-full text-left text-xs">
        <thead className="text-neutral-500">
          <tr>
            <th className="py-1">请求</th>
            <th>策略</th>
            <th>结果</th>
            <th>原因/槽位</th>
          </tr>
        </thead>
        <tbody>
          {rejected.map((r, i) => (
            <tr key={i} className="border-t border-neutral-800">
              <td className="py-1.5">{r.name}</td>
              <td className="font-mono">{r.policy}</td>
              <td>
                <Badge tone="bad">{r.result}</Badge>
              </td>
              <td className="text-neutral-400">{r.reason}</td>
            </tr>
          ))}
          {legit.map((r, i) => (
            <tr key={`l${i}`} className="border-t border-neutral-800">
              <td className="py-1.5">{r.name}</td>
              <td className="font-mono">IDEMPOTENT_SLOT</td>
              <td>
                <Badge tone="ok">{r.result}</Badge>
              </td>
              <td className="text-neutral-400">{r.slot}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="mt-2 flex items-center gap-2 text-xs">
        <Badge tone={summary.audit_chain_valid ? "ok" : "bad"}>
          VerifyAuditChain {summary.audit_chain_valid ? "valid ✓" : "INVALID ✗"}
        </Badge>
        <span className="text-neutral-500">
          审计条目 {String(summary.audit_entries ?? "—")} · 拒签率 {String(summary.reject_rate ?? "—")}
        </span>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

const STEP_TONE: Record<SimStep["status"], "ok" | "warn" | "bad" | "info" | "muted"> = {
  OK: "ok",
  RUNNING: "info",
  FAILED: "bad",
  SKIPPED: "muted",
  PENDING: "muted",
};

export default function SimulatorPage() {
  const runScenario = useRunScenario();
  const anvil = useSimAnvil();
  const [runId, setRunId] = useState<string>();
  const run = useSimRun(runId);
  const [anvilParams, setAnvilParams] = useState("{}");
  const [anvilAction, setAnvilAction] = useState<"snapshot" | "mine" | "revert" | "drop-transaction">("snapshot");

  const launch = async (s: SimScenario) => {
    const r = await runScenario.mutateAsync(s);
    setRunId(r.run_id);
  };

  const data: SimRun | undefined = run.data;
  const summary = data?.summary;

  return (
    <div className="space-y-6">
      <section>
        <h1 className="mb-1 text-xl font-bold">现场可交互故障演练舱</h1>
        <p className="text-sm text-neutral-500">
          ARCHITECTURE §8 四大对抗场景 · ENABLE_DEV_SIMULATOR=true 网关专属（x-dev-only，生产不可用）
        </p>
      </section>

      <div className="grid gap-3 sm:grid-cols-2">
        {SCENARIOS.map((s) => (
          <Card key={s.id} title={s.title}>
            <p className="min-h-12 text-xs leading-relaxed text-neutral-400">{s.desc}</p>
            <button
              type="button"
              className={btnCls + " mt-3"}
              disabled={runScenario.isPending}
              onClick={() => void launch(s.id).catch(() => undefined)}
            >
              {runScenario.isPending && runScenario.variables === s.id ? <Spinner /> : null}
              一键触发 {s.id}
            </button>
          </Card>
        ))}
      </div>

      {runScenario.error && <ErrorBox error={runScenario.error} />}

      {data && (
        <Card
          title={`演练回放 — ${data.scenario} · run ${shortAddr(data.run_id, 10, 6)}`}
          actions={<Badge tone={data.status === "COMPLETED" ? "ok" : data.status === "FAILED" ? "bad" : "info"}>{data.status}</Badge>}
        >
          <div className="space-y-5">
            {/* step timeline */}
            <ol className="space-y-1.5">
              {data.steps.map((s) => (
                <li key={s.seq} className="flex items-start gap-3 text-sm">
                  <span className="mt-0.5 font-mono text-xs text-neutral-600">{String(s.seq).padStart(2, "0")}</span>
                  <Badge tone={STEP_TONE[s.status]}>
                    {s.status === "RUNNING" ? "RUNNING…" : s.status}
                  </Badge>
                  <div className="min-w-0 flex-1">
                    <div className="text-neutral-200">{s.name}</div>
                    {s.detail && <div className="text-xs text-neutral-500">{s.detail}</div>}
                    {s.tx_hash && <div className="font-mono text-[10px] text-neutral-600">tx {s.tx_hash}</div>}
                  </div>
                  {s.at && <span className="text-[10px] text-neutral-600">{fmtDateTime(s.at)}</span>}
                </li>
              ))}
            </ol>

            {summary && (
              <div className="grid gap-5 lg:grid-cols-2">
                <div className="space-y-5">
                  <ForkTree summary={summary} />
                  {Array.isArray(summary.lifo_reversals) && (
                    <LifoReplay reversals={summary.lifo_reversals as Reversal[]} />
                  )}
                </div>
                <div className="space-y-5">
                  {Array.isArray(summary.rejected_requests) && <SignerAudit summary={summary} />}
                  {Array.isArray(summary.quarantined) && (
                    <div>
                      <h3 className="mb-2 text-xs font-semibold text-neutral-300">隔离与 ReturnBomb 结果</h3>
                      <div className="space-y-1 text-xs text-neutral-400">
                        <div>
                          正常归集 {String(summary.swept_count ?? "—")}/5 · 隔离 {String(summary.quarantined_count ?? "—")} ·
                          ReturnBomb returndata {String(summary.returnbomb_returndata_bytes ?? "—")}B 仅读取{" "}
                          {String(summary.returnbomb_read_bytes ?? "—")}B
                        </div>
                        {(summary.quarantined as Array<{ address: string; reason: string }>).map((q, i) => (
                          <div key={i} className="flex items-center gap-2 font-mono">
                            <AddressText value={q.address} /> <Badge tone="bad">{q.reason}</Badge>
                          </div>
                        ))}
                        {typeof summary.gas_settle === "object" && summary.gas_settle != null && (
                          <div>
                            Revert 交易 Gas 已记账 · HOT_WALLET_NATIVE 1-Wei 账平：
                            {(summary.gas_settle as { matched_1wei?: boolean }).matched_1wei ? " ✓" : " ✗"}
                          </div>
                        )}
                      </div>
                    </div>
                  )}
                  {summary.payout_frozen === true && (
                    <div className="rounded-lg border border-red-800 bg-red-950/40 p-3 text-xs text-red-300">
                      merchants.payout_frozen = true · MERCHANT_REORG_RECEIVABLE ={" "}
                      {formatTokenAmount(String(summary.reorg_receivable ?? "0"), 6, { symbol: "USDC" })}
                    </div>
                  )}
                  {Array.isArray(summary.orphaned_deposits) && (
                    <div className="text-xs text-neutral-400">
                      {(summary.orphaned_deposits as Array<{ tx_hash: string; status: string }>).map((d, i) => (
                        <div key={i} className="font-mono">
                          孤块充值 {shortAddr(d.tx_hash, 12, 8)} → <Badge tone="warn">{d.status}</Badge>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              </div>
            )}
            {data.error && <ErrorBox error={new Error(data.error)} />}
          </div>
        </Card>
      )}

      {/* raw anvil passthrough */}
      <Card title="Anvil 链控直通（/v1/sim/anvil/{action}）">
        <div className="flex flex-wrap items-end gap-3">
          <Field label="action">
            <select
              className={inputCls + " w-48"}
              value={anvilAction}
              onChange={(e) => setAnvilAction(e.target.value as typeof anvilAction)}
            >
              <option value="snapshot">snapshot (evm_snapshot)</option>
              <option value="mine">mine (anvil_mine)</option>
              <option value="revert">revert (evm_revert)</option>
              <option value="drop-transaction">drop-transaction</option>
            </select>
          </Field>
          <Field label="params JSON（mine→{blocks} revert→{snapshot_id} drop→{tx_hash}）">
            <input className={inputCls + " w-96"} value={anvilParams} onChange={(e) => setAnvilParams(e.target.value)} />
          </Field>
          <button
            type="button"
            className={btnCls}
            disabled={anvil.isPending}
            onClick={() => {
              let params: Record<string, unknown> = {};
              try {
                params = JSON.parse(anvilParams || "{}");
              } catch {
                return;
              }
              void anvil.mutateAsync({ action: anvilAction, params }).catch(() => undefined);
            }}
          >
            {anvil.isPending && <Spinner />}执行
          </button>
        </div>
        {anvil.data && (
          <pre className="mt-3 overflow-x-auto rounded-lg border border-neutral-800 bg-neutral-950 p-3 font-mono text-xs text-emerald-300">
            {JSON.stringify(anvil.data, null, 2)}
          </pre>
        )}
        {anvil.error && <div className="mt-3"><ErrorBox error={anvil.error} /></div>}
      </Card>
    </div>
  );
}

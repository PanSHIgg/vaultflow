// W6-3 — outbound tx pipeline, nonce cursors / NONCE_STOLEN_HALT breakers,
// quarantined vaults, and vault-signer health + audit-chain integrity.

"use client";

import { useState } from "react";
import {
  AddressText,
  AmountText,
  Badge,
  btnGhostCls,
  Card,
  ErrorBox,
  ProgressBar,
} from "@/components/ui";
import { formatTokenAmount } from "@/lib/amount";
import { chainName } from "@/lib/chains";
import {
  fmtDateTime,
  INTENT_STATUS_LABEL,
  shortAddr,
} from "@/lib/format";
import { useOutboundQueue, useQuarantinedVaults, useSignerHealth } from "@/lib/hooks";
import type { OutboundTxIntent } from "@/lib/types";

const ATTEMPT_STATUS_TONE: Record<string, "ok" | "warn" | "bad" | "info" | "muted"> = {
  BROADCASTED: "info",
  INCLUDED_UNSAFE: "warn",
  FINALIZED: "ok",
  SUPERSEDED: "muted",
  DROPPED: "bad",
};

const INTENT_STATUS_TONE: Record<string, "ok" | "warn" | "bad" | "info" | "muted"> = {
  QUEUED: "muted",
  IN_FLIGHT: "info",
  INCLUDED_PENDING_SAFE: "warn",
  CONFIRMED: "ok",
  FAILED_REVERTED: "bad",
  CANCELLED_BY_FILLER: "muted",
  NONCE_STOLEN_HALT: "bad",
};

function IntentRow({ intent }: { intent: OutboundTxIntent }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <tr className="border-t border-neutral-800 text-xs">
        <td className="py-2">
          <button type="button" className="font-mono text-sky-400 hover:underline" onClick={() => setOpen(!open)}>
            {open ? "▾" : "▸"} {shortAddr(intent.id, 10, 6)}
          </button>
          <div className="text-neutral-500">{intent.intent_type}</div>
        </td>
        <td className="font-mono">{intent.wallet_address && <AddressText value={intent.wallet_address} />}</td>
        <td className="font-mono">#{intent.nonce}</td>
        <td>
          <Badge tone={INTENT_STATUS_TONE[intent.status]}>{INTENT_STATUS_LABEL[intent.status]}</Badge>
        </td>
        <td className="text-neutral-500">{fmtDateTime(intent.created_at)}</td>
      </tr>
      {open && (
        <tr className="border-t border-neutral-900 bg-neutral-950/60">
          <td colSpan={5} className="px-4 py-3">
            <table className="w-full text-left text-[11px]">
              <thead className="text-neutral-600">
                <tr>
                  <th>#</th>
                  <th>kind</th>
                  <th>tx_hash</th>
                  <th>状态</th>
                  <th>块高</th>
                  <th>receipt</th>
                  <th>maxFee / tip</th>
                  <th>gas_used</th>
                </tr>
              </thead>
              <tbody className="font-mono">
                {intent.attempts.map((a) => (
                  <tr key={a.id} className="border-t border-neutral-900">
                    <td className="py-1">{a.attempt_no}</td>
                    <td>{a.attempt_kind}</td>
                    <td>
                      <AddressText value={a.tx_hash} copy />
                    </td>
                    <td>
                      <Badge tone={ATTEMPT_STATUS_TONE[a.status]}>{a.status}</Badge>
                    </td>
                    <td>{a.block_number ?? "—"}</td>
                    <td>{a.receipt_status === 1 ? "✓" : a.receipt_status === 0 ? "✗" : "—"}</td>
                    <td>
                      {a.max_fee_per_gas && a.max_priority_fee_per_gas
                        ? `${formatTokenAmount(a.max_fee_per_gas, 9, { maxFrac: 2 })} / ${formatTokenAmount(a.max_priority_fee_per_gas, 9, { maxFrac: 2 })} gwei`
                        : "—"}
                    </td>
                    <td>{a.gas_used ?? "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </td>
        </tr>
      )}
    </>
  );
}

export default function OpsPage() {
  const queue = useOutboundQueue();
  const quarantine = useQuarantinedVaults();
  const signer = useSignerHealth();
  const h = signer.data;

  return (
    <div className="space-y-6">
      <section>
        <h1 className="mb-1 text-xl font-bold">出站队列与运维监控</h1>
        <p className="text-sm text-neutral-500">
          TxManager 意图流水线 · Nonce 游标 · RBF 历史 · NONCE_STOLEN_HALT 熔断 · 隔离金库 · Signer 审计链
        </p>
      </section>

      {/* NONCE_STOLEN_HALT banner */}
      {(queue.data?.halted_wallets.length ?? 0) > 0 && (
        <div className="rounded-xl border border-red-700 bg-red-950/60 p-4">
          <div className="font-semibold text-red-300">⛔ NONCE_STOLEN_HALT — 热钱包私钥带外 Nonce 消耗告警</div>
          <ul className="mt-2 space-y-1 text-sm text-red-200">
            {queue.data!.halted_wallets.map((w) => (
              <li key={w.intent_id} className="font-mono text-xs">
                chain {w.chain_id} · wallet {shortAddr(w.wallet_address, 10, 6)} · intent {shortAddr(w.intent_id, 10, 6)} — 该钱包出站队列已熔断暂停
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Nonce cursors */}
      <Card title="热钱包 Nonce 游标（wallet_nonce_cursors）">
        {queue.error && <ErrorBox error={queue.error} />}
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-neutral-500">
            <tr>
              <th className="py-1">链</th>
              <th>角色</th>
              <th>钱包地址</th>
              <th>next_nonce</th>
              <th>队列状态</th>
              <th>更新</th>
            </tr>
          </thead>
          <tbody className="text-xs">
            {(queue.data?.nonce_cursors ?? []).map((c) => (
              <tr key={`${c.chain_id}-${c.wallet_address}`} className="border-t border-neutral-800">
                <td className="py-2">{c.chain_id}</td>
                <td>
                  <Badge tone={c.wallet_role === "RELAYER_HOT" ? "info" : "warn"}>{c.wallet_role}</Badge>
                </td>
                <td>
                  <AddressText value={c.wallet_address} copy />
                </td>
                <td className="font-mono text-base">{c.next_nonce}</td>
                <td>{c.halted ? <Badge tone="bad">HALTED</Badge> : <Badge tone="ok">RUNNING</Badge>}</td>
                <td className="text-neutral-500">{fmtDateTime(c.updated_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      {/* Intent pipeline */}
      <Card title="出站意图流水线（outbound_tx_intents + attempts RBF 阶梯）">
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-neutral-500">
            <tr>
              <th className="py-1">Intent / 类型</th>
              <th>钱包</th>
              <th>Nonce</th>
              <th>状态</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            {(queue.data?.data ?? []).map((i) => (
              <IntentRow key={i.id} intent={i} />
            ))}
          </tbody>
        </table>
      </Card>

      <div className="grid gap-4 lg:grid-cols-2">
        {/* Quarantined vaults */}
        <Card title={`隔离金库（is_quarantined）— ${quarantine.data?.data.length ?? 0} 个`}>
          {quarantine.error && <ErrorBox error={quarantine.error} />}
          <div className="space-y-2">
            {(quarantine.data?.data ?? []).map((v) => (
              <div key={v.deposit_address_id} className="rounded-lg border border-amber-800/60 bg-amber-950/20 p-3 text-xs">
                <div className="flex items-center justify-between">
                  <AddressText value={v.address} copy />
                  <Badge tone="bad">{v.item_status ?? "QUARANTINED"}</Badge>
                </div>
                <div className="mt-1 text-neutral-400">
                  {v.asset_symbol} · {chainName(v.chain_id)} · {v.reason}
                </div>
                <div className="mt-1 text-neutral-500">
                  关联订单 {v.related_order_id ? shortAddr(v.related_order_id, 10, 6) : "—"} · 隔离于 {fmtDateTime(v.quarantined_at)}
                </div>
              </div>
            ))}
            {(quarantine.data?.data.length ?? 0) === 0 && (
              <p className="py-4 text-center text-sm text-neutral-600">无隔离金库 — 归集批次全部健康</p>
            )}
          </div>
        </Card>

        {/* Signer health */}
        <Card title="vault-signer 健康与审计链">
          {signer.error && <ErrorBox error={signer.error} />}
          {h && (
            <div className="space-y-3 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <Badge tone={h.reachable ? "ok" : "bad"}>{h.reachable ? "mTLS 可达" : "不可达"}</Badge>
                <Badge tone={h.mtls_ok ? "ok" : "bad"}>mTLS {h.mtls_ok ? "✓" : "✗"}</Badge>
                <span className="text-xs text-neutral-500">
                  {h.signer_version} · uptime {h.uptime_seconds}s
                </span>
              </div>
              <div className="grid grid-cols-2 gap-2 text-xs">
                {(h.wallets ?? []).map((w) => (
                  <div key={w.address} className="rounded-lg border border-neutral-800 p-2">
                    <Badge tone="info">{w.role}</Badge>
                    <div className="mt-1">
                      <AddressText value={w.address} copy />
                    </div>
                    <div className="mt-1 text-neutral-500">
                      chains [{w.chain_ids.join(", ")}] · {w.loaded ? "密钥已加载" : "未加载"}
                    </div>
                  </div>
                ))}
              </div>
              {h.circuit_breaker && (
                <div>
                  <div className="mb-1 flex justify-between text-xs text-neutral-500">
                    <span>24h 滑动限额熔断器 {h.circuit_breaker.tripped ? "(已跳闸)" : ""}</span>
                    <span className="font-mono">
                      <AmountText amount={h.circuit_breaker.rolling_24h_used ?? "0"} decimals={6} /> /{" "}
                      <AmountText amount={h.circuit_breaker.rolling_24h_limit ?? "0"} decimals={6} symbol="USDC" />
                    </span>
                  </div>
                  <ProgressBar
                    value={BigInt(h.circuit_breaker.rolling_24h_used ?? "0")}
                    max={BigInt(h.circuit_breaker.rolling_24h_limit ?? "1")}
                    tone={h.circuit_breaker.tripped ? "bad" : "info"}
                  />
                </div>
              )}
              <div className="grid grid-cols-3 gap-2 text-center text-xs">
                <div className="rounded-lg border border-neutral-800 p-2">
                  <div className="text-lg font-semibold">{h.whitelist?.active_count ?? "—"}</div>
                  <div className="text-neutral-500">白名单 ACTIVE</div>
                </div>
                <div className="rounded-lg border border-neutral-800 p-2">
                  <div className="text-lg font-semibold">{h.whitelist?.pending_cooldown_count ?? "—"}</div>
                  <div className="text-neutral-500">冷却中</div>
                </div>
                <div className="rounded-lg border border-neutral-800 p-2">
                  <div className="text-lg font-semibold">{h.whitelist?.revoked_count ?? "—"}</div>
                  <div className="text-neutral-500">已吊销</div>
                </div>
              </div>
              <div className="rounded-lg border border-neutral-800 p-2 text-xs">
                <div className="flex items-center justify-between">
                  <span className="text-neutral-400">意图幂等槽 open / filler_signed</span>
                  <span className="font-mono">
                    {h.intent_slots?.open_slots ?? "—"} / {h.intent_slots?.filler_signed_slots ?? "—"}
                  </span>
                </div>
              </div>
              <div className={`rounded-lg border p-3 ${h.audit_chain?.valid ? "border-emerald-800 bg-emerald-950/30" : "border-red-800 bg-red-950/30"}`}>
                <div className="flex items-center justify-between">
                  <span className="font-semibold">审计哈希链 VerifyAuditChain</span>
                  <Badge tone={h.audit_chain?.valid ? "ok" : "bad"}>{h.audit_chain?.valid ? "valid ✓" : "INVALID ✗"}</Badge>
                </div>
                <div className="mt-1 font-mono text-[11px] text-neutral-400">
                  last_seq={h.audit_chain?.last_seq} · last_hash={shortAddr(h.audit_chain?.last_entry_hash, 12, 8)}
                  <br />
                  校验于 {fmtDateTime(h.audit_chain?.verified_at)} · 巡检间隔 {h.audit_chain?.verification_interval_seconds}s
                </div>
              </div>
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}

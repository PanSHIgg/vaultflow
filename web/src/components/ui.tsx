// Small shared UI primitives (Tailwind only, no component lib).

"use client";

import { useState, type ReactNode } from "react";
import { formatTokenAmount } from "@/lib/amount";
import { shortAddr } from "@/lib/format";

export function Card({ title, children, className = "", actions }: {
  title?: string;
  children: ReactNode;
  className?: string;
  actions?: ReactNode;
}) {
  return (
    <section className={`rounded-xl border border-neutral-800 bg-neutral-900/60 p-4 ${className}`}>
      {(title || actions) && (
        <header className="mb-3 flex items-center justify-between gap-2">
          {title && <h2 className="text-sm font-semibold text-neutral-200">{title}</h2>}
          {actions}
        </header>
      )}
      {children}
    </section>
  );
}

const TONE: Record<string, string> = {
  ok: "border-emerald-700 bg-emerald-950/60 text-emerald-300",
  warn: "border-amber-700 bg-amber-950/60 text-amber-300",
  bad: "border-red-700 bg-red-950/60 text-red-300",
  info: "border-sky-700 bg-sky-950/60 text-sky-300",
  muted: "border-neutral-700 bg-neutral-800/60 text-neutral-400",
};

export function Badge({ tone = "muted", children }: { tone?: keyof typeof TONE | string; children: ReactNode }) {
  return (
    <span className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium ${TONE[tone] ?? TONE.muted}`}>
      {children}
    </span>
  );
}

export function CopyButton({ text, label = "复制" }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
        } catch {
          // clipboard API may be unavailable on http:// — fall through to prompt
          window.prompt("手动复制：", text);
        }
        setDone(true);
        setTimeout(() => setDone(false), 1500);
      }}
      className="rounded-md border border-neutral-700 px-2 py-1 text-xs text-neutral-300 transition hover:border-neutral-500 hover:text-white"
    >
      {done ? "已复制 ✓" : label}
    </button>
  );
}

export function AddressText({ value, copy = false }: { value: string; copy?: boolean }) {
  return (
    <span className="inline-flex items-center gap-1.5 font-mono text-xs">
      <span title={value}>{shortAddr(value, 8, 6)}</span>
      {copy && <CopyButton text={value} />}
    </span>
  );
}

export function AmountText({
  amount,
  decimals,
  symbol,
  className = "",
  maxFrac,
}: {
  amount: string | bigint;
  decimals: number;
  symbol?: string;
  className?: string;
  maxFrac?: number;
}) {
  return (
    <span className={`font-mono tabular-nums ${className}`} title={`${amount} atomic`}>
      {formatTokenAmount(amount, decimals, { symbol, maxFrac })}
    </span>
  );
}

export function ProgressBar({ value, max, tone = "info" }: { value: number | bigint; max: number | bigint; tone?: "ok" | "warn" | "info" | "bad" }) {
  const v = Number(value);
  const m = Math.max(1, Number(max));
  const pct = Math.min(100, Math.max(0, (v / m) * 100));
  const color =
    tone === "ok" ? "bg-emerald-500" : tone === "warn" ? "bg-amber-500" : tone === "bad" ? "bg-red-500" : "bg-sky-500";
  return (
    <div className="h-2 w-full overflow-hidden rounded-full bg-neutral-800">
      <div className={`h-full rounded-full transition-all duration-500 ${color}`} style={{ width: `${pct}%` }} />
    </div>
  );
}

export function Spinner({ className = "h-4 w-4" }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={`animate-spin text-current ${className}`} fill="none" aria-label="loading">
      <circle cx="12" cy="12" r="10" stroke="currentColor" strokeOpacity="0.25" strokeWidth="3" />
      <path d="M12 2a10 10 0 0 1 10 10" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

export function ErrorBox({ error }: { error: unknown }) {
  if (!error) return null;
  const msg = error instanceof Error ? error.message : String(error);
  const code = (error as { code?: string })?.code;
  return (
    <div className="rounded-lg border border-red-800 bg-red-950/50 px-3 py-2 text-sm text-red-300">
      {code ? `[${code}] ` : ""}
      {msg}
    </div>
  );
}

export function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="block">
      <span className="mb-1 block text-xs font-medium text-neutral-400">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-[11px] text-neutral-500">{hint}</span>}
    </label>
  );
}

export const inputCls =
  "w-full rounded-lg border border-neutral-700 bg-neutral-950 px-3 py-2 font-mono text-sm text-neutral-100 outline-none transition focus:border-sky-600 placeholder:text-neutral-600";

export const btnCls =
  "inline-flex items-center justify-center gap-2 rounded-lg border border-sky-700 bg-sky-900/50 px-3 py-2 text-sm font-medium text-sky-200 transition hover:bg-sky-800/60 disabled:cursor-not-allowed disabled:border-neutral-700 disabled:bg-neutral-800 disabled:text-neutral-500";

export const btnDangerCls =
  "inline-flex items-center justify-center gap-2 rounded-lg border border-red-800 bg-red-950/50 px-3 py-2 text-sm font-medium text-red-300 transition hover:bg-red-900/60 disabled:cursor-not-allowed disabled:border-neutral-700 disabled:bg-neutral-800 disabled:text-neutral-500";

export const btnGhostCls =
  "inline-flex items-center justify-center gap-2 rounded-lg border border-neutral-700 bg-neutral-800/50 px-3 py-2 text-sm font-medium text-neutral-300 transition hover:border-neutral-500 hover:text-white disabled:cursor-not-allowed disabled:opacity-50";

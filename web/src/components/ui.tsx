// Small shared UI primitives (Tailwind only, no component lib).

"use client";

import { useState, type ReactNode } from "react";

export function Card({ title, children, className = "", actions }: {
  title?: string;
  children: ReactNode;
  className?: string;
  actions?: ReactNode;
}) {
  return (
    <section className={`rounded-xl border border-[#2B3139] bg-[#1E2329] p-4 ${className}`}>
      {(title || actions) && (
        <header className="mb-3 flex items-center justify-between gap-2">
          {title && <h2 className="text-sm font-semibold text-[#EAECEF]">{title}</h2>}
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
  info: "border-[#FCD535]/40 bg-[#FCD535]/10 text-[#FCD535]",
  muted: "border-[#2B3139] bg-[#2B3139]/60 text-[#848E9C]",
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
          window.prompt("手动复制：", text);
        }
        setDone(true);
        setTimeout(() => setDone(false), 1500);
      }}
      className="rounded-md border border-[#2B3139] px-2 py-1 text-xs text-[#EAECEF] transition hover:border-[#FCD535] hover:text-[#FCD535]"
    >
      {done ? "已复制 ✓" : label}
    </button>
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
  return (
    <div className="rounded-lg border border-red-800 bg-red-950/50 px-3 py-2 text-sm text-red-300">
      {msg}
    </div>
  );
}

export function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="block">
      <span className="mb-1 block text-xs font-medium text-[#848E9C]">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-[11px] text-[#5E6673]">{hint}</span>}
    </label>
  );
}

export const inputCls =
  "w-full rounded-lg border border-[#2B3139] bg-[#0B0E11] px-3 py-2 text-sm text-[#EAECEF] outline-none transition focus:border-[#FCD535] placeholder:text-[#5E6673]";

export const btnCls =
  "inline-flex items-center justify-center gap-2 rounded-lg bg-[#FCD535] px-3 py-2 text-sm font-semibold text-[#0B0E11] transition hover:bg-[#F0B90B] disabled:cursor-not-allowed disabled:bg-[#2B3139] disabled:text-[#5E6673]";

export const btnGhostCls =
  "inline-flex items-center justify-center gap-2 rounded-lg border border-[#2B3139] bg-[#2B3139]/50 px-3 py-2 text-sm font-medium text-[#EAECEF] transition hover:border-[#FCD535] hover:text-[#FCD535] disabled:cursor-not-allowed disabled:opacity-50";

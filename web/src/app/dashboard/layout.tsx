"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { AdminKeyProvider } from "@/lib/admin-key";
import { USE_MOCK } from "@/lib/mock";

const NAV = [
  { href: "/dashboard", label: "账本与对账大盘" },
  { href: "/dashboard/exceptions", label: "异常单工作台" },
  { href: "/dashboard/ops", label: "出站队列与运维" },
  { href: "/dashboard/approvals", label: "离线 Ed25519 审批" },
  { href: "/dashboard/simulator", label: "故障演练舱" },
];

export default function DashboardLayout({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  return (
    <AdminKeyProvider>
      <div className="mx-auto max-w-6xl px-4 py-6">
        <header className="mb-6 flex flex-wrap items-center justify-between gap-3">
          <div className="flex items-center gap-3">
            <Link href="/" className="text-lg font-bold tracking-tight">
              VaultFlow <span className="text-sm font-normal text-neutral-500">Console</span>
            </Link>
            <span
              className={`rounded-full border px-2 py-0.5 text-[11px] ${
                USE_MOCK
                  ? "border-amber-700 bg-amber-950/50 text-amber-300"
                  : "border-emerald-700 bg-emerald-950/50 text-emerald-300"
              }`}
            >
              {USE_MOCK ? "MOCK" : "LIVE"}
            </span>
          </div>
          <nav className="flex flex-wrap gap-1">
            {NAV.map((n) => {
              const active = pathname === n.href;
              return (
                <Link
                  key={n.href}
                  href={n.href}
                  className={`rounded-lg px-3 py-1.5 text-sm transition ${
                    active
                      ? "bg-sky-900/60 text-sky-200"
                      : "text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200"
                  }`}
                >
                  {n.label}
                </Link>
              );
            })}
          </nav>
        </header>
        {children}
      </div>
    </AdminKeyProvider>
  );
}

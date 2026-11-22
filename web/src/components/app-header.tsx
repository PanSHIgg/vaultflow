"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useAdminKey } from "@/lib/admin-key";
import { demoMerchantId } from "@/lib/env";
import { shortAddr } from "@/lib/format";
import { USE_MOCK } from "@/lib/mock";

export function AppHeader() {
  const pathname = usePathname();
  const adminKey = useAdminKey();
  const isAdmin = pathname.startsWith("/dashboard") || pathname.startsWith("/admin");
  const isCashier = pathname.startsWith("/pay/");
  const isUser = !isAdmin && !isCashier;

  return (
    <header className="sticky top-0 z-30 border-b border-slate-800/80 bg-[#06090f]/85 backdrop-blur-md">
      <div className="mx-auto flex max-w-7xl flex-wrap items-center justify-between gap-3 px-4 py-3 sm:px-6">
        <div className="flex items-center gap-3">
          <Link href="/" className="group flex items-center gap-2.5">
            <span className="flex h-8 w-8 items-center justify-center rounded-xl bg-gradient-to-br from-emerald-400 to-sky-500 font-mono text-sm font-bold text-slate-950 shadow-sm">
              VF
            </span>
            <div>
              <div className="flex items-center gap-2">
                <span className="text-base font-bold tracking-tight text-white">VaultFlow</span>
                <span className="rounded-md border border-slate-700/80 bg-slate-900 px-1.5 py-0.5 text-[10px] font-medium text-slate-400">
                  DEMO
                </span>
              </div>
              <p className="hidden text-[11px] text-slate-400 sm:block">
                多链稳定币托管与清结算网关
              </p>
            </div>
          </Link>

          <div className="hidden items-center gap-2 border-l border-slate-800 pl-3 md:flex">
            <span
              className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-[11px] font-medium ${
                USE_MOCK
                  ? "border-amber-500/35 bg-amber-500/10 text-amber-300"
                  : "border-emerald-500/35 bg-emerald-500/10 text-emerald-300"
              }`}
            >
              <span
                className={`h-1.5 w-1.5 rounded-full ${USE_MOCK ? "bg-amber-400" : "bg-emerald-400"}`}
              />
              {USE_MOCK ? "MOCK 内存引擎" : "LIVE 链上网关"}
            </span>
            <span
              className="rounded-full border border-slate-800 bg-slate-900/80 px-2.5 py-0.5 font-mono text-[11px] text-slate-400"
              title={`演示商户 UUID: ${demoMerchantId()}`}
            >
              商户 {shortAddr(demoMerchantId(), 8, 4)}
            </span>
            {adminKey.hasKey && (
              <span
                className="rounded-full border border-sky-500/30 bg-sky-500/10 px-2.5 py-0.5 font-mono text-[11px] text-sky-300"
                title={`WebCrypto Ed25519 管理员公钥: ${adminKey.publicKeyHex}`}
              >
                Ed25519 已加载
              </span>
            )}
          </div>
        </div>

        <nav className="flex items-center gap-1 rounded-xl border border-slate-800 bg-slate-950/90 p-1">
          <Link
            href="/"
            className={`inline-flex items-center gap-2 rounded-lg px-3.5 py-1.5 text-xs font-semibold transition ${
              isUser
                ? "bg-emerald-500 text-slate-950 shadow-sm"
                : "text-slate-300 hover:bg-slate-900 hover:text-white"
            }`}
          >
            <svg className="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.8">
              <rect x="2.5" y="4.5" width="15" height="11" rx="2" />
              <path d="M13 10h4.5" strokeLinecap="round" />
            </svg>
            <span>用户页 · 充提与余额</span>
          </Link>

          <Link
            href="/dashboard"
            className={`inline-flex items-center gap-2 rounded-lg px-3.5 py-1.5 text-xs font-semibold transition ${
              isAdmin
                ? "bg-sky-500 text-slate-950 shadow-sm"
                : "text-slate-300 hover:bg-slate-900 hover:text-white"
            }`}
          >
            <svg className="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.8">
              <path d="M3 4.5h6v5H3zM11 4.5h6v3H11zM11 9.5h6v6H11zM3 11.5h6v4H3z" strokeLinejoin="round" />
            </svg>
            <span>管理后台 · 账本与运维</span>
          </Link>

          {isCashier && (
            <span className="inline-flex items-center gap-1.5 rounded-lg bg-slate-800 px-3 py-1.5 text-xs font-semibold text-amber-300">
              收银台详情
            </span>
          )}
        </nav>
      </div>
    </header>
  );
}

// Authenticated app shell: VaultFlow header, 充值/资产 nav, session guard.

"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { clearSession, getEmail, getToken } from "@/lib/auth";
import { Spinner } from "./ui";

const NAV = [
  { href: "/", label: "充值" },
  { href: "/assets", label: "资产" },
];

export function Shell({ children }: { children: ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const qc = useQueryClient();
  const [authed, setAuthed] = useState<boolean | null>(null);

  useEffect(() => {
    if (!getToken()) {
      router.replace("/login");
      setAuthed(false);
    } else {
      setAuthed(true);
    }
  }, [router]);

  if (authed !== true) {
    return (
      <main className="flex min-h-screen items-center justify-center">
        <Spinner className="h-6 w-6 text-[#FCD535]" />
      </main>
    );
  }

  return (
    <main className="mx-auto min-h-screen max-w-5xl px-4 pb-16">
      <header className="flex items-center justify-between border-b border-[#2B3139] py-4">
        <div className="flex items-center gap-8">
          <h1 className="text-lg font-bold tracking-tight">
            Vault<span className="text-[#FCD535]">Flow</span>
          </h1>
          <nav className="flex items-center gap-5 text-sm">
            {NAV.map((n) => {
              const active = pathname === n.href;
              return (
                <Link
                  key={n.href}
                  href={n.href}
                  className={
                    active
                      ? "border-b-2 border-[#FCD535] pb-1 font-semibold text-[#FCD535]"
                      : "pb-1 text-[#848E9C] transition hover:text-[#EAECEF]"
                  }
                >
                  {n.label}
                </Link>
              );
            })}
          </nav>
        </div>
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
      {children}
    </main>
  );
}

"use client";

import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import * as api from "@/lib/api";
import { setSession } from "@/lib/auth";
import { btnCls, ErrorBox, inputCls } from "@/components/ui";

type Mode = "login" | "register";

export default function LoginPage() {
  const router = useRouter();
  const [mode, setMode] = useState<Mode>("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = mode === "login" ? await api.login(email, password) : await api.register(email, password);
      setSession(res.token, email);
      router.replace("/");
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="mx-auto flex min-h-screen max-w-sm flex-col justify-center px-6 py-16">
      <header className="mb-8 text-center">
        <h1 className="text-2xl font-bold tracking-tight">
          Vault<span className="text-[#FCD535]">Flow</span>
        </h1>
        <p className="mt-1 text-sm text-[#848E9C]">数字资产充值平台</p>
      </header>

      <div className="mb-6 grid grid-cols-2 rounded-lg bg-[#1E2329] p-1 text-sm font-medium">
        {(["login", "register"] as const).map((m) => (
          <button
            key={m}
            type="button"
            onClick={() => setMode(m)}
            className={`rounded-md py-2 transition ${
              mode === m ? "bg-[#FCD535] text-[#0B0E11]" : "text-[#848E9C] hover:text-[#EAECEF]"
            }`}
          >
            {m === "login" ? "登录" : "注册"}
          </button>
        ))}
      </div>

      <form onSubmit={onSubmit} className="flex flex-col gap-4">
        <input
          type="email"
          required
          autoComplete="email"
          placeholder="邮箱"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          className={inputCls}
        />
        <input
          type="password"
          required
          minLength={8}
          autoComplete={mode === "login" ? "current-password" : "new-password"}
          placeholder="密码（至少 8 位）"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          className={inputCls}
        />
        <ErrorBox error={error} />
        <button type="submit" disabled={busy} className={btnCls}>
          {busy ? "提交中…" : mode === "login" ? "登录" : "创建账户"}
        </button>
      </form>
    </main>
  );
}

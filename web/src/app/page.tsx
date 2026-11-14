import Link from "next/link";
import { DEMO_ORDER_ID, DEMO_PARTIAL_ORDER_ID } from "@/lib/mock-data";
import { USE_MOCK } from "@/lib/mock";

// The seeded demo orders only exist inside the mock engine — in LIVE mode the
// cashier URL comes from POST /v1/orders (cashier_url field), so the two demo
// cards are hidden instead of linking to guaranteed-404 pages.
const mockCashierCards = [
  {
    href: `/pay/${DEMO_ORDER_ID}`,
    title: "托管收银台（足额 USDC → PAID）",
    desc: "二维码 + ERC-20 transfer / USDC EIP-3009 免 Gas 签名 + SSE 实时确认进度",
  },
  {
    href: `/pay/${DEMO_PARTIAL_ORDER_ID}`,
    title: "托管收银台（60% 部分支付 → PARTIALLY_PAID）",
    desc: "观察 DETECTED 确认数推进与部分到账终态",
  },
];

const cards = [
  ...(USE_MOCK ? mockCashierCards : []),
  {
    href: "/dashboard",
    title: "商户与运维对账监控台",
    desc: "14 科目复式账本大盘 / 双层对账巡检 / 异常单工作台 / 出站队列 / Signer 健康",
  },
  {
    href: "/dashboard/approvals",
    title: "离线 Ed25519 审批面板",
    desc: "WebCrypto 内存密钥 · 69B 白名单租期 / 127B RefundTicket 二进制载荷签名",
  },
  {
    href: "/dashboard/simulator",
    title: "现场故障演练舱",
    desc: "浅/深分叉 LIFO 回滚 · 黑名单隔离与 ReturnBomb · Signer 越权拦截 · 分叉高度树",
  },
];

export default function HomePage() {
  return (
    <main className="mx-auto flex min-h-screen max-w-3xl flex-col justify-center gap-6 px-6 py-16">
      <header>
        <div className="flex items-center gap-3">
          <h1 className="text-3xl font-bold tracking-tight">VaultFlow</h1>
          <span
            className={`rounded-full border px-2 py-0.5 text-xs ${
              USE_MOCK
                ? "border-amber-700 bg-amber-950/50 text-amber-300"
                : "border-emerald-700 bg-emerald-950/50 text-emerald-300"
            }`}
          >
            {USE_MOCK ? "MOCK 模式" : "LIVE 模式"}
          </span>
        </div>
        <p className="mt-2 text-sm text-neutral-400">
          多链稳定币支付与托管清结算 — Next.js 收银台 + 运维对账监控台
          {USE_MOCK ? "（Mock 脱后端运行）" : "（连接真实 gateway-core + Anvil devnet）"}
        </p>
      </header>
      <nav className="grid gap-3">
        {cards.map((c) => (
          <Link
            key={c.href}
            href={c.href}
            className="group rounded-xl border border-neutral-800 bg-neutral-900/60 p-4 transition hover:border-sky-700 hover:bg-neutral-900"
          >
            <div className="font-medium text-neutral-100 group-hover:text-sky-300">{c.title} →</div>
            <div className="mt-1 text-sm text-neutral-500">{c.desc}</div>
          </Link>
        ))}
      </nav>
    </main>
  );
}

// Presentation helpers.

import type { DepositStatus } from "./types";

export const DEPOSIT_STATUS_LABEL: Record<DepositStatus, string> = {
  PENDING: "确认中",
  CONFIRMED: "已确认",
  CREDITED: "已到账",
  SWEPT: "已归集",
};

export const DEPOSIT_STATUS_TONE: Record<DepositStatus, "ok" | "warn" | "muted"> = {
  PENDING: "warn",
  CONFIRMED: "ok",
  CREDITED: "ok",
  SWEPT: "muted",
};

export function shortAddr(addr: string | null | undefined, head = 6, tail = 4): string {
  if (!addr) return "—";
  if (addr.length <= head + tail + 3) return addr;
  return `${addr.slice(0, head)}…${addr.slice(-tail)}`;
}

export function fmtDateTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("zh-CN", { hour12: false });
}

/** Rough human duration for the 充币到账时间 row, e.g. "约 2 分钟". */
export function estDuration(totalSeconds: number): string {
  if (!Number.isFinite(totalSeconds) || totalSeconds <= 0) return "约 1 分钟";
  if (totalSeconds < 3600) return `约 ${Math.max(1, Math.round(totalSeconds / 60))} 分钟`;
  return `约 ${(totalSeconds / 3600).toFixed(1)} 小时`;
}

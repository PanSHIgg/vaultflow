// Presentation helpers: zh labels, colors, truncation, timestamps.

import type {
  LedgerAccountType,
  MerchantPayoutStatus,
  OutboundTxIntentStatus,
  PaymentOrderStatus,
  WithdrawalAddressStatus,
} from "./types";

export const ORDER_STATUS_LABEL: Record<PaymentOrderStatus, string> = {
  AWAITING_PAYMENT: "待支付",
  PARTIALLY_PAID: "部分到账",
  PAID: "已支付",
  OVERPAID: "超额支付",
  EXPIRED: "已过期",
  UNDERPAID_EXPIRED_REVIEW: "少付超时·待审核",
  LATE_PAID_REVIEW: "迟付·待审核",
  REFUND_IN_PROGRESS: "退款执行中",
  REFUNDED: "已退款",
  SETTLED_REVERSED: "已结算被回滚",
  REVERSAL_RESOLVED: "回滚已核销",
};

export const ORDER_STATUS_TONE: Record<PaymentOrderStatus, "ok" | "warn" | "bad" | "info" | "muted"> = {
  AWAITING_PAYMENT: "info",
  PARTIALLY_PAID: "warn",
  PAID: "ok",
  OVERPAID: "warn",
  EXPIRED: "muted",
  UNDERPAID_EXPIRED_REVIEW: "bad",
  LATE_PAID_REVIEW: "bad",
  REFUND_IN_PROGRESS: "warn",
  REFUNDED: "muted",
  SETTLED_REVERSED: "bad",
  REVERSAL_RESOLVED: "muted",
};

export const PAYOUT_STATUS_LABEL: Record<MerchantPayoutStatus, string> = {
  LOCKED_QUEUED: "已锁定排队",
  IN_FLIGHT: "链上在途",
  COMPLETED: "已完成",
  FAILED_RETRYABLE: "失败可重试",
  CANCELLED_UNLOCKED: "已取消解冻",
};

export const INTENT_STATUS_LABEL: Record<OutboundTxIntentStatus, string> = {
  QUEUED: "排队中",
  IN_FLIGHT: "在途",
  INCLUDED_PENDING_SAFE: "已入块待安全高度",
  CONFIRMED: "已确认",
  FAILED_REVERTED: "链上 Revert",
  CANCELLED_BY_FILLER: "已被 Filler 取消",
  NONCE_STOLEN_HALT: "Nonce 被盗用·熔断",
};

export const WHITELIST_STATUS_LABEL: Record<WithdrawalAddressStatus, string> = {
  PENDING_COOLDOWN: "24h 冷却中",
  ACTIVE: "已激活",
  EXPIRED: "租期已过",
  REVOKED: "已吊销",
};

export const ACCOUNT_TYPE_LABEL: Record<LedgerAccountType, string> = {
  CUSTODY_UNSWEPT: "待归集托管",
  CUSTODY_COLD: "冷金库托管",
  CUSTODY_HOT_PAYOUT: "提现热钱包备付",
  HOT_WALLET_NATIVE: "热钱包原生 Gas",
  GAS_EXPENSE: "Gas 累计消耗",
  MERCHANT_REORG_RECEIVABLE: "商户回滚应收",
  ORDER_ESCROW_LIAB: "订单在途托管",
  MERCHANT_AVAILABLE: "商户可用余额",
  PAYOUT_LOCKED_LIAB: "提现冻结",
  SUSPENSE_OVERPAY_LIAB: "溢缴暂挂",
  SUSPENSE_REFUNDABLE_LIAB: "可退暂挂",
  SUSPENSE_UNMATCHED_LIAB: "异币误充暂挂",
  TREASURY_EQUITY_CAPITAL: "平台权益资本",
  FEE_REVENUE: "手续费收入",
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

export function fmtCountdown(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const mm = String(m).padStart(2, "0");
  const ss = String(sec).padStart(2, "0");
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
}

export function fmtUnix(unixSec: number | bigint | null | undefined): string {
  if (unixSec == null) return "—";
  return fmtDateTime(new Date(Number(unixSec) * 1000).toISOString());
}

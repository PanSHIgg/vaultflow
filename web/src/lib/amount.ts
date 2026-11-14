// W6-1 — NUMERIC(78,0) decimal-string <-> BigInt utilities.
// Frozen contract (AGENTS.md §2.5.1 / openapi Amount): every amount on the wire
// is a decimal string matching ^(0|[1-9][0-9]{0,77})$ — JSON numbers and
// Number-typed arithmetic are forbidden. All parsing/formatting here is pure
// string/BigInt math; no Number is ever used on amount values.

const AMOUNT_RE = /^(0|[1-9][0-9]{0,77})$/;

/** True iff `value` is a canonical NUMERIC(78,0) decimal string. */
export function isValidAmountString(value: string): boolean {
  return AMOUNT_RE.test(value);
}

/**
 * Parse a canonical decimal amount string to BigInt.
 * Throws on anything that is not a non-negative integer decimal string of at
 * most 78 digits (no leading zeros, no signs, no whitespace).
 */
export function parseAmount(value: string | bigint): bigint {
  if (typeof value === "bigint") {
    if (value < 0n) throw new Error("amount must be >= 0");
    if (value.toString().length > 78) throw new Error("amount exceeds NUMERIC(78,0)");
    return value;
  }
  if (!AMOUNT_RE.test(value)) {
    throw new Error(`invalid NUMERIC(78,0) amount string: ${JSON.stringify(value)}`);
  }
  return BigInt(value);
}

/** Safe variant of parseAmount — returns null instead of throwing. */
export function tryParseAmount(value: string | null | undefined): bigint | null {
  if (value == null || !AMOUNT_RE.test(value)) return null;
  return BigInt(value);
}

/** a < b => -1, a == b => 0, a > b => 1. */
export function cmpAmount(a: bigint, b: bigint): -1 | 0 | 1 {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * Format an atomic-unit BigInt with `decimals` decimal places.
 * Trailing fractional zeros are trimmed; no scientific notation ever.
 * formatUnits(1500000n, 6) === "1.5"; formatUnits(0n, 6) === "0".
 */
export function formatUnits(amount: bigint, decimals: number): string {
  if (amount < 0n) throw new Error("amount must be >= 0");
  if (!Number.isInteger(decimals) || decimals < 0 || decimals > 78) {
    throw new Error(`invalid decimals: ${decimals}`);
  }
  if (decimals === 0) return amount.toString();
  const s = amount.toString().padStart(decimals + 1, "0");
  const intPart = s.slice(0, s.length - decimals);
  const fracPart = s.slice(s.length - decimals).replace(/0+$/, "");
  return fracPart === "" ? intPart : `${intPart}.${fracPart}`;
}

const PARSE_UNITS_RE = /^([0-9]+)(?:\.([0-9]+))?$/;

/**
 * Parse a human-entered decimal token amount into atomic units.
 * Inverse of formatUnits. Rejects scientific notation, negatives, commas,
 * excess fractional precision.
 */
export function parseUnits(text: string, decimals: number): bigint {
  const trimmed = text.trim();
  const m = PARSE_UNITS_RE.exec(trimmed);
  if (!m) throw new Error(`invalid decimal amount: ${JSON.stringify(text)}`);
  const intPart = m[1].replace(/^0+(?=\d)/, "");
  const fracPart = (m[2] ?? "").replace(/0+$/, "");
  if (fracPart.length > decimals) {
    throw new Error(`too many decimal places (max ${decimals})`);
  }
  const atomic = BigInt(intPart) * 10n ** BigInt(decimals) + BigInt(fracPart === "" ? "0" : fracPart.padEnd(decimals, "0"));
  if (atomic.toString().length > 78) throw new Error("amount exceeds NUMERIC(78,0)");
  return atomic;
}

/** Insert thousands separators into an integer decimal string ("1234567" -> "1,234,567"). */
export function groupInteger(intDigits: string): string {
  const neg = intDigits.startsWith("-");
  const digits = neg ? intDigits.slice(1) : intDigits;
  const out: string[] = [];
  for (let i = digits.length; i > 0; i -= 3) {
    out.unshift(digits.slice(Math.max(0, i - 3), i));
  }
  return (neg ? "-" : "") + out.join(",");
}

export interface FormatTokenOptions {
  /** Token symbol suffix, e.g. "USDC". */
  symbol?: string;
  /** Max fractional digits to display (rest truncated, not rounded). */
  maxFrac?: number;
  /** Insert thousands separators (default true). */
  group?: boolean;
}

/**
 * Human display of an atomic amount: "1,234.5 USDC".
 * Pure string math — safe for the full uint256 range.
 */
export function formatTokenAmount(
  amount: string | bigint,
  decimals: number,
  opts: FormatTokenOptions = {},
): string {
  const v = parseAmount(amount);
  let [intPart, fracPart] = formatUnits(v, decimals).split(".") as [string, string?];
  if (opts.maxFrac != null && fracPart != null && fracPart.length > opts.maxFrac) {
    fracPart = fracPart.slice(0, opts.maxFrac).replace(/0+$/, "");
  }
  if (opts.group !== false) intPart = groupInteger(intPart);
  const num = fracPart ? `${intPart}.${fracPart}` : intPart;
  return opts.symbol ? `${num} ${opts.symbol}` : num;
}

/**
 * Compact display for dashboards: keeps up to `maxFrac` fraction digits and
 * appends a symbol. Falls back to the raw atomic string when decimals unknown.
 */
export function shortTokenAmount(amount: string | bigint, decimals: number, symbol?: string): string {
  return formatTokenAmount(amount, decimals, { symbol, maxFrac: 4 });
}

/** Sum of decimal-string amounts (variadic) as BigInt. */
export function sumAmounts(...values: Array<string | bigint>): bigint {
  let acc = 0n;
  for (const v of values) acc += parseAmount(v);
  return acc;
}

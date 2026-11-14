// W6-1 acceptance: BigInt decimal-string amount utilities — no Number, full
// NUMERIC(78,0) / uint256 range, strict validation.

import { describe, expect, it } from "vitest";
import {
  cmpAmount,
  formatTokenAmount,
  formatUnits,
  groupInteger,
  isValidAmountString,
  parseAmount,
  parseUnits,
  sumAmounts,
  tryParseAmount,
} from "@/lib/amount";

describe("parseAmount / isValidAmountString", () => {
  it("accepts canonical decimal strings", () => {
    expect(parseAmount("0")).toBe(0n);
    expect(parseAmount("1500000")).toBe(1500000n);
    // 78-digit max (uint256 ceiling ~1.1579e77)
    expect(parseAmount("9".repeat(78))).toBe(BigInt("9".repeat(78)));
  });

  it("rejects non-canonical strings", () => {
    for (const bad of ["", "01", " 1", "1 ", "-1", "+1", "1.0", "1e3", "0x10", "abc", "1_000"]) {
      expect(isValidAmountString(bad), bad).toBe(false);
      expect(() => parseAmount(bad), bad).toThrow();
    }
  });

  it("rejects 79-digit overflow", () => {
    expect(isValidAmountString("1" + "0".repeat(78))).toBe(false);
    expect(() => parseAmount("1" + "0".repeat(78))).toThrow();
  });

  it("accepts bigint passthrough but rejects negative/oversize", () => {
    expect(parseAmount(42n)).toBe(42n);
    expect(() => parseAmount(-1n)).toThrow();
    expect(() => parseAmount(BigInt("1" + "0".repeat(78)))).toThrow();
  });

  it("tryParseAmount is null-safe", () => {
    expect(tryParseAmount("100")).toBe(100n);
    expect(tryParseAmount("x")).toBeNull();
    expect(tryParseAmount(null)).toBeNull();
    expect(tryParseAmount(undefined)).toBeNull();
  });
});

describe("formatUnits / parseUnits (inverse pair)", () => {
  it("formats atomic units with decimal places and trims zeros", () => {
    expect(formatUnits(1500000n, 6)).toBe("1.5");
    expect(formatUnits(1000000n, 6)).toBe("1");
    expect(formatUnits(1n, 6)).toBe("0.000001");
    expect(formatUnits(0n, 6)).toBe("0");
    expect(formatUnits(123456789n, 6)).toBe("123.456789");
    expect(formatUnits(5n, 0)).toBe("5");
    expect(formatUnits(512000000000000000n, 18)).toBe("0.512");
  });

  it("handles the uint256 ceiling without precision loss", () => {
    const max = BigInt("115792089237316195423570985008687907853269984665640564039457584007913129639935");
    const s = formatUnits(max, 18);
    expect(s.startsWith("115792089237316195423570985008687907853269984665640564039457")).toBe(true);
    expect(parseUnits(s.split(".")[0], 0)).toBe(max / 10n ** 18n);
  });

  it("parseUnits is the strict inverse", () => {
    expect(parseUnits("1.5", 6)).toBe(1500000n);
    expect(parseUnits("0.000001", 6)).toBe(1n);
    expect(parseUnits("123", 6)).toBe(123000000n);
    expect(parseUnits("1.000000", 6)).toBe(1000000n);
    expect(parseUnits("007.5", 6)).toBe(7500000n);
  });
});

describe("parseUnits strictness", () => {
  it("rejects over-precision and junk", () => {
    expect(() => parseUnits("1.0000001", 6)).toThrow();
    expect(() => parseUnits("-1", 6)).toThrow();
    expect(() => parseUnits("1,000", 6)).toThrow();
    expect(() => parseUnits("1e-6", 6)).toThrow();
    expect(() => parseUnits(".5", 6)).toThrow();
    expect(() => parseUnits("", 6)).toThrow();
  });
});

describe("formatTokenAmount / groupInteger", () => {
  it("groups thousands on the integer part only", () => {
    expect(groupInteger("1234567890")).toBe("1,234,567,890");
    expect(groupInteger("0")).toBe("0");
    expect(groupInteger("999")).toBe("999");
    expect(formatTokenAmount("1234567890123", 6, { symbol: "USDC" })).toBe("1,234,567.890123 USDC");
    expect(formatTokenAmount("1500000", 6, { symbol: "USDC" })).toBe("1.5 USDC");
  });

  it("maxFrac truncates without rounding", () => {
    expect(formatTokenAmount("123456789", 6, { maxFrac: 2 })).toBe("123.45");
    expect(formatTokenAmount("100000001", 6, { maxFrac: 6 })).toBe("100.000001");
    expect(formatTokenAmount("1999999", 6, { maxFrac: 1 })).toBe("1.9");
  });
});

describe("cmpAmount / sumAmounts", () => {
  it("compares and sums as BigInt", () => {
    expect(cmpAmount(1n, 2n)).toBe(-1);
    expect(cmpAmount(2n, 2n)).toBe(0);
    expect(cmpAmount(3n, 2n)).toBe(1);
    expect(sumAmounts("1", "2", 3n)).toBe(6n);
    expect(sumAmounts("1500000000", "5000000000", "800000000")).toBe(7300000000n);
  });
});

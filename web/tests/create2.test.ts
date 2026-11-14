// W6-1/W6-2 acceptance: salt / auth_nonce / init_code_hash / vault_address
// derivations must match docs/GOLDEN_VECTORS.json byte-for-byte. The bytes16
// right-padding subtlety is exercised by feeding UUID text and raw16 hex forms
// of the same ids — both must produce the vector.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  buildInitCode,
  deriveAuthNonce,
  deriveSalt,
  deriveVaultAddress,
  initCodeHash,
  toBytes16Hex,
} from "@/lib/create2";

const gv = JSON.parse(
  readFileSync(fileURLToPath(new URL("../../docs/GOLDEN_VECTORS.json", import.meta.url)), "utf8"),
) as {
  canonical_addresses: { implementation: string; factory: string };
  vectors: Array<{
    name: string;
    chain_id: number;
    merchant_id: string;
    order_id: string;
    salt: string;
    auth_nonce: string;
    init_code: string;
    init_code_hash: string;
    vault_address: string;
  }>;
};

const IMPL = gv.canonical_addresses.implementation as `0x${string}`;
const FACTORY = gv.canonical_addresses.factory as `0x${string}`;

function toUuid(raw16: string): string {
  const h = raw16.replace(/^0x/, "");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

describe("EIP-1167 init_code template", () => {
  it("builds the frozen 55-byte creation code", () => {
    const code = buildInitCode(IMPL);
    expect(code).toBe(gv.vectors[0].init_code);
    expect((code.length - 2) / 2).toBe(55);
    // runtime segment = last 45 bytes
    expect(code.slice(2 + 20)).toBe(gv.vectors[0].init_code.slice(22));
    expect(initCodeHash(IMPL)).toBe(gv.vectors[0].init_code_hash);
  });

  it("rejects malformed implementation addresses", () => {
    expect(() => buildInitCode("0x1234" as `0x${string}`)).toThrow();
  });
});

describe("toBytes16Hex", () => {
  it("accepts raw16 hex and UUID text identically", () => {
    const raw = "0x00112233445566778899aabbccddeeff";
    const uuid = "00112233-4455-6677-8899-aabbccddeeff";
    expect(toBytes16Hex(raw)).toBe(raw);
    expect(toBytes16Hex(uuid)).toBe(raw);
    expect(() => toBytes16Hex("0x1234")).toThrow();
  });
});

describe.each(gv.vectors)("vector $name", (v) => {
  it("derives salt (bytes16 RIGHT-padded abi.encode)", () => {
    // raw16 hex form
    expect(deriveSalt(v.chain_id, v.merchant_id, v.order_id)).toBe(v.salt);
    // hyphenated UUID form of the same raw16 must give the identical salt
    expect(deriveSalt(v.chain_id, toUuid(v.merchant_id), toUuid(v.order_id))).toBe(v.salt);
  });

  it("derives auth_nonce = keccak256(abi.encode(chainId, salt))", () => {
    expect(deriveAuthNonce(v.chain_id, v.salt as `0x${string}`)).toBe(v.auth_nonce);
  });

  it("derives vault CREATE2 address", () => {
    const addr = deriveVaultAddress(FACTORY, v.salt as `0x${string}`, v.init_code_hash as `0x${string}`);
    expect(addr.toLowerCase()).toBe(v.vault_address);
  });
});

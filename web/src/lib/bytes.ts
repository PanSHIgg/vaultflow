// W6-1 — byte-level helpers shared by the Ed25519 payload builders
// (AGENTS.md §2.3) and the CREATE2 derivations (§2.1). All encodings are
// fixed-width big-endian binary; no JSON/serialization ambiguity allowed.

const HEX_RE = /^(0x)?[0-9a-fA-F]*$/;

/** Decode a hex string (optional 0x prefix, even length) to bytes. */
export function hexToBytes(hex: string): Uint8Array {
  if (!HEX_RE.test(hex)) throw new Error(`invalid hex: ${JSON.stringify(hex).slice(0, 80)}`);
  const h = hex.startsWith("0x") ? hex.slice(2) : hex;
  if (h.length % 2 !== 0) throw new Error(`odd-length hex: ${hex.length} chars`);
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(h.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

/** Decode hex and assert an exact byte length (e.g. 20 for an EVM address). */
export function hexToBytesN(hex: string, byteLen: number, what = "value"): Uint8Array {
  const b = hexToBytes(hex);
  if (b.length !== byteLen) {
    throw new Error(`${what} must be ${byteLen} bytes, got ${b.length}`);
  }
  return b;
}

/** Lowercase hex WITHOUT 0x prefix (canonical for payload previews). */
export function bytesToHex(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += b.toString(16).padStart(2, "0");
  return s;
}

/** Lowercase hex WITH 0x prefix (canonical wire format, AGENTS.md §2.5.3). */
export function bytesToHex0x(bytes: Uint8Array): `0x${string}` {
  return `0x${bytesToHex(bytes)}`;
}

export function utf8(s: string): Uint8Array {
  return new TextEncoder().encode(s);
}

export function concatBytes(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

const UUID_RE =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** Validate canonical hyphenated UUID text (case-insensitive). */
export function isUuid(s: unknown): s is string {
  return typeof s === "string" && UUID_RE.test(s);
}

/** UUID text ("00112233-…-aabbccddeeff") -> raw 16 bytes (hyphens stripped). */
export function uuidToBytes16(uuid: string): Uint8Array {
  if (!UUID_RE.test(uuid)) throw new Error(`invalid UUID: ${JSON.stringify(uuid)}`);
  return hexToBytes(uuid.replace(/-/g, ""));
}

/** raw 16 bytes -> canonical hyphenated lowercase UUID text. */
export function bytes16ToUuid(b: Uint8Array): string {
  if (b.length !== 16) throw new Error(`bytes16 must be 16 bytes, got ${b.length}`);
  const h = bytesToHex(b);
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/** Write `value` as a big-endian unsigned integer of `byteLen` bytes at `offset`. */
export function writeUintBE(
  out: Uint8Array,
  offset: number,
  value: bigint,
  byteLen: number,
): void {
  if (value < 0n) throw new Error("negative value not allowed");
  let v = value;
  for (let i = byteLen - 1; i >= 0; i--) {
    out[offset + i] = Number(v & 0xffn);
    v >>= 8n;
  }
  if (v !== 0n) throw new Error(`value ${value} does not fit in ${byteLen} bytes`);
}

export function u64be(value: bigint): Uint8Array {
  const out = new Uint8Array(8);
  writeUintBE(out, 0, value, 8);
  return out;
}

export function u32be(value: bigint): Uint8Array {
  const out = new Uint8Array(4);
  writeUintBE(out, 0, value, 4);
  return out;
}

export function u256be(value: bigint): Uint8Array {
  const out = new Uint8Array(32);
  writeUintBE(out, 0, value, 32);
  return out;
}

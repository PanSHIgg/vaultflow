// W6-3 — Browser WebCrypto offline Ed25519 admin approval panel.
// Payload layout is FROZEN (AGENTS.md §2.3 / docs/GOLDEN_VECTORS.json):
//
//   ADDR_V1   69B  = "VAULTFLOW_ADDR_V1"(17B)   || chain_id u64BE || merchant_uuid raw16
//                    || recipient bytes20 || valid_until u64BE
//   REFUND_V1 127B = "VAULTFLOW_REFUND_V1"(19B) || chain_id u64BE || token bytes20
//                    || to bytes20 || exact_amount u256BE || payout_uuid raw16
//                    || retry_seq u32BE || expiry u64BE
//
// The private key never leaves browser memory (extractable only when the caller
// explicitly asks to import a seed; generation is non-extractable by default —
// see generateAdminKeyPair options).

import {
  bytesToHex,
  bytesToHex0x,
  concatBytes,
  hexToBytes,
  hexToBytesN,
  u32be,
  u64be,
  u256be,
  utf8,
  uuidToBytes16,
  writeUintBE,
} from "./bytes";

export const ADDR_V1_MAGIC = "VAULTFLOW_ADDR_V1"; // 17 ASCII bytes
export const REFUND_V1_MAGIC = "VAULTFLOW_REFUND_V1"; // 19 ASCII bytes
export const ADDR_V1_PAYLOAD_BYTES = 69;
export const REFUND_V1_PAYLOAD_BYTES = 127;

const ADDR_MAGIC_BYTES = utf8(ADDR_V1_MAGIC); // 17
const REFUND_MAGIC_BYTES = utf8(REFUND_V1_MAGIC); // 19
const ZERO_UUID = "00000000-0000-0000-0000-000000000000";

export interface AddrV1Fields {
  chainId: bigint;
  /** Hyphenated UUID text -> raw16 (e.g. "00112233-4455-6677-8899-aabbccddeeff"). */
  merchantUuid: string;
  /** 0x-prefixed 20-byte EVM address. */
  recipientAddress: string;
  validUntilUnix: bigint;
}

export interface RefundV1Fields {
  chainId: bigint;
  tokenAddress: string;
  toAddress: string;
  /** Atomic units; must equal the wire `amount` field (full-ticket refund). */
  exactAmount: bigint;
  payoutUuid: string;
  retrySeq: number | bigint;
  expiryUnix: bigint;
}

/**
 * Assemble the frozen 69-byte whitelist-lease payload. Throws if any field is
 * malformed or the result is not exactly 69 bytes.
 */
export function buildAddrV1Payload(f: AddrV1Fields): Uint8Array {
  if (ADDR_MAGIC_BYTES.length !== 17) throw new Error("magic constant corrupt");
  const out = new Uint8Array(ADDR_V1_PAYLOAD_BYTES);
  out.set(ADDR_MAGIC_BYTES, 0);
  writeUintBE(out, 17, f.chainId, 8);
  out.set(uuidToBytes16(f.merchantUuid || ZERO_UUID), 25);
  out.set(hexToBytesN(f.recipientAddress, 20, "recipient_address"), 41);
  writeUintBE(out, 61, f.validUntilUnix, 8);
  if (out.length !== ADDR_V1_PAYLOAD_BYTES) {
    throw new Error(`ADDR_V1 payload must be 69 bytes, got ${out.length}`);
  }
  return out;
}

/**
 * Assemble the frozen 127-byte RefundTicket payload. Throws if any field is
 * malformed or the result is not exactly 127 bytes.
 */
export function buildRefundV1Payload(f: RefundV1Fields): Uint8Array {
  if (REFUND_MAGIC_BYTES.length !== 19) throw new Error("magic constant corrupt");
  const out = new Uint8Array(REFUND_V1_PAYLOAD_BYTES);
  out.set(REFUND_MAGIC_BYTES, 0);
  writeUintBE(out, 19, f.chainId, 8);
  out.set(hexToBytesN(f.tokenAddress, 20, "token_address"), 27);
  out.set(hexToBytesN(f.toAddress, 20, "to_address"), 47);
  out.set(u256be(f.exactAmount), 67);
  out.set(uuidToBytes16(f.payoutUuid || ZERO_UUID), 99);
  out.set(u32be(BigInt(f.retrySeq)), 115);
  writeUintBE(out, 119, f.expiryUnix, 8);
  if (out.length !== REFUND_V1_PAYLOAD_BYTES) {
    throw new Error(`REFUND_V1 payload must be 127 bytes, got ${out.length}`);
  }
  return out;
}

// ---------------------------------------------------------------------------
// WebCrypto Ed25519 key handling (Node 22 / modern Chrome support `Ed25519`)
// ---------------------------------------------------------------------------

export const ED25519_ALGO = "Ed25519";

// PKCS#8 DER prefix for a 32-byte Ed25519 seed:
//   SEQUENCE(46) { INTEGER 0, SEQUENCE{ OID 1.3.101.112 }, OCTET STRING{ OCTET STRING{ seed } } }
const ED25519_PKCS8_PREFIX = hexToBytes("302e020100300506032b657004220420");
// SubjectPublicKeyInfo DER prefix for a raw 32-byte Ed25519 public key.
const ED25519_SPKI_PREFIX = hexToBytes("302a300506032b6570032100");

function subtle(): SubtleCrypto {
  const s = globalThis.crypto?.subtle;
  if (!s) throw new Error("WebCrypto subtle API unavailable (needs secure context / Node >= 20)");
  return s;
}

export function seedToPkcs8(seed: Uint8Array): Uint8Array {
  if (seed.length !== 32) throw new Error(`Ed25519 seed must be 32 bytes, got ${seed.length}`);
  return concatBytes(ED25519_PKCS8_PREFIX, seed);
}

export function rawPublicKeyToSpki(raw: Uint8Array): Uint8Array {
  if (raw.length !== 32) throw new Error(`Ed25519 public key must be 32 bytes, got ${raw.length}`);
  return concatBytes(ED25519_SPKI_PREFIX, raw);
}

/** Probe whether this runtime actually implements Ed25519 in WebCrypto. */
export async function ed25519Available(): Promise<boolean> {
  try {
    await subtle().generateKey({ name: ED25519_ALGO } as EcKeyGenParams, false, ["sign", "verify"]);
    return true;
  } catch {
    return false;
  }
}

export interface AdminKeyPair {
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  /** 0x-prefixed lowercase 64-hex raw public key. */
  publicKeyHex: `0x${string}`;
}

/** Generate a fresh admin keypair fully in memory (never persisted). */
export async function generateAdminKeyPair(extractable = false): Promise<AdminKeyPair> {
  const kp = (await subtle().generateKey({ name: ED25519_ALGO } as EcKeyGenParams, extractable, [
    "sign",
    "verify",
  ])) as CryptoKeyPair;
  return finalizeKeyPair(kp.privateKey!, kp.publicKey!);
}

/**
 * Import an admin keypair from a raw 32-byte seed (hex). The seed is wrapped in
 * a PKCS#8 envelope locally — nothing is transmitted anywhere.
 */
export async function importAdminSeed(seedHex: string): Promise<AdminKeyPair> {
  const seed = hexToBytesN(seedHex, 32, "Ed25519 seed");
  const pkcs8 = seedToPkcs8(seed);
  const privateKey = await subtle().importKey(
    "pkcs8",
    pkcs8 as BufferSource,
    { name: ED25519_ALGO } as EcKeyImportParams,
    true, // extractable only to derive the public half via JWK below; stays in-memory
    ["sign"],
  );
  const publicKey = await derivePublicKey(privateKey);
  return finalizeKeyPair(privateKey, publicKey);
}

/** Export a CryptoKey public half to raw 32 bytes. */
export async function exportRawPublicKey(publicKey: CryptoKey): Promise<Uint8Array> {
  return new Uint8Array(await subtle().exportKey("raw", publicKey));
}

/** Derive the public CryptoKey from an extractable private CryptoKey (JWK x). */
export async function derivePublicKey(privateKey: CryptoKey): Promise<CryptoKey> {
  const jwk = (await subtle().exportKey("jwk", privateKey)) as JsonWebKey;
  if (!jwk.x) {
    throw new Error("runtime cannot export Ed25519 public component (jwk.x missing)");
  }
  return subtle().importKey("jwk", { kty: "OKP", crv: ED25519_ALGO, x: jwk.x }, { name: ED25519_ALGO } as EcKeyImportParams, true, ["verify"]);
}

/** Import a raw 32-byte public key (hex) for offline verification. */
export async function importRawPublicKey(rawHex: string): Promise<CryptoKey> {
  const raw = hexToBytesN(rawHex, 32, "Ed25519 public key");
  return subtle().importKey("raw", raw as BufferSource, { name: ED25519_ALGO } as EcKeyImportParams, false, ["verify"]);
}

async function finalizeKeyPair(privateKey: CryptoKey, publicKey: CryptoKey): Promise<AdminKeyPair> {
  const raw = await exportRawPublicKey(publicKey);
  return { privateKey, publicKey, publicKeyHex: bytesToHex0x(raw) };
}

/** Sign a binary payload — returns the raw 64-byte RFC 8032 signature. */
export async function signPayload(privateKey: CryptoKey, payload: Uint8Array): Promise<Uint8Array> {
  const sig = await subtle().sign({ name: ED25519_ALGO }, privateKey, payload as BufferSource);
  const out = new Uint8Array(sig);
  if (out.length !== 64) throw new Error(`Ed25519 signature must be 64 bytes, got ${out.length}`);
  return out;
}

/** Verify a raw 64-byte signature against a payload. */
export async function verifyPayload(
  publicKey: CryptoKey,
  signature: Uint8Array,
  payload: Uint8Array,
): Promise<boolean> {
  return subtle().verify({ name: ED25519_ALGO }, publicKey, signature as BufferSource, payload as BufferSource);
}

/** Convenience: sign + hex encode. */
export async function signPayloadHex(privateKey: CryptoKey, payload: Uint8Array): Promise<`0x${string}`> {
  return bytesToHex0x(await signPayload(privateKey, payload));
}

export { bytesToHex, bytesToHex0x, concatBytes, hexToBytes, u64be };

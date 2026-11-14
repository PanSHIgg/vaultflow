// W6-3 acceptance: WebCrypto Ed25519 panel must assemble the frozen
// 69B ADDR_V1 / 127B REFUND_V1 payloads byte-for-byte and produce signatures
// matching docs/GOLDEN_VECTORS.json (Ed25519 is deterministic — same seed,
// same payload => identical signature).

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { bytesToHex, bytesToHex0x, hexToBytes } from "@/lib/bytes";
import {
  ADDR_V1_PAYLOAD_BYTES,
  buildAddrV1Payload,
  buildRefundV1Payload,
  importAdminSeed,
  importRawPublicKey,
  REFUND_V1_PAYLOAD_BYTES,
  seedToPkcs8,
  signPayload,
  verifyPayload,
} from "@/lib/ed25519";

const vectors = JSON.parse(
  readFileSync(fileURLToPath(new URL("../../docs/GOLDEN_VECTORS.json", import.meta.url)), "utf8"),
) as {
  ed25519_admin: {
    private_seed_hex: string;
    public_key_hex: string;
    addr_v1: { payload_hex: string; payload_bytes: number; signature_hex: string };
    refund_v1: { payload_hex: string; payload_bytes: number; signature_hex: string };
  };
};

const SEED = vectors.ed25519_admin.private_seed_hex;
const PUBKEY = vectors.ed25519_admin.public_key_hex;

// Field decomposition of the frozen vector payloads (decoded by hand, then
// re-assembled byte-for-byte below — that IS the test).
const CHAIN_ID = 31337n;
const MERCHANT_UUID = "00112233-4455-6677-8899-aabbccddeeff";
const RECIPIENT = "0x3333333333333333333333333333333333333333";
const VALID_UNTIL = BigInt("0x70dbd880");
const TOKEN = "0x4444444444444444444444444444444444444444";
const REFUND_TO = "0x5555555555555555555555555555555555555555";
const EXACT_AMOUNT = 1500000n;
const PAYOUT_UUID = "ffeeddcc-bbaa-9988-7766-554433221100";
const RETRY_SEQ = 0n;
const EXPIRY = BigInt("0x70dbd880");

describe("ADDR_V1 69-byte payload", () => {
  it("assembles byte-for-byte per the golden vector", () => {
    const p = buildAddrV1Payload({
      chainId: CHAIN_ID,
      merchantUuid: MERCHANT_UUID,
      recipientAddress: RECIPIENT,
      validUntilUnix: VALID_UNTIL,
    });
    expect(p.length).toBe(ADDR_V1_PAYLOAD_BYTES);
    expect(vectors.ed25519_admin.addr_v1.payload_bytes).toBe(69);
    expect(`0x${bytesToHex(p)}`).toBe(vectors.ed25519_admin.addr_v1.payload_hex);
  });

  it("layout offsets: magic@0(17B), chain@17(8B BE), uuid@25(16B), addr@41(20B), valid_until@61(8B BE)", () => {
    const p = buildAddrV1Payload({
      chainId: CHAIN_ID,
      merchantUuid: MERCHANT_UUID,
      recipientAddress: RECIPIENT,
      validUntilUnix: VALID_UNTIL,
    });
    expect(bytesToHex(p.slice(0, 17))).toBe(bytesToHex(new TextEncoder().encode("VAULTFLOW_ADDR_V1")));
    expect(bytesToHex(p.slice(17, 25))).toBe("0000000000007a69"); // 31337 BE
    expect(bytesToHex(p.slice(25, 41))).toBe("00112233445566778899aabbccddeeff");
    expect(bytesToHex(p.slice(41, 61))).toBe("33".repeat(20));
    expect(bytesToHex(p.slice(61, 69))).toBe("0000000070dbd880");
  });
});

describe("REFUND_V1 127-byte payload", () => {
  it("assembles byte-for-byte per the golden vector", () => {
    const p = buildRefundV1Payload({
      chainId: CHAIN_ID,
      tokenAddress: TOKEN,
      toAddress: REFUND_TO,
      exactAmount: EXACT_AMOUNT,
      payoutUuid: PAYOUT_UUID,
      retrySeq: RETRY_SEQ,
      expiryUnix: EXPIRY,
    });
    expect(p.length).toBe(REFUND_V1_PAYLOAD_BYTES);
    expect(vectors.ed25519_admin.refund_v1.payload_bytes).toBe(127);
    expect(`0x${bytesToHex(p)}`).toBe(vectors.ed25519_admin.refund_v1.payload_hex);
  });

  it("layout offsets incl. u256BE amount and u32BE retry_seq", () => {
    const p = buildRefundV1Payload({
      chainId: CHAIN_ID,
      tokenAddress: TOKEN,
      toAddress: REFUND_TO,
      exactAmount: EXACT_AMOUNT,
      payoutUuid: PAYOUT_UUID,
      retrySeq: RETRY_SEQ,
      expiryUnix: EXPIRY,
    });
    expect(bytesToHex(p.slice(0, 19))).toBe(bytesToHex(new TextEncoder().encode("VAULTFLOW_REFUND_V1")));
    expect(bytesToHex(p.slice(19, 27))).toBe("0000000000007a69");
    expect(bytesToHex(p.slice(27, 47))).toBe("44".repeat(20));
    expect(bytesToHex(p.slice(47, 67))).toBe("55".repeat(20));
    expect(bytesToHex(p.slice(67, 99))).toBe("0".repeat(58) + "16e360"); // 1500000 u256BE
    expect(bytesToHex(p.slice(99, 115))).toBe("ffeeddccbbaa99887766554433221100");
    expect(bytesToHex(p.slice(115, 119))).toBe("00000000");
    expect(bytesToHex(p.slice(119, 127))).toBe("0000000070dbd880");
  });
});

describe("WebCrypto Ed25519 sign/verify vs golden vectors", () => {
  it("derives the golden public key from the 32B seed", async () => {
    const kp = await importAdminSeed(SEED);
    expect(kp.publicKeyHex).toBe(PUBKEY);
  });

  it("signs the 69B payload => exact golden signature", async () => {
    const kp = await importAdminSeed(SEED);
    const payload = buildAddrV1Payload({
      chainId: CHAIN_ID,
      merchantUuid: MERCHANT_UUID,
      recipientAddress: RECIPIENT,
      validUntilUnix: VALID_UNTIL,
    });
    const sig = await signPayload(kp.privateKey, payload);
    expect(sig.length).toBe(64);
    expect(bytesToHex0x(sig)).toBe(vectors.ed25519_admin.addr_v1.signature_hex);
  });

  it("signs the 127B payload => exact golden signature", async () => {
    const kp = await importAdminSeed(SEED);
    const payload = buildRefundV1Payload({
      chainId: CHAIN_ID,
      tokenAddress: TOKEN,
      toAddress: REFUND_TO,
      exactAmount: EXACT_AMOUNT,
      payoutUuid: PAYOUT_UUID,
      retrySeq: RETRY_SEQ,
      expiryUnix: EXPIRY,
    });
    const sig = await signPayload(kp.privateKey, payload);
    expect(bytesToHex0x(sig)).toBe(vectors.ed25519_admin.refund_v1.signature_hex);
  });

  it("verifies both golden signatures against the imported raw pubkey", async () => {
    const pub = await importRawPublicKey(PUBKEY);
    const addrPayload = hexToBytes(vectors.ed25519_admin.addr_v1.payload_hex);
    const refundPayload = hexToBytes(vectors.ed25519_admin.refund_v1.payload_hex);
    expect(await verifyPayload(pub, hexToBytes(vectors.ed25519_admin.addr_v1.signature_hex), addrPayload)).toBe(true);
    expect(await verifyPayload(pub, hexToBytes(vectors.ed25519_admin.refund_v1.signature_hex), refundPayload)).toBe(true);
    // tamper one byte => reject
    const bad = new Uint8Array(addrPayload);
    bad[bad.length - 1] ^= 1;
    expect(await verifyPayload(pub, hexToBytes(vectors.ed25519_admin.addr_v1.signature_hex), bad)).toBe(false);
  });

  it("seed -> PKCS8 wrap is the standard 16B DER prefix", () => {
    const pkcs8 = seedToPkcs8(hexToBytes(SEED));
    expect(pkcs8.length).toBe(48);
    expect(bytesToHex(pkcs8.slice(0, 16))).toBe("302e020100300506032b657004220420");
  });
});

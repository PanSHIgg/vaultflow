// I2 — shared helpers for the real-backend E2E scripts (links A/B/C).
// Plain Node ≥20 (fetch/WebCrypto); viem for chain calls; NO test-runner deps.
// Run from repo root or web/:  node web/scripts/e2e-*.mjs

import { execFileSync } from "node:child_process";
import { createHmac, randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createPublicClient, createWalletClient, defineChain, erc20Abi, http, parseAbi } from "viem";
import { privateKeyToAccount } from "viem/accounts";

// ---------------------------------------------------------------------------
// Environment / constants (I1 docker-compose deployment)
// ---------------------------------------------------------------------------

export const REPO_ROOT = path.resolve(fileURLToPath(new URL(".", import.meta.url)), "../..");
export const DEPLOY_DIR = path.join(REPO_ROOT, "deploy");
export const DEPLOYED = JSON.parse(readFileSync(path.join(DEPLOY_DIR, "deployed.json"), "utf8"));

export const GW = (process.env.GATEWAY_URL ?? "http://localhost:8080").replace(/\/+$/, "");
export const RPC_A = process.env.ANVIL_RPC ?? "http://localhost:8545";
export const API_KEY =
  process.env.VF_API_KEY ?? "vf_demo_c12724dadc929d6a43a6b810e8779a362efb51e082401a39";
export const MERCHANT_ID = "11111111-2222-3333-4444-555555555555";
export const ADMIN_JWT_SECRET = "vaultflow-dev-admin-secret"; // deploy/gateway.env dev secret

export const CHAIN_ID = 31337;
export const USDC = DEPLOYED.chains[String(CHAIN_ID)].usdc;
export const USDT = DEPLOYED.chains[String(CHAIN_ID)].usdt;
export const ROUTER = DEPLOYED.chains[String(CHAIN_ID)].router;
export const FACTORY = DEPLOYED.chains[String(CHAIN_ID)].factory;

/** Anvil default accounts (foundry mnemonic "test test test … junk"). */
export const FUNDER_PK = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"; // #0 0xf39f…2266 (funding_account)
export const PAYER_PK = "0x2a871d0798f97d79848a013d4936a73bf4cc922c825d33c1cf7073dff6d409c6"; // #9 0xa0Ee…9720 — free account, NOT a platform wallet
export const CUSTOMER_PK = "0xdbda1821b80551c9d65939329250298aa3472ba22feea921c0cf5d620ea67b97"; // #8 0x2361…1E8f — free
export const PAYER = privateKeyToAccount(PAYER_PK).address.toLowerCase();
export const CUSTOMER = privateKeyToAccount(CUSTOMER_PK).address.toLowerCase();

/** platform hot wallets per chain — targets of the nonce-cursor workaround. */
export const HOT_WALLETS = {
  31337: {
    RELAYER_HOT: "0x70997970c51812dc3a010c7d01b50e0d17dc79c8",
    PAYOUT_HOT: "0x90f79bf6eb2c4f870365e785982e1f101e93b906",
  },
  31338: {
    RELAYER_HOT: "0x15d34aaf54267db7d7c367839aaf71a00a2c6a65",
    PAYOUT_HOT: "0x9965507d1a55bcc2695c58ba16fb37d819b0a4dc",
  },
};

export const anvil = defineChain({
  id: CHAIN_ID,
  name: "anvil-a",
  nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
  rpcUrls: { default: { http: [RPC_A] } },
});

export const pub = createPublicClient({ chain: anvil, transport: http(RPC_A) });
export const funder = createWalletClient({
  account: privateKeyToAccount(FUNDER_PK),
  chain: anvil,
  transport: http(RPC_A),
});
export const payerWallet = createWalletClient({
  account: privateKeyToAccount(PAYER_PK),
  chain: anvil,
  transport: http(RPC_A),
});
export const payerAccount = privateKeyToAccount(PAYER_PK);

// ---------------------------------------------------------------------------
// Small utils
// ---------------------------------------------------------------------------

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
export const uid = () => randomUUID();
export function ok(cond, msg) {
  if (!cond) throw new Error(`ASSERT FAILED: ${msg}`);
  console.log(`  ✔ ${msg}`);
}

export async function waitFor(label, fn, { timeoutMs = 90_000, intervalMs = 1_500 } = {}) {
  const t0 = Date.now();
  let last;
  while (Date.now() - t0 < timeoutMs) {
    last = await fn();
    if (last) return last;
    await sleep(intervalMs);
  }
  throw new Error(`TIMEOUT waiting for ${label} (last=${JSON.stringify(last)})`);
}

// ---------------------------------------------------------------------------
// Gateway REST client — mirrors web/src/lib/api.ts wire behavior
// ---------------------------------------------------------------------------

export class GatewayError extends Error {
  constructor(status, code, message, details) {
    super(`${status} ${code}: ${message}`);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

let adminJwt;
export function adminToken() {
  if (!adminJwt) {
    const b64 = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
    const h = `${b64({ alg: "HS256", typ: "JWT" })}.${b64({
      sub: "i2-e2e-admin",
      iat: Math.floor(Date.now() / 1000),
      exp: Math.floor(Date.now() / 1000) + 7200,
    })}`;
    adminJwt = `${h}.${createHmac("sha256", ADMIN_JWT_SECRET).update(h).digest("base64url")}`;
  }
  return adminJwt;
}

/** @returns {{status:number, data:any}} — throws GatewayError on !ok */
export async function api(method, path, { apiKey = API_KEY, token, body, idem } = {}) {
  const headers = {};
  if (body !== undefined) headers["content-type"] = "application/json";
  if (apiKey) headers["X-Api-Key"] = apiKey;
  if (idem) headers["Idempotency-Key"] = idem;
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(`${GW}${path}`, { method, headers, body: body !== undefined ? JSON.stringify(body) : undefined });
  const text = await res.text();
  let data;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = text;
  }
  if (!res.ok) {
    const e = data?.error ?? {};
    throw new GatewayError(res.status, e.code ?? "INTERNAL", e.message ?? res.statusText, e.details);
  }
  return { status: res.status, data };
}

export const createOrder = ({ chainId = CHAIN_ID, asset = "USDC", amount, expiresIn = 1800, ref } = {}) =>
  api("POST", "/v1/orders", {
    idem: `i2-${uid()}`,
    body: {
      chain_id: chainId,
      asset_symbol: asset,
      amount,
      expires_in_seconds: expiresIn,
      ...(ref ? { merchant_order_ref: ref } : {}),
    },
  });

export const getPublicPayment = (orderId) => api("GET", `/v1/public/payments/${orderId}`, { apiKey: undefined });
export const getPayout = (id) => api("GET", `/v1/payouts/${id}`);
export const listPayouts = () => api("GET", "/v1/payouts");
export const ledgerAccounts = () => api("GET", "/v1/ledger/accounts");
export const outboundQueue = () => api("GET", "/v1/admin/outbound-queue", { apiKey: undefined, token: adminToken() });
export const signerHealth = () => api("GET", "/v1/admin/signer/health", { apiKey: undefined, token: adminToken() });

export async function waitOrderStatus(orderId, want, timeoutMs = 90_000) {
  return waitFor(`order ${orderId.slice(0, 8)} status ∈ ${want.join("/")}`, async () => {
    const { data } = await getPublicPayment(orderId);
    return want.includes(data.status) ? data : null;
  }, { timeoutMs });
}

// ---------------------------------------------------------------------------
// SSE reader — fetch+stream frame parser (node has no EventSource <22.3)
// Emits {id,event,data,raw} per frame; heartbeat comments skipped.
// ---------------------------------------------------------------------------

export async function* sseFrames(url, { signal } = {}) {
  const res = await fetch(url, { signal, headers: { accept: "text/event-stream" } });
  if (!res.ok || !res.body) throw new Error(`SSE open failed: ${res.status}`);
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      buf += dec.decode(value, { stream: true });
      buf = buf.replace(/\r\n/g, "\n").replace(/\r$/, ""); // tolerate CRLF framing
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const raw = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        const frame = { id: undefined, event: "message", data: "" };
        for (const line of raw.split("\n")) {
          if (line.startsWith(":")) continue; // comment / heartbeat
          const c = line.indexOf(":");
          const field = c < 0 ? line : line.slice(0, c);
          const val = c < 0 ? "" : line.slice(c + 1).replace(/^ /, "");
          if (field === "id") frame.id = val;
          else if (field === "event") frame.event = val;
          else if (field === "data") frame.data += (frame.data ? "\n" : "") + val;
        }
        yield frame;
      }
    }
  } finally {
    reader.cancel().catch(() => {});
  }
}

/**
 * Watch an order's SSE stream in the background.
 * Returns {frames, done, stop} — `frames` fills progressively (each
 * {id,event,data,json}); `done` resolves when the stream ends/stops/errors;
 * `stop()` aborts immediately and `done` resolves with the frames so far.
 */
export function watchOrder(orderId, { stop, timeoutMs = 300_000 } = {}) {
  const frames = [];
  const ac = new AbortController();
  const t0 = Date.now();
  const done = (async () => {
    try {
      for await (const f of sseFrames(`${GW}/v1/public/payments/${orderId}/events`, { signal: ac.signal })) {
        let json;
        try {
          json = JSON.parse(f.data);
        } catch {
          json = undefined;
        }
        const rec = { ...f, json };
        if (f.event !== "message" || f.data) frames.push(rec); // skip empty heartbeat frames
        console.log(
          `  [SSE +${((Date.now() - t0) / 1000).toFixed(1)}s] event=${f.event} id=${f.id ?? "-"} ` +
            `${json?.status ? `status=${json.status} ` : ""}${json?.tx_hash ? `tx=${String(json.tx_hash).slice(0, 18)}… ` : ""}` +
            `${json?.current_confirmations !== undefined ? `conf=${json.current_confirmations}/${json.required_confirmations}` : ""}` +
            `${f.data && !json ? `data=${String(f.data).slice(0, 80)}` : ""}`,
        );
        if (stop?.(f, json)) break;
      }
    } catch (e) {
      if (!ac.signal.aborted) console.log(`  [SSE] stream error: ${e.message}`);
    }
    return frames;
  })();
  const timer = setTimeout(() => ac.abort(), timeoutMs);
  done.finally(() => clearTimeout(timer));
  return { frames, done, stop: () => ac.abort() };
}

// ---------------------------------------------------------------------------
// Chain helpers (anvil-a / MockUSDC)
// ---------------------------------------------------------------------------

export const erc20 = (account) => ({
  address: USDC,
  abi: erc20Abi,
});

const mintAbi = parseAbi(["function mint(address to, uint256 amount)"]);

export async function mintUsdc(to, amount) {
  return funder.writeContract({
    address: USDC,
    abi: mintAbi,
    functionName: "mint",
    args: [to, BigInt(amount)],
  });
}

export async function usdcBalanceOf(addr) {
  return pub.readContract({ ...erc20(), functionName: "balanceOf", args: [addr] });
}

export async function transferUsdc(fromWallet, to, amount) {
  return fromWallet.writeContract({ ...erc20(), functionName: "transfer", args: [to, BigInt(amount)] });
}

/** Advance the chain so the RPC "safe" tag covers freshly-mined txs.
 *  This anvil reports safe = latest - 32 and only mines on demand — always
 *  mine generously (≥40) after a tx and keep autoMine ticking while polling. */
export async function mineBlocks(n = 45) {
  await pub.request({ method: "anvil_mine", params: [`0x${n.toString(16)}`] });
}

/** Background miner: keeps safe_head advancing while we poll. Returns stop(). */
export function autoMine(intervalMs = 2500) {
  const t = setInterval(() => mineBlocks(1).catch(() => {}), intervalMs);
  t.unref?.();
  return () => clearInterval(t);
}

export async function waitTx(hash) {
  return pub.waitForTransactionReceipt({ hash, confirmations: 0, timeout: 60_000 });
}

// ---------------------------------------------------------------------------
// Byte-level Ed25519 payload builders — MIRROR of web/src/lib/ed25519.ts
// (AGENTS.md §2.3 FROZEN layout). Kept byte-identical; the signer's Ed25519
// verify of the server-rebuilt payload is itself the byte-parity check:
// any drift here → guaranteed SIGNER_REJECTED / REFUND_TICKET_INVALID.
// ---------------------------------------------------------------------------

const enc = new TextEncoder();
const hexToBytes = (h) => {
  const s = h.startsWith("0x") ? h.slice(2) : h;
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.slice(i * 2, i * 2 + 2), 16);
  return out;
};
const bytesToHex0x = (b) => `0x${[...b].map((x) => x.toString(16).padStart(2, "0")).join("")}`;
export { hexToBytes, bytesToHex0x };

export function uuidToBytes16(uuid) {
  return hexToBytes(uuid.replace(/-/g, ""));
}

function writeUintBE(out, offset, value, byteLen) {
  let v = BigInt(value);
  for (let i = byteLen - 1; i >= 0; i--) {
    out[offset + i] = Number(v & 0xffn);
    v >>= 8n;
  }
  if (v !== 0n) throw new Error(`value does not fit in ${byteLen} bytes`);
}

/** 69B "VAULTFLOW_ADDR_V1" || chain u64BE || merchant raw16 || recipient raw20 || valid_until u64BE */
export function buildAddrV1({ chainId, merchantUuid, recipient, validUntil }) {
  const out = new Uint8Array(69);
  out.set(enc.encode("VAULTFLOW_ADDR_V1"), 0);
  writeUintBE(out, 17, chainId, 8);
  out.set(uuidToBytes16(merchantUuid), 25);
  out.set(hexToBytes(recipient), 41);
  writeUintBE(out, 61, validUntil, 8);
  return out;
}

/** 127B "VAULTFLOW_REFUND_V1" || chain u64BE || token r20 || to r20 || amount u256BE || payout_uuid r16 || retry u32BE || expiry u64BE */
export function buildRefundV1({ chainId, token, to, amount, payoutUuid, retrySeq, expiry }) {
  const out = new Uint8Array(127);
  out.set(enc.encode("VAULTFLOW_REFUND_V1"), 0);
  writeUintBE(out, 19, chainId, 8);
  out.set(hexToBytes(token), 27);
  out.set(hexToBytes(to), 47);
  writeUintBE(out, 67, BigInt(amount), 32);
  out.set(uuidToBytes16(payoutUuid), 99);
  writeUintBE(out, 115, BigInt(retrySeq), 4);
  writeUintBE(out, 119, BigInt(expiry), 8);
  return out;
}

const PKCS8_PREFIX = hexToBytes("302e020100300506032b657004220420");

export async function importAdminSeed(seedHex) {
  const seed = hexToBytes(seedHex);
  if (seed.length !== 32) throw new Error("Ed25519 seed must be 32 bytes");
  const pkcs8 = new Uint8Array(PKCS8_PREFIX.length + 32);
  pkcs8.set(PKCS8_PREFIX, 0);
  pkcs8.set(seed, PKCS8_PREFIX.length);
  return crypto.subtle.importKey("pkcs8", pkcs8, { name: "Ed25519" }, true, ["sign"]);
}

export async function edSign(priv, payload) {
  const sig = new Uint8Array(await crypto.subtle.sign({ name: "Ed25519" }, priv, payload));
  if (sig.length !== 64) throw new Error("bad Ed25519 signature length");
  return bytesToHex0x(sig);
}

export function adminSeed() {
  return JSON.parse(readFileSync(path.join(DEPLOY_DIR, "admin-ed25519.json"), "utf8"));
}

// Read-only diagnostics (I2-BUG-1/3 runtime workarounds removed in FIX-G —
// the gateway nonce path now uses a legal "latest" block parameter and the
// deploy seed books HOT_WALLET_NATIVE TREASURY_TOPUP from real on-chain ETH).

export function psql(sql) {
  return execFileSync(
    "docker",
    ["exec", "vaultflow-postgres", "psql", "-U", "vaultflow", "-d", "vaultflow", "-tA", "-c", sql],
    { encoding: "utf8" },
  ).trim();
}

export function nonceCursors() {
  return psql("SELECT chain_id||' '||wallet_address||' '||next_nonce FROM wallet_nonce_cursors ORDER BY chain_id, wallet_address");
}

// ---------------------------------------------------------------------------
// Wave-3 tests/e2e support: re-export viem constructors so suites outside
// web/ can build clients through THIS file's module-resolution context
// (web/node_modules) without needing their own dependencies.
// ---------------------------------------------------------------------------
export {
  createPublicClient,
  createWalletClient,
  defineChain,
  encodeFunctionData,
  erc20Abi,
  http,
  keccak256,
  parseAbi,
  toBytes,
} from "viem";
export { generatePrivateKey, privateKeyToAccount } from "viem/accounts";

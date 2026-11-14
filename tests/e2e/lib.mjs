// Wave-3 adversarial E2E — shared helpers on top of web/scripts/e2e-lib.mjs.
// All third-party modules (viem) are resolved through e2e-lib's own import
// context (web/node_modules) — this file deliberately never imports bare
// specifiers, so tests/e2e needs no node_modules of its own. Plain Node ≥20.
// MUST run inside WSL (docker exec + python3 sqlite3 + anvil RPC).

import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";

export * from "../../web/scripts/e2e-lib.mjs";
import {
  adminSeed,
  adminToken,
  anvil,
  api,
  buildAddrV1,
  buildRefundV1,
  CHAIN_ID,
  CUSTOMER_PK,
  DEPLOY_DIR,
  DEPLOYED,
  edSign,
  funder,
  FUNDER_PK,
  importAdminSeed,
  MERCHANT_ID,
  psql,
  pub,
  RPC_A,
  sleep,
  USDC,
  USDT,
  waitFor,
} from "../../web/scripts/e2e-lib.mjs";

// viem constructors re-exported by e2e-lib (resolved in web/node_modules).
import {
  createPublicClient,
  createWalletClient,
  defineChain,
  http,
  parseAbi,
} from "../../web/scripts/e2e-lib.mjs";
import { privateKeyToAccount } from "../../web/scripts/e2e-lib.mjs";

// ---------------------------------------------------------------------------
// Chains / wallets / contracts
// ---------------------------------------------------------------------------

export const CHAIN_B = 31338;
export const RPC_B = process.env.ANVIL_RPC_B ?? "http://localhost:8546";
export const anvilB = defineChain({
  id: CHAIN_B,
  name: "anvil-b",
  nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
  rpcUrls: { default: { http: [RPC_B] } },
});
export const pubB = createPublicClient({ chain: anvilB, transport: http(RPC_B) });

const envText = readFileSync(path.join(DEPLOY_DIR, "gateway.env"), "utf8");
const cfgLine = envText.split(/\r?\n/).find((l) => l.startsWith("CHAIN_CONFIG_JSON="));
export const CHAIN_CFG = JSON.parse(cfgLine.slice("CHAIN_CONFIG_JSON=".length)).chains;

export const RELAYER_A = CHAIN_CFG[String(CHAIN_ID)].relayer_hot_wallet.toLowerCase();
export const PAYOUT_A = CHAIN_CFG[String(CHAIN_ID)].payout_hot_wallet.toLowerCase();
export const COLD_A = CHAIN_CFG[String(CHAIN_ID)].cold_wallets[0].toLowerCase();
export const SWEEPER_A = CHAIN_CFG[String(CHAIN_ID)].batch_sweeper.toLowerCase();
export const RELAYER_B = CHAIN_CFG[String(CHAIN_B)].relayer_hot_wallet.toLowerCase();
export const PAYOUT_B = CHAIN_CFG[String(CHAIN_B)].payout_hot_wallet.toLowerCase();
export const COLD_B = CHAIN_CFG[String(CHAIN_B)].cold_wallets[0].toLowerCase();
export const USDC_B = DEPLOYED.chains[String(CHAIN_B)].usdc.toLowerCase();
export const USDT_B = DEPLOYED.chains[String(CHAIN_B)].usdt.toLowerCase();

export const ZERO_UUID = "00000000-0000-0000-0000-000000000000";

// transfer/mint declared WITHOUT returns: simulates/decodes cleanly for both
// MockUSDC (returns bool — extra return bytes ignored) and MockUSDT (returns
// nothing — USDT shape).
export const tokenAbi = parseAbi([
  "function transfer(address to, uint256 amount)",
  "function mint(address to, uint256 amount)",
  "function balanceOf(address a) view returns (uint256)",
  "function setBlackListed(address who, bool listed)",
  "function isBlackListed(address who) view returns (bool)",
]);
export const sweeperAbi = parseAbi([
  "function deployAndSweepBatch((bytes32 salt, uint256 exactAmount)[] items, address token, address targetColdWallet)",
]);
export const factoryAbi = parseAbi([
  "function predictVaultAddress(bytes32 salt) view returns (address)",
]);

// Anvil default accounts (foundry test mnemonic).
const COLD_PK_A = "0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a"; // #2 = COLD_A
export const coldWalletA = createWalletClient({
  account: privateKeyToAccount(COLD_PK_A), chain: anvil, transport: http(RPC_A),
});
export const customerWallet = createWalletClient({
  account: privateKeyToAccount(CUSTOMER_PK), chain: anvil, transport: http(RPC_A),
});
export const funderB = createWalletClient({
  account: privateKeyToAccount(FUNDER_PK), chain: anvilB, transport: http(RPC_B),
});
// JSON-RPC account (no signing) — used after anvil_impersonateAccount.
export const relayerRpc = createWalletClient({
  account: RELAYER_A, chain: anvil, transport: http(RPC_A),
});

export const sendToken = (wallet, token, to, amount) =>
  wallet.writeContract({ address: token, abi: tokenAbi, functionName: "transfer", args: [to, amount] });
export const mintToken = (token, to, amount) =>
  funder.writeContract({ address: token, abi: tokenAbi, functionName: "mint", args: [to, amount] });
export const tokenBal = (token, addr, { client = pub, blockTag } = {}) =>
  client.readContract({ address: token, abi: tokenAbi, functionName: "balanceOf", args: [addr], blockNumber: blockTag });
export const setUsdtBlacklist = (who, listed) =>
  funder.writeContract({ address: USDT, abi: tokenAbi, functionName: "setBlackListed", args: [who, listed] });
export const isUsdtBlacklisted = (who) =>
  pub.readContract({ address: USDT, abi: tokenAbi, functionName: "isBlackListed", args: [who] });

// ---------------------------------------------------------------------------
// Anvil fork control (direct JSON-RPC — independent of the gateway)
// ---------------------------------------------------------------------------

export const aRpc = (method, params = []) => pub.request({ method, params });
export const snapA = () => aRpc("evm_snapshot");
export const revertA = (id) => aRpc("evm_revert", [id]);
export const mineA = (n = 1) => aRpc("anvil_mine", [`0x${BigInt(n).toString(16)}`]);
export const setAutomine = (on) => aRpc("anvil_setAutomine", [on]);
export const blockNumA = async () => Number(await pub.getBlockNumber());
export const dropTx = (hash) => aRpc("anvil_dropTransaction", [hash]);
export const impersonate = (addr) => aRpc("anvil_impersonateAccount", [addr]);
export const stopImpersonate = (addr) => aRpc("anvil_stopImpersonatingAccount", [addr]);
export const ethBal = (addr, { client = pub, blockTag } = {}) =>
  client.getBalance({ address: addr, blockNumber: blockTag });
export const predictVault = (salt) =>
  pub.readContract({
    address: DEPLOYED.chains[String(CHAIN_ID)].factory,
    abi: factoryAbi, functionName: "predictVaultAddress", args: [salt],
  });

// ---------------------------------------------------------------------------
// Postgres readers (docker exec vaultflow-postgres)
// ---------------------------------------------------------------------------

export const q1 = (sql) => psql(sql);
export const qRows = (sql) => {
  const out = psql(sql);
  return out ? out.split("\n") : [];
};

export const depRowsForOrder = (orderId) =>
  qRows(`SELECT id||' '||status||' '||tx_hash||' '||block_number||' '||amount||' '||deposit_category
         FROM onchain_deposits WHERE matched_order_id='${orderId}' ORDER BY block_number, log_index`);
export const orderStatusRow = (orderId) =>
  q1(`SELECT status||' '||received_amount||' '||expected_amount FROM payment_orders WHERE id='${orderId}'`);
export const vaultFlags = (addr, chainId = CHAIN_ID) =>
  q1(`SELECT is_quarantined||' '||is_deployed FROM deposit_addresses
      WHERE chain_id=${chainId} AND address='${addr.toLowerCase()}'`);
export const sweepItemRows = (vaultAddr, chainId = CHAIN_ID) =>
  qRows(`SELECT sbi.id||' '||sbi.item_status||' '||sb.status||' '||sbi.batch_id||' '||
                COALESCE(sbi.actual_swept_amount::text,'-')||' '||sbi.exact_sweep_amount
         FROM sweep_batch_items sbi
         JOIN deposit_addresses da ON da.id=sbi.deposit_address_id
         JOIN sweep_batches sb ON sb.id=sbi.batch_id
         WHERE sb.chain_id=${chainId} AND da.address='${vaultAddr.toLowerCase()}'
         ORDER BY sb.created_at`);
export const intentRows = (where) =>
  qRows(`SELECT id||' '||intent_type||' '||status||' '||nonce||' '||reference_id
         FROM outbound_tx_intents WHERE ${where} ORDER BY created_at`);
export const attemptRows = (intentId) =>
  qRows(`SELECT id||' '||attempt_no||' '||attempt_kind||' '||status||' '||tx_hash||' '||
                COALESCE(block_number::text,'-')||' '||COALESCE(receipt_status::text,'-')||' '||
                COALESCE(gas_used::text,'-')||' '||COALESCE(effective_gas_price::text,'-')||' '||
                COALESCE(block_hash::text,'-')
         FROM outbound_tx_attempts WHERE intent_id='${intentId}' ORDER BY attempt_no`);
export const ledgerBal = (accountType, { owner = ZERO_UUID, chainId = CHAIN_ID, asset } = {}) =>
  BigInt(
    psql(`SELECT COALESCE(SUM(balance),0) FROM ledger_accounts
          WHERE owner_merchant_id='${owner}' AND chain_id=${chainId}
            AND asset_symbol='${asset}' AND account_type='${accountType}'`) || "0",
  );
export const merchantFrozen = () =>
  psql(`SELECT payout_frozen FROM merchants WHERE id='${MERCHANT_ID}'`) === "t";
export const receivableBal = (asset, chainId = CHAIN_ID) =>
  ledgerBal("MERCHANT_REORG_RECEIVABLE", { owner: MERCHANT_ID, chainId, asset });
export const safeHead = (chainId = CHAIN_ID) =>
  Number(psql(`SELECT safe_head_height FROM chain_sync_cursors WHERE chain_id=${chainId}`) || "0");
export const payoutRow = (id) =>
  q1(`SELECT status||' '||payout_kind||' '||source_account_type||' '||amount FROM merchant_payouts WHERE id='${id}'`);
export const reorgReversals = () =>
  qRows(`SELECT reference_id||' '||created_at FROM ledger_transactions
         WHERE reference_type='REORG_REVERSAL' ORDER BY created_at, id`);
export const supportedAssets = () =>
  qRows(`SELECT chain_id||' '||asset_symbol||' '||COALESCE(token_contract,'-') FROM supported_assets ORDER BY chain_id, asset_symbol`);

// ---------------------------------------------------------------------------
// Signer SQLite readers (deploy/signer-data/signer.db via python3 sqlite3)
// ---------------------------------------------------------------------------

const SIGNER_DB = path.join(DEPLOY_DIR, "signer-data", "signer.db");
const PYC = `import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
try:
  for r in c.execute(sys.argv[2]):
    print("|".join("NULL" if v is None else (v.hex() if isinstance(v,bytes) else str(v)) for v in r))
finally:
  c.close()`;
export const signerDb = (sql) => {
  try {
    return execFileSync("python3", ["-c", PYC, SIGNER_DB, sql], { encoding: "utf8" }).trim();
  } catch (e) {
    return `__ERR__ ${e.message}`;
  }
};
export const signerSlots = (where = "1=1") =>
  signerDb(`SELECT chain_id,from_wallet,nonce,kind,COALESCE(hex(payout_uuid),'NULL'),COALESCE(retry_seq,-1),COALESCE(hex(token),'NULL'),COALESCE(hex(recipient),'NULL'),COALESCE(hex(amount),'NULL'),filler_signed FROM signer_intent_slots WHERE ${where} ORDER BY nonce`)
    .split("\n").filter((l) => l && !l.startsWith("__ERR__"));
export const signerUsageCount = (chainId = CHAIN_ID) =>
  Number(signerDb(`SELECT COUNT(*) FROM rolling_24h_usage WHERE chain_id=${chainId}`) || "0");
export const auditRows = () =>
  signerDb(`SELECT seq,timestamp_unix,action,hex(request_digest),decision,reason,hex(prev_hash),hex(entry_hash) FROM audit_logs ORDER BY seq`)
    .split("\n").filter((l) => l && !l.startsWith("__ERR__"));

// JS-side re-implementation of the signer audit hash chain:
//   entry_hash = SHA256(seq u64BE || ts i64BE || len32|action || digest32
//                       || len32|decision || len32|reason || prev_hash32)
export function verifyAuditChainLocal() {
  const u64 = (v) => { const b = Buffer.alloc(8); b.writeBigUInt64BE(BigInt(v)); return b; };
  const i64 = (v) => { const b = Buffer.alloc(8); b.writeBigInt64BE(BigInt(v)); return b; };
  const lp = (s) => {
    const b = Buffer.from(s, "utf8"); const l = Buffer.alloc(4);
    l.writeUInt32BE(b.length); return Buffer.concat([l, b]);
  };
  let prev = "00".repeat(32);
  let n = 0;
  for (const line of auditRows()) {
    const f = line.split("|");
    // sqlite hex() renders UPPERCASE; hash inputs must keep original case
    // (action/decision/reason are hashed as UTF-8), only digests normalize.
    const [seq, ts, action, digestU, decision, reason, prevHU, entryHU] = f;
    const digest = digestU.toLowerCase();
    const prevH = prevHU.toLowerCase();
    const entryH = entryHU.toLowerCase();
    n++;
    if (BigInt(seq) !== BigInt(n)) return { valid: false, firstBad: n, why: `seq gap: want ${n} got ${seq}` };
    if (prevH !== prev) return { valid: false, firstBad: n, why: `prev_hash mismatch at seq ${seq}` };
    const h = createHash("sha256").update(Buffer.concat([
      u64(seq), i64(ts), lp(action), Buffer.from(digest, "hex"),
      lp(decision), lp(reason), Buffer.from(prevH, "hex"),
    ])).digest("hex");
    if (h !== entryH) return { valid: false, firstBad: n, why: `entry_hash mismatch at seq ${seq}` };
    prev = entryH;
  }
  return { valid: true, entries: n };
}

// ---------------------------------------------------------------------------
// Admin / simulator / reconciliation API helpers
// ---------------------------------------------------------------------------

export const adminApi = (method, p, opts = {}) =>
  api(method, p, { ...opts, apiKey: undefined, token: adminToken() });
export const simScenario = (name, options = {}) =>
  adminApi("POST", `/v1/sim/scenarios/${name}`, {
    idem: `sim-${name}-${randomUUID()}`,
    body: { chain_id: CHAIN_ID, options },
  });
export const simRun = (runId) => adminApi("GET", `/v1/sim/runs/${runId}`);
export const waitSimRun = (runId, timeoutMs = 300_000) =>
  waitFor(`sim run ${String(runId).slice(0, 8)}`, async () => {
    const { data } = await simRun(runId);
    return ["COMPLETED", "FAILED"].includes(data.status) ? data : null;
  }, { timeoutMs, intervalMs: 2_000 });
export const reconRun = (body = {}) =>
  adminApi("POST", "/v1/admin/reconciliation/run", { idem: `recon-${randomUUID()}`, body });
export const reconStatus = () => adminApi("GET", "/v1/admin/reconciliation/status");
export const unfreezeMerchant = () =>
  adminApi("POST", `/v1/admin/merchants/${MERCHANT_ID}/payout-unfreeze`, { idem: `unfreeze-${randomUUID()}` });

// ---------------------------------------------------------------------------
// Merchant flows (whitelist / payout / refund / review / cancel)
// ---------------------------------------------------------------------------

let adminPriv = null;
const adminKey = async () => {
  if (!adminPriv) adminPriv = await importAdminSeed(adminSeed().private_key_hex);
  return adminPriv;
};

export async function registerWhitelist(addr, { chainId = CHAIN_ID } = {}) {
  const priv = await adminKey();
  const validUntil = Math.floor(Date.now() / 1000) + 7 * 86400;
  const payload = buildAddrV1({ chainId, merchantUuid: MERCHANT_ID, recipient: addr, validUntil });
  const sig = await edSign(priv, payload);
  const res = await api("POST", "/v1/withdrawal-addresses", {
    idem: `wl-${randomUUID()}`,
    body: { chain_id: chainId, address: addr.toLowerCase(), valid_until_unix: validUntil, admin_signature: sig },
  });
  return { ...res, registeredAt: Date.now() };
}

// Signer-side lease cooldown is 60s from first_seen_at; pad to 65s.
export const ensureCooldown = async (registeredAt, cooldownMs = 65_000) => {
  const wait = cooldownMs - (Date.now() - registeredAt);
  if (wait > 0) {
    console.log(`  …signer whitelist cooldown: waiting ${Math.ceil(wait / 1000)}s`);
    await sleep(wait);
  }
};

export async function createPayout({ to, amount, asset = "USDC", chainId = CHAIN_ID }) {
  const payoutUuid = randomUUID();
  const res = await api("POST", "/v1/payouts", {
    idem: `po-${payoutUuid}`,
    body: {
      payout_uuid: payoutUuid, source_account: "MERCHANT_AVAILABLE",
      chain_id: chainId, asset_symbol: asset, amount: String(amount), to_address: to.toLowerCase(),
    },
  });
  return { ...res, payoutUuid };
}

export const cancelPayout = (id, reason = "w3-e2e cleanup") =>
  api("POST", `/v1/payouts/${id}/cancel`, { idem: `cx-${randomUUID()}`, body: { reason } });

export const reviewOrder = (orderId, action, extra = {}) =>
  api("POST", `/v1/orders/${orderId}/review`, {
    idem: `rev-${randomUUID()}`,
    body: { action, note: `w3-e2e ${action}`, ...extra },
  });

export async function createSuspenseRefund({ source, asset = "USDC", chainId = CHAIN_ID, amount, to, orderId = null }) {
  const priv = await adminKey();
  const payoutUuid = randomUUID();
  const token = (asset === "USDC" ? (chainId === CHAIN_ID ? USDC : USDC_B) : (chainId === CHAIN_ID ? USDT : USDT_B));
  const expiry = Math.floor(Date.now() / 1000) + 3600;
  const ticket = buildRefundV1({ chainId, token, to, amount: BigInt(amount), payoutUuid, retrySeq: 0, expiry });
  const sig = await edSign(priv, ticket);
  const res = await api("POST", "/v1/refunds", {
    idem: `rf-${payoutUuid}`,
    body: {
      payout_uuid: payoutUuid, source_account: source, source_order_id: orderId,
      chain_id: chainId, asset_symbol: asset, amount: String(amount),
      refund_ticket: {
        to_address: to.toLowerCase(), payout_uuid: payoutUuid,
        retry_seq: 0, expiry_unix: expiry, admin_signature: sig,
      },
    },
  });
  return { ...res, payoutUuid };
}

// ---------------------------------------------------------------------------
// Drain-wait + second-layer custody snapshot
// ---------------------------------------------------------------------------

export async function quiesce(chainId = CHAIN_ID, { timeoutMs = 300_000 } = {}) {
  await waitFor("pipeline quiesce", async () => {
    const n = q1(`SELECT
      (SELECT COUNT(*) FROM onchain_deposits WHERE chain_id=${chainId} AND status='DETECTED')||' '||
      (SELECT COUNT(*) FROM outbound_tx_intents WHERE chain_id=${chainId} AND status IN ('QUEUED','IN_FLIGHT','INCLUDED_PENDING_SAFE'))||' '||
      (SELECT COUNT(*) FROM outbound_tx_attempts a JOIN outbound_tx_intents i ON i.id=a.intent_id
        WHERE i.chain_id=${chainId} AND a.status IN ('BROADCASTED','INCLUDED_UNSAFE'))||' '||
      (SELECT COUNT(*) FROM merchant_payouts WHERE chain_id=${chainId} AND status IN ('LOCKED_QUEUED','IN_FLIGHT'))||' '||
      (SELECT COUNT(*) FROM sweep_batches WHERE chain_id=${chainId} AND status IN ('PENDING','IN_FLIGHT'))||' '||
      (SELECT COUNT(*) FROM sweep_batch_items s JOIN sweep_batches b ON b.id=s.batch_id
        WHERE b.chain_id=${chainId} AND s.item_status='PENDING')`);
    return n === "0 0 0 0 0 0" ? true : null;
  }, { timeoutMs, intervalMs: 3_000 });
  await waitFor("safe head catches tip", async () => {
    const tip = await blockNumA();
    return safeHead(chainId) >= tip - 1 ? true : null;
  }, { timeoutMs: 90_000, intervalMs: 2_000 });
}

// Layer-2 UNSWEPT: sum of FINALIZED deposits minus SWEPT items per vault (DB
// truth) must equal the on-chain token balance of each vault at safe_head.
export async function custodySnapshotCheck(chainId, asset, tokenAddr, client, coldAddr, hotPayout) {
  const safe = safeHead(chainId);
  const blockTag = BigInt(safe);
  const tok = tokenAddr.toLowerCase();
  const vaultRows = qRows(`SELECT da.address, (COALESCE(d.s,0)-COALESCE(w.s,0))::text
    FROM deposit_addresses da
    LEFT JOIN (SELECT lower(to_address) a, SUM(amount) s FROM onchain_deposits
               WHERE chain_id=${chainId} AND status='FINALIZED' AND lower(token_contract)='${tok}'
               GROUP BY lower(to_address)) d ON d.a=da.address
    LEFT JOIN (SELECT sbi.deposit_address_id i, SUM(sbi.actual_swept_amount) s
               FROM sweep_batch_items sbi JOIN sweep_batches sb ON sb.id=sbi.batch_id
               WHERE sb.chain_id=${chainId} AND sb.asset_symbol='${asset}' AND sbi.item_status='SWEPT'
               GROUP BY sbi.deposit_address_id) w ON w.i=da.id
    WHERE da.chain_id=${chainId} AND COALESCE(d.s,0)-COALESCE(w.s,0) <> 0`);
  let chainSum = 0n;
  const vaultMismatch = [];
  for (const line of vaultRows) {
    const sp = line.indexOf("|"); // psql -tA default field separator
    const addr = line.slice(0, sp);
    const want = BigInt(line.slice(sp + 1));
    const got = await tokenBal(tok, addr, { client, blockTag });
    chainSum += got;
    if (got !== want) vaultMismatch.push({ addr, db_unswept: line.slice(sp + 1), onchain: got.toString() });
  }
  const ledUnswept = ledgerBal("CUSTODY_UNSWEPT", { chainId, asset });
  const onCold = await tokenBal(tok, coldAddr, { client, blockTag });
  const ledCold = ledgerBal("CUSTODY_COLD", { chainId, asset });
  const onHot = await tokenBal(tok, hotPayout, { client, blockTag });
  const ledHot = ledgerBal("CUSTODY_HOT_PAYOUT", { chainId, asset });
  return {
    chainId, asset, safeHead: safe,
    UNSWEPT: { ledger: ledUnswept, onchain: chainSum, delta: chainSum - ledUnswept, vaultCount: vaultRows.length, vaultMismatch },
    COLD: { ledger: ledCold, onchain: onCold, delta: onCold - ledCold },
    HOT_PAYOUT: { ledger: ledHot, onchain: onHot, delta: onHot - ledHot },
  };
}

// HOT_WALLET_NATIVE: relayer + payout wallet ETH at safe_head vs ledger.
export async function nativeSnapshotCheck(chainId, client, relayer, payout) {
  const safe = safeHead(chainId);
  const blockTag = BigInt(safe);
  const onchain = (await ethBal(relayer, { client, blockTag })) + (await ethBal(payout, { client, blockTag }));
  const rows = qRows(`SELECT asset_symbol||' '||balance FROM ledger_accounts
    WHERE owner_merchant_id='${ZERO_UUID}' AND chain_id=${chainId} AND account_type='HOT_WALLET_NATIVE'`);
  const ledger = rows.reduce((acc, l) => acc + BigInt(l.split(" ")[1]), 0n);
  return { chainId, safeHead: safe, ledger, onchain, delta: onchain - ledger, ledgerRows: rows };
}

// ---------------------------------------------------------------------------
// Assertion / reporting helpers (non-fatal check + fatal ok from e2e-lib)
// ---------------------------------------------------------------------------

let FAILS = 0;
export const check = (cond, label) => {
  if (cond) console.log(`  ✓ ${label}`);
  else { FAILS++; console.log(`  ✗ FAIL: ${label}`); }
};
export const checkEq = (got, want, label) => check(String(got) === String(want), `${label} (got=${got} want=${want})`);
export const section = (t) => console.log(`\n━━━ ${t} ━━━`);
export const failCount = () => FAILS;
export const finish = (name) => {
  if (FAILS > 0) { console.error(`\n=== ${name} FAILED (${FAILS} assertion(s)) ===`); process.exit(1); }
  console.log(`\n=== ${name} PASSED ===`);
};

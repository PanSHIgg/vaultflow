// W3-S1 — 双向 LIFO 重组验收 (ARCHITECTURE §8.1 / plan.md Wave-3 S1)
//
//   S1a 浅分叉·入站: deposit DETECTED -> evm_revert 孤块化 -> 竞争分叉重发 ->
//      ORPHANED_SHALLOW + 新拷贝 FINALIZED/PAID (金额不双计).
//   S1b 浅分叉·出站: sweep attempt INCLUDED_UNSAFE -> revert -> LIFO 重置 ->
//      同 intent 重广播在竞争分叉重新打包 -> FINALIZED + 冷钱包到账.
//   S1c 深分叉     : 充值 FINALIZED(订单结算) -> 归集 FINALIZED(CUSTODY_COLD)
//                    -> 提现 COMPLETED -> 回滚到充值前 -> LIFO 红冲
//                    (payout > sweep > deposit 高度降序) ->
//                    ORPHANED_DEEP + MERCHANT_REORG_RECEIVABLE +
//                    payout_frozen 自动置位; 冻结期间提现/解冻被拒;
//                    运营核销后解冻恢复.
//
// Run: node tests/e2e/s1-reorg.mjs   (WSL, stack up)

import {
  attemptRows,
  autoMine,
  blockNumA,
  cancelPayout,
  CHAIN_ID,
  check,
  checkEq,
  coldWalletA,
  COLD_A,
  createOrder,
  createPayout,
  CUSTOMER,
  customerWallet,
  depRowsForOrder,
  ensureCooldown,
  finish,
  generatePrivateKey,
  GatewayError,
  intentRows,
  ledgerBal,
  MERCHANT_ID,
  merchantFrozen,
  mineA,
  mintToken,
  orderStatusRow,
  PAYER,
  payerWallet,
  payoutRow,
  PAYOUT_A,
  privateKeyToAccount,
  psql,
  q1,
  qRows,
  receivableBal,
  registerWhitelist,
  reorgReversals,
  revertA,
  section,
  sendToken,
  setAutomine,
  snapA,
  sweepItemRows,
  tokenBal,
  uid,
  unfreezeMerchant,
  USDC,
  vaultFlags,
  waitFor,
  waitTx,
  ZERO_UUID,
} from "./lib.mjs";

const O1_AMT = "1000000";    // 1.0 USDC
const O2_AMT = "3000000";    // 3.0 USDC
const TOPUP = 2_000_000n;    // cold -> payout-hot rebalance (canonical, pre-snapC)

console.log("=== W3-S1 双向 LIFO 重组 (浅入站/浅出站/深红冲) ===\n");

await setAutomine(true);
let mining = autoMine(2500); // periodic empty blocks drive confirmation depth

// --- funding ----------------------------------------------------------------
if ((await tokenBal(USDC, PAYER)) < 6_000_000n) await waitTx(await mintToken(USDC, PAYER, 12_000_000n));
if ((await tokenBal(USDC, CUSTOMER)) < 2_000_000n) await waitTx(await mintToken(USDC, CUSTOMER, 4_000_000n));

// --- whitelist + cold->hot rebalance (both canonical, before the deep snap) ---
section("前置：白名单注册 + 冷→热 USDC 调拨");
const W = privateKeyToAccount(generatePrivateKey()).address.toLowerCase();
const wl = await registerWhitelist(W);
check(wl.status === 201 && wl.data?.status === "ACTIVE", `withdrawal whitelist ${W.slice(0, 10)}… registered ACTIVE`);
const wlAt = wl.registeredAt;

// drain leftover in-flight inbound legs so the rebalance delta is exact
await waitFor("prior inbound deposits drained", async () =>
  q1(`SELECT COUNT(*) FROM onchain_deposits WHERE chain_id=${CHAIN_ID} AND status='DETECTED'`) === "0" ? true : null,
{ timeoutMs: 120_000 });
const hotBal0 = await tokenBal(USDC, PAYOUT_A);
const ledHot0 = ledgerBal("CUSTODY_HOT_PAYOUT", { asset: "USDC" });
const topupTx = await sendToken(coldWalletA, USDC, PAYOUT_A, TOPUP);
await waitTx(topupTx);
await waitFor("cold→hot rebalance FINALIZED", async () =>
  q1(`SELECT status FROM onchain_deposits
        WHERE chain_id=${CHAIN_ID} AND tx_hash='${topupTx.toLowerCase()}'`) === "FINALIZED" ? true : null,
{ timeoutMs: 90_000 });
checkEq((await tokenBal(USDC, PAYOUT_A)) - hotBal0, TOPUP, "payout hot wallet on-chain +rebalance");
checkEq(ledgerBal("CUSTODY_HOT_PAYOUT", { asset: "USDC" }) - ledHot0, TOPUP, "ledger CUSTODY_HOT_PAYOUT +rebalance");

// ===========================================================================
section("S1a 浅分叉·入站：未确认充值孤块化 → 竞争分叉重放不双计");
// ===========================================================================
const { data: o1 } = await createOrder({ amount: O1_AMT, asset: "USDC" });
console.log(`  order1 ${o1.id} vault=${o1.deposit_address}`);

mining(); // stop ticker — deposit must stay DETECTED (never reaches safe head)
const snap1 = await snapA();
const h0 = await blockNumA();
const d1 = await sendToken(payerWallet, USDC, o1.deposit_address, BigInt(O1_AMT));
await waitTx(d1);
await waitFor("o1 deposit DETECTED", async () =>
  depRowsForOrder(o1.id).some((r) => r.split(" ")[1] === "DETECTED") ? true : null,
{ timeoutMs: 90_000 });
console.log(`  deposit tx ${d1.slice(0, 14)}… detected at h=${h0 + 1} (unsafe)`);

check(await revertA(snap1), "evm_revert to pre-deposit snapshot");
// re-send on the competing fork from a DIFFERENT payer — identical nonce+params
// would otherwise reuse the same tx_hash and be invisible to the indexer.
const d2 = await sendToken(customerWallet, USDC, o1.deposit_address, BigInt(O1_AMT));
await waitTx(d2);
mining = autoMine(2500); // resume — new fork wins, resend finalizes

const o1Final = await waitFor("o1 PAID on competing fork", async () => {
  const s = orderStatusRow(o1.id);
  return s?.startsWith("PAID") || s?.startsWith("OVERPAID") ? s : null;
}, { timeoutMs: 180_000 });
const rows1a = depRowsForOrder(o1.id);
const depOrphan = rows1a.find((r) => r.includes("ORPHANED_SHALLOW"));
const depFinal = rows1a.find((r) => r.includes("FINALIZED"));
console.log("  deposits:", rows1a);
check(!!depOrphan, `orphaned unconfirmed deposit marked ORPHANED_SHALLOW`);
check(depOrphan?.split(" ")[2] === d1, `  └ orphan row carries original tx ${d1.slice(0, 14)}…`);
check(!!depFinal, `re-sent deposit FINALIZED on competing fork`);
check(depFinal?.split(" ")[2] === d2, `  └ finalized row carries resend tx ${d2.slice(0, 14)}…`);
checkEq(orderStatusRow(o1.id).split(" ")[1], O1_AMT, `received_amount counts resend exactly once (${o1Final})`);

// ===========================================================================
section("S1b 浅分叉·出站：INCLUDED_UNSAFE 归集孤块化 → 同 intent 重广播重入");
// ===========================================================================
mining(); // pause: capture the sweep attempt in its unsafe window
const snapB = await snapA();
const hB = await blockNumA();
const swe = await waitFor("sweep attempt INCLUDED_UNSAFE", async () => {
  for (const i of intentRows(`intent_type='SWEEP_BATCH' AND chain_id=${CHAIN_ID}`)) {
    const iid = i.split(" ")[0];
    const att = attemptRows(iid).find((a) => a.split(" ")[3] === "INCLUDED_UNSAFE");
    if (att) return { intentId: iid, att };
  }
  return null;
}, { timeoutMs: 180_000, intervalMs: 1_000 });
console.log(`  intent ${swe.intentId.slice(0, 8)}… attempt#${swe.att.split(" ")[1]} included unsafe at h>${hB}`);

check(await revertA(snapB), "evm_revert orphans the unsafe inclusion");
mining = autoMine(2500);

// RebroadcastIntent REUSES the reset attempt row and deterministically re-signs
// the identical transaction — the recovery proof is: same attempt row re-
// FINALIZED, same tx_hash, different block_hash on the winning fork. No
// attempt_no>=2 row is appended for reorg recovery.
const [sweAttRowId, , , , sweTxHash, , , , , sweBlkHash] = swe.att.split(" ");
const reincl = await waitFor("same attempt re-broadcast + re-FINALIZED", async () => {
  const atts = attemptRows(swe.intentId);
  const fin = atts.find((a) => a.split(" ")[3] === "FINALIZED" && a.split(" ")[9] !== sweBlkHash);
  return fin ? { atts, fin } : null;
}, { timeoutMs: 240_000 });
console.log("  attempts:", reincl.atts);
checkEq(reincl.atts.length, 1, "attempt row REUSED — no new attempt appended");
check(reincl.fin.split(" ")[0] === sweAttRowId, "same attempt row id re-FINALIZED");
check(reincl.fin.split(" ")[4] === sweTxHash,
  `same tx_hash ${sweTxHash.slice(0, 14)}… re-signed deterministically on new fork`);
check(reincl.fin.split(" ")[9] !== sweBlkHash, "re-included under a DIFFERENT block hash");
const v1Items = await waitFor("V1 item SWEPT + batch COMPLETED", async () => {
  const r = sweepItemRows(o1.deposit_address);
  return r.some((l) => l.includes("SWEPT") && l.includes("COMPLETED")) ? r : null;
}, { timeoutMs: 180_000 });
console.log("  V1 items:", v1Items);
check(v1Items.every((l) => l.includes("SWEPT")), "sweep items re-settled SWEPT on winning fork");

// ===========================================================================
section("S1c 深分叉·LIFO：结算→归集→提现全部逆序红冲 + 追偿冻结");
// ===========================================================================
await ensureCooldown(wlAt);

// Drain ALL of MERCHANT_AVAILABLE with the payout: the unwind then finds zero
// consumable MA and the FULL settle-net lands on MERCHANT_REORG_RECEIVABLE —
// deterministic regardless of residual MA left by earlier scenarios/seeds.
const priorMA = ledgerBal("MERCHANT_AVAILABLE", { owner: MERCHANT_ID, asset: "USDC" });
const settleNet = 2_992_500n;               // O2 3,000,000 − 25bps seed fee → net credited to MA
const payAmt = priorMA + settleNet;          // == MA right after o2 settles
// The payout's chain leg needs hot USDC ≥ payAmt — top up BEFORE snapC so the
// top-up stays canonical and the re-broadcast payout still has funds on the fork.
const hotBalC = await tokenBal(USDC, PAYOUT_A);
if (hotBalC < payAmt) {
  const need = payAmt - hotBalC + 1_000_000n;
  const coldShort = need - (await tokenBal(USDC, COLD_A));
  if (coldShort > 0n) await waitTx(await mintToken(USDC, COLD_A, coldShort + 1_000_000n));
  const top2 = await sendToken(coldWalletA, USDC, PAYOUT_A, need);
  await waitTx(top2);
  await waitFor("S1c pre-snap topup FINALIZED", async () =>
    q1(`SELECT status FROM onchain_deposits WHERE chain_id=${CHAIN_ID} AND tx_hash='${top2.toLowerCase()}'`)
      === "FINALIZED" ? true : null, { timeoutMs: 90_000 });
}

const { data: o2 } = await createOrder({ amount: O2_AMT, asset: "USDC" });
console.log(`  order2 ${o2.id} vault=${o2.deposit_address}`);
const snapC = await snapA();
const hC = await blockNumA();

const d3 = await sendToken(payerWallet, USDC, o2.deposit_address, BigInt(O2_AMT));
await waitTx(d3);
await waitFor("o2 PAID (ORDER_SETTLE posted)", async () =>
  orderStatusRow(o2.id)?.startsWith("PAID") ? true : null, { timeoutMs: 180_000 });
const depRow2 = depRowsForOrder(o2.id).find((r) => r.includes("FINALIZED"));
const [depId2, , , depBlk2] = depRow2.split(" ");
console.log(`  deposit FINALIZED @h=${depBlk2} -> ORDER_SETTLE`);

const v2Swept = await waitFor("V2 item SWEPT + batch COMPLETED (SWEEP_SETTLE posted)", async () => {
  const r = sweepItemRows(o2.deposit_address);
  return r.some((l) => l.includes("SWEPT") && l.includes("COMPLETED")) ? r : null;
}, { timeoutMs: 240_000 });
const v2Batch = v2Swept[0].split(" ")[3];
const v2Intent = intentRows(`intent_type='SWEEP_BATCH' AND reference_id='${v2Batch}'`)[0].split(" ")[0];
const sweAtt = attemptRows(v2Intent).find((a) => a.split(" ")[3] === "FINALIZED").split(" ");
const [sweAttId, , , , , sweBlk] = sweAtt;
console.log(`  sweep attempt ${sweAttId.slice(0, 8)}… FINALIZED @h=${sweBlk}`);

const { payoutUuid, data: po } = await createPayout({ to: W, amount: String(payAmt), asset: "USDC" });
check(po.status === "LOCKED_QUEUED", `payout ${payoutUuid.slice(0, 8)}… LOCKED_QUEUED`);
await waitFor("payout COMPLETED (PAYOUT_FLOW exec posted)", async () =>
  payoutRow(payoutUuid)?.startsWith("COMPLETED") ? true : null, { timeoutMs: 240_000 });
const payIntent = intentRows(`intent_type='MERCHANT_PAYOUT' AND reference_id='${payoutUuid}'`)[0].split(" ")[0];
const payAtt = attemptRows(payIntent).find((a) => a.split(" ")[3] === "FINALIZED").split(" ");
const [payAttId, , , , , payBlk] = payAtt;
console.log(`  payout attempt ${payAttId.slice(0, 8)}… FINALIZED @h=${payBlk} -> COMPLETED`);
check(Number(depBlk2) < Number(sweBlk) && Number(sweBlk) < Number(payBlk),
  `heights ascending dep@${depBlk2} < sweep@${sweBlk} < payout@${payBlk}`);

const ledColdPre = ledgerBal("CUSTODY_COLD", { asset: "USDC" });

// ---- the deep reorg ----------------------------------------------------------
mining(); // freeze block production during the revert
check(await revertA(snapC), `evm_revert to h=${hC} — orphan deposit+sweep+payout window`);
await mineA(12); // competing fork strictly longer
mining = autoMine(2500);

const rev = await waitFor("SETTLED_REVERSED + receivable + frozen", async () => {
  const st = orderStatusRow(o2.id) || "";
  const recv = receivableBal("USDC");
  const frz = merchantFrozen();
  return st.startsWith("SETTLED_REVERSED") && recv > 0n && frz
    ? { st, recv, frz } : null;
}, { timeoutMs: 300_000, intervalMs: 2_500 });
console.log(`  receivable=${rev.recv} frozen=${rev.frz} order=${rev.st}`);
check(true, "order2 -> SETTLED_REVERSED (deep unwind of ORDER_SETTLE)");
check(depRowsForOrder(o2.id).some((r) => r.includes("ORPHANED_DEEP")),
  "o2 deposit -> ORPHANED_DEEP (was FINALIZED pre-fork)");

// LIFO ordering evidence: REORG_REVERSAL journals must descend in block height.
const heightOf = (ref) => {
  const f = ref.split(":");
  if (ref.startsWith("reorgdep:")) return f[1] === depId2 ? Number(depBlk2) : -1;
  if (ref.startsWith("reorgsweepitem:")) return f[2] === sweAttId ? Number(sweBlk) : -1;
  if (ref.startsWith("reorgpayoutexec:")) return f[1] === payAttId ? Number(payBlk) : -1;
  if (ref.startsWith("reorggas:")) {
    if (f[1] === payAttId) return Number(payBlk);
    if (f[1] === sweAttId) return Number(sweBlk);
  }
  return -1;
};
const revRefs = reorgReversals().map((l) => l.split(" ")[0]).filter((r) => heightOf(r) > 0);
const revHeights = revRefs.map(heightOf);
console.log("  reversal journals:", revRefs.map((r, i) => `${r.split(":")[0]}@h${revHeights[i]}`));
check(revRefs.some((r) => r.startsWith("reorgdep:")), "reorgdep journal present (deposit finalize reversed)");
check(revRefs.some((r) => r.startsWith("reorgsweepitem:")), "reorgsweepitem journal present (sweep settle reversed)");
check(revRefs.some((r) => r.startsWith("reorgpayoutexec:")), "reorgpayoutexec journal present (payout exec reversed)");
check(revHeights.every((h, i) => i === 0 || h <= revHeights[i - 1]),
  `LIFO: reversal heights strictly non-increasing [${revHeights.join(",")}]`);
check(
  revRefs.findIndex((r) => r.startsWith("reorgpayoutexec:")) <
    revRefs.findIndex((r) => r.startsWith("reorgdep:")),
  "payout reversal (highest) precedes deposit reversal (lowest)",
);

const maNow = ledgerBal("MERCHANT_AVAILABLE", { owner: MERCHANT_ID, asset: "USDC" });
console.log(`  MERCHANT_AVAILABLE now=${maNow} (priorMA=${priorMA}) receivable=${rev.recv}`);
check(rev.recv === settleNet,
  `MERCHANT_REORG_RECEIVABLE == settleNet(${settleNet}) — payout drained all MA pre-fork = ${rev.recv}`);
check(merchantFrozen(), "merchants.payout_frozen == true (auto-set by deep unwind)");

// ---- re-broadcast outcomes on the winning fork -------------------------------
const payRe = await waitFor("payout re-broadcast re-completes", async () =>
  payoutRow(payoutUuid)?.startsWith("COMPLETED") ? true : null, { timeoutMs: 240_000 });
check(!!payRe, "payout intent re-signed (signer slot replay) + re-COMPLETED on new fork");
const v2ItemsPost = await waitFor("V2 item QUARANTINED_REVERT on new fork", async () => {
  const r = sweepItemRows(o2.deposit_address);
  return r.some((l) => l.includes("QUARANTINED_REVERT")) ? r : null;
}, { timeoutMs: 240_000 });
console.log("  V2 items post-reorg:", v2ItemsPost);
check(v2ItemsPost.some((l) => l.includes("QUARANTINED_REVERT")),
  "re-broadcast sweep quarantines V2 (vault empty on new fork) — per-item isolation");
check(vaultFlags(o2.deposit_address).startsWith("true"),
  "V2 deposit_addresses.is_quarantined == true");
checkEq(ledgerBal("CUSTODY_COLD", { asset: "USDC" }), ledColdPre - BigInt(O2_AMT),
  "CUSTODY_COLD dropped by the reversed 3.0 USDC sweep");

// ---- freeze enforcement + ops resolution --------------------------------------
section("冻结强制 + 运营核销 → 解冻");
try {
  await createPayout({ to: W, amount: "1", asset: "USDC" });
  check(false, "payout while frozen MUST be rejected");
} catch (e) {
  check(e instanceof GatewayError && [400, 409, 422].includes(e.status),
    `payout while frozen rejected (${e.status} ${e.code ?? ""})`);
}
try {
  await unfreezeMerchant();
  check(false, "unfreeze while receivable>0 MUST 409");
} catch (e) {
  check(e instanceof GatewayError && e.status === 409,
    `unfreeze blocked: 409 ${e.code ?? ""} (receivable in details)`);
}

// No REST write-off endpoint exists (ops gap — see README): simulate the ops
// bad-debt write-off as a real double-entry posting inside one SQL tx.
const recvRows = qRows(`SELECT id||' '||chain_id||' '||asset_symbol||' '||balance
  FROM ledger_accounts WHERE owner_merchant_id='${MERCHANT_ID}'
    AND account_type='MERCHANT_REORG_RECEIVABLE' AND balance>0`);
check(recvRows.length > 0, `receivable account rows present (${recvRows.length})`);
for (const r of recvRows) {
  const [accId, cid, asset, bal] = r.split(" ");
  const eq = q1(`SELECT id||' '||balance FROM ledger_accounts
    WHERE owner_merchant_id='${ZERO_UUID}' AND account_type='TREASURY_EQUITY_CAPITAL'
      AND chain_id=${cid} AND asset_symbol='${asset}' AND balance>=${bal} LIMIT 1`);
  check(!!eq, `TREASURY_EQUITY_CAPITAL covers write-off ${bal} ${asset}@${cid}`);
  if (!eq) continue;
  const [eqId, eqBal] = eq.split(" ");
  const txid = uid();
  psql(`BEGIN;
    INSERT INTO ledger_transactions(id,reference_type,reference_id,description)
      VALUES ('${txid}','INTERNAL_TREASURY_REBALANCE','ops-writeoff-${txid}',
              'ops bad-debt write-off of MERCHANT_REORG_RECEIVABLE (equity absorbs)');
    INSERT INTO ledger_entries(transaction_id,account_id,direction,amount,balance_after)
      VALUES ('${txid}','${eqId}','DEBIT',${bal},${BigInt(eqBal) - BigInt(bal)});
    INSERT INTO ledger_entries(transaction_id,account_id,direction,amount,balance_after)
      VALUES ('${txid}','${accId}','CREDIT',${bal},0);
    UPDATE ledger_accounts SET balance=balance-${bal} WHERE id='${eqId}';
    UPDATE ledger_accounts SET balance=0 WHERE id='${accId}';
    COMMIT;`);
}
checkEq(receivableBal("USDC"), 0n, "receivable written off to 0 (ops posting)");
const un = await unfreezeMerchant();
check(un.status === 200, `unfreeze succeeds after write-off (${un.status})`);
check(!merchantFrozen(), "payout_frozen cleared");

// The deep unwind drained MERCHANT_AVAILABLE — top it up via a fresh paid
// order so the post-unfreeze payout has a lockable source.
const { data: o3 } = await createOrder({ amount: "10000", asset: "USDC", ref: "w3s1-recover" });
await waitTx(await sendToken(payerWallet, USDC, o3.deposit_address, 10_000n));
await waitFor("o3 PAID (MA topped up)", async () =>
  orderStatusRow(o3.id)?.startsWith("PAID") ? true : null, { timeoutMs: 120_000 });

const { payoutUuid: p2, data: po2 } = await createPayout({ to: W, amount: "1", asset: "USDC" });
check(po2.status === "LOCKED_QUEUED", `fresh payout ${p2.slice(0, 8)}… accepted post-unfreeze`);
const cx = await cancelPayout(p2);
check([200, 409].includes(cx.status), `cleanup cancel payout (${cx.status} ${cx.data?.status ?? ""})`);

console.log(`\n  summary: orphan_tx=${d1.slice(0, 14)}… resend=${d2.slice(0, 14)}…
  reversal heights=[${revHeights.join(",")}]
  receivable_written_off=${rev.recv} order2=${o2.id} payout=${payoutUuid}`);
finish("W3-S1");

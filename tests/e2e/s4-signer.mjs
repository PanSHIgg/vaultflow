// W3-S4 — 签名机渗透 + 同 nonce RBF/GAP_FILLER 双字段幂等槽
//   (ARCHITECTURE §8.4 / plan.md Wave-3 S4)
//
//   S4a sim scenario `signer-penetration`: 4 类伪造 SignTx -> 期望全拒
//       (冷金库篡改/salt 篡改/未白名单或冷却中收款人/伪造 RefundTicket)
//       + 合法 GAP_FILLER 签名 + run 内 VerifyAuditChain.
//   S4b E2E 独立审计: JS 侧按冻结公式逐条重算 SHA-256 哈希链.
//   S4c 冷却拒签→GAP_FILLER: 新白名单地址即时提现 -> signer
//       WHITELIST_COOLDOWN 拒签 -> 网关节点自动同 nonce GAP_FILLER 广播 ->
//       intent CANCELLED_BY_FILLER + slot(kind=GAP_FILLER, 无业务字段) +
//       24h 用量零扣减.
//   S4d DROPPED→同 nonce 重发: automine 暂停 -> 提现广播 -> 丢交易 ->
//       DROPPED 重发同 nonce (slot Replay, 不二次扣限额) -> 终态 COMPLETED.
//
// Run: node tests/e2e/s4-signer.mjs   (WSL, stack up, signer.db readable)

import {
  attemptRows,
  auditRows,
  autoMine,
  cancelPayout,
  CHAIN_ID,
  check,
  checkEq,
  coldWalletA,
  createOrder,
  createPayout,
  dropTx,
  ensureCooldown,
  finish,
  generatePrivateKey,
  intentRows,
  ledgerBal,
  MERCHANT_ID,
  mineA,
  mintToken,
  orderStatusRow,
  PAYER,
  payerWallet,
  payoutRow,
  PAYOUT_A,
  privateKeyToAccount,
  q1,
  registerWhitelist,
  section,
  sendToken,
  setAutomine,
  signerSlots,
  signerUsageCount,
  simScenario,
  tokenBal,
  USDT,
  verifyAuditChainLocal,
  waitFor,
  waitSimRun,
  waitTx,
} from "./lib.mjs";

const bytes16Hex = (uuid) => uuid.replace(/-/g, "").toLowerCase();

console.log("=== W3-S4 签名机渗透 + 同 nonce 双字段幂等槽 ===\n");
await setAutomine(true);
let mining = autoMine(2500);

// --- ensure MA_USDT + hot USDT so the legs are self-sufficient ---------------
if (ledgerBal("MERCHANT_AVAILABLE", { owner: MERCHANT_ID, asset: "USDT" }) < 700_000n) {
  const { data: seed } = await createOrder({ amount: "800000", asset: "USDT", ref: "w3s4-seed" });
  if ((await tokenBal(USDT, PAYER)) < 1_000_000n) await waitTx(await mintToken(USDT, PAYER, 3_000_000n));
  await waitTx(await sendToken(payerWallet, USDT, seed.deposit_address, 800_000n));
  await waitFor("seed order PAID", async () => orderStatusRow(seed.id)?.startsWith("PAID") ? true : null, { timeoutMs: 120_000 });
}
if ((await tokenBal(USDT, PAYOUT_A)) < 700_000n) {
  await waitTx(await sendToken(coldWalletA, USDT, PAYOUT_A, 1_000_000n));
  await waitFor("hot topup FINALIZED", async () =>
    q1(`SELECT status FROM onchain_deposits WHERE chain_id=${CHAIN_ID} AND to_address='${PAYOUT_A}'
          AND deposit_category='TREASURY_REBALANCE' ORDER BY block_number DESC, log_index DESC LIMIT 1`) === "FINALIZED" ? true : null,
  { timeoutMs: 90_000 });
}

// register W2 now — its 60s signer cooldown elapses while S4a runs.
const W2 = privateKeyToAccount(generatePrivateKey()).address.toLowerCase();
const wl2 = await registerWhitelist(W2);
check(wl2.status === 201, `dance recipient ${W2.slice(0, 10)}… whitelisted`);
const wl2At = wl2.registeredAt;

// ===========================================================================
section("S4a sim scenario signer-penetration：4 类越权盲签 100% 拒签");
// ===========================================================================
const { data: run0 } = await simScenario("signer-penetration", { include_legit_dance: "true" });
const run = await waitSimRun(run0.run_id, 240_000);
for (const s of run.steps ?? []) console.log(`    [${s.status}] ${s.name} — ${s.detail ?? ""}`);
checkEq(run.status, "COMPLETED", "scenario run COMPLETED");
const S = run.summary ?? {};
check(S.all_rejected === true, `summary.all_rejected (reject_rate=${S.reject_rate})`);
checkEq(S.reject_rate, "4/4", "all four forgery classes rejected");
checkEq((S.rejected_requests ?? []).length, 4, "rejected_requests[] has 4 entries");
for (const r of S.rejected_requests ?? []) console.log(`    forged ${r.name}: result=${r.result} ${r.reason ?? r.reject_code ?? ""}`);
check(S.audit_chain_valid === true, `summary.audit_chain_valid (entries=${S.audit_entries})`);
const legit = (S.legit_requests ?? [])[0];
check(legit?.result === "SIGNED",
  `legit same-nonce GAP_FILLER signed cleanly (hard acceptance; got=${legit?.result} ${legit?.slot ?? ""})`);

// ===========================================================================
section("S4b 独立审计链校验（JS 重算冻结公式）");
// ===========================================================================
const vac = verifyAuditChainLocal();
check(vac.valid === true && vac.entries > 0,
  `audit chain re-hash valid — ${vac.entries} entries (firstBad=${vac.firstBad ?? "-"})`);

// ===========================================================================
section("S4c 冷却拒签 → 同 nonce GAP_FILLER（filler_created 槽，无业务字段）");
// ===========================================================================
const W3 = privateKeyToAccount(generatePrivateKey()).address.toLowerCase();
const wl3 = await registerWhitelist(W3);
check(wl3.status === 201, `fresh recipient ${W3.slice(0, 10)}… whitelisted (inside 60s cooldown)`);

const auditStart = auditRows().length;
const usage0 = signerUsageCount(CHAIN_ID);
const { payoutUuid: pu3 } = await createPayout({ to: W3, amount: "500000", asset: "USDT" });
check(true, `payout ${pu3.slice(0, 8)}… accepted by gateway API (whitelist ACTIVE there)`);

const fillerDone = await waitFor("cooldown reject -> GAP_FILLER cancel", async () => {
  const row = payoutRow(pu3);
  const its = intentRows(`reference_id='${pu3}'`);
  if (!row || !its.length) return null;
  const iid = its[0].split(" ")[0];
  const atts = attemptRows(iid);
  const gf = atts.find((a) => a.split(" ")[2] === "GAP_FILLER" && a.split(" ")[3] === "FINALIZED");
  return row.startsWith("FAILED_RETRYABLE") && gf ? { row, its: its[0], gf, iid } : null;
}, { timeoutMs: 300_000, intervalMs: 2_500 });
console.log(`  intent ${fillerDone.iid.slice(0, 8)}… ${fillerDone.its.split(" ")[2]} payout=${fillerDone.row.split(" ")[0]}`);
check(fillerDone.its.split(" ")[2] === "CANCELLED_BY_FILLER", "intent CANCELLED_BY_FILLER after signer cooldown reject");
check(fillerDone.gf.split(" ")[6] === "1", "GAP_FILLER attempt mined successfully (receipt=1)");
checkEq(signerUsageCount(CHAIN_ID), usage0, "24h usage rows +0 — rejected payout never deducted");

const fillerNonce = fillerDone.its.split(" ")[3];
const slots3 = signerSlots(`chain_id=${CHAIN_ID} AND lower(from_wallet)=lower('${PAYOUT_A}') AND nonce=${fillerNonce}`);
console.log("  slot:", slots3);
const sl3 = slots3[0]?.split("|") ?? [];
check(sl3[3] === "GAP_FILLER", `slot@nonce${fillerNonce} kind=GAP_FILLER (filler_created)`);
check(!sl3[4], "slot payout_uuid empty — no business fields on a filler slot");
check(sl3[9] === "1", "slot filler_signed=1");
const fillerAudit = auditRows().slice(auditStart);
check(fillerAudit.some((l) => l.split("|")[4] === "REJECTED"),
  "audit: REJECTED entry recorded for the cooldown payout request");
check(fillerAudit.some((l) => l.split("|")[4] === "APPROVED" && l.split("|")[5].includes("filler_created")),
  "audit: APPROVED entry 'slot=filler_created' for the GAP_FILLER");

const cx3 = await cancelPayout(pu3);
check(cx3.status === 200, `cleanup: failed payout cancelled (${cx3.status})`);

// ===========================================================================
section("S4d DROPPED → 同 nonce 重发（slot Replay，不二次扣限额）");
// ===========================================================================
await ensureCooldown(wl2At);
const usageD0 = signerUsageCount(CHAIN_ID);

await setAutomine(false); // halt mining so the broadcast can be dropped
const { payoutUuid: pu4 } = await createPayout({ to: W2, amount: "500000", asset: "USDT" });
const it4 = await waitFor("dance intent attempt BROADCASTED", async () => {
  const its = intentRows(`reference_id='${pu4}'`);
  if (!its.length) return null;
  const atts = attemptRows(its[0].split(" ")[0]);
  const a = atts.find((x) => x.split(" ")[3] === "BROADCASTED");
  return a ? { its: its[0], att: a } : null;
}, { timeoutMs: 120_000 });
const [it4Id, , , nonce4] = it4.its.split(" ");
const droppedHash = it4.att.split(" ")[4];
console.log(`  broadcast ${droppedHash.slice(0, 14)}… @nonce${nonce4} — dropping from mempool`);
await dropTx(droppedHash);
await setAutomine(true);
await mineA(2);

const replay = await waitFor("DROPPED re-send same nonce -> COMPLETED", async () => {
  const row = payoutRow(pu4);
  return row?.startsWith("COMPLETED") ? row : null;
}, { timeoutMs: 300_000, intervalMs: 3_000 });
check(!!replay, `payout ${pu4.slice(0, 8)}… COMPLETED via same-nonce resend`);
const atts4 = attemptRows(it4Id);
console.log("  attempts:", atts4);
check(atts4.filter((a) => a.split(" ")[3] === "FINALIZED").length === 1, "exactly one FINALIZED attempt after resend");
check(atts4.some((a) => Number(a.split(" ")[1]) >= 2), "resend recorded as attempt_no>=2 (DROPPED path)");

checkEq(signerUsageCount(CHAIN_ID), usageD0 + 1,
  "24h usage rows +1 — same-nonce replay does NOT double-deduct the limit");
const slots4 = signerSlots(`chain_id=${CHAIN_ID} AND lower(from_wallet)=lower('${PAYOUT_A}') AND nonce=${nonce4}`);
console.log("  slot:", slots4);
const sl4 = slots4[0]?.split("|") ?? [];
check(sl4[3] === "PAYOUT", `slot@nonce${nonce4} kind=PAYOUT`);
check(sl4[4]?.toLowerCase() === bytes16Hex(pu4), `slot payout_uuid == ${pu4} (raw16)`);
checkEq(sl4[5], 0, "slot retry_seq=0");
check((sl4[6] ?? "").toLowerCase() === USDT.toLowerCase().slice(2), "slot token == USDT raw20");
check((sl4[7] ?? "").toLowerCase() === W2.slice(2), "slot recipient == W2 raw20");
checkEq(BigInt(`0x${sl4[8] ?? "0"}`), 500_000n, "slot amount == 500000 (u256 BE)");
checkEq(sl4[9], 0, "slot filler_signed=0 (business slot untouched by replay)");

const audit4 = auditRows();
const signedSameNonce = audit4.filter((l) => {
  const f = l.split("|");
  return f[4] === "APPROVED" && f[5].includes(`nonce=${nonce4} `) && f[5].includes("slot=");
});
console.log("  same-nonce sign reasons:", signedSameNonce.map((l) => l.split("|")[5]));
check(signedSameNonce.length >= 2, `audit: >=2 APPROVED entries at nonce=${nonce4} (fresh + replay)`);
check(signedSameNonce.some((l) => l.split("|")[5].includes("slot=replay")),
  "audit: resend note contains 'slot=replay' (immutable triple match)");

console.log(`\n  summary: forgeries=${S.forged_requests} rejected=${S.reject_rate} audit_chain=${vac.entries}
  filler_slot@${fillerNonce} payout_slot@${nonce4} usage_delta=+1`);
finish("W3-S4");

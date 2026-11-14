// W3-S2 — 黑名单/ReturnBomb 单元素隔离 + Revert Gas 1-Wei 账平
//   (ARCHITECTURE §8.2 / plan.md Wave-3 S2)
//
//   S2a USDT 黑名单: 3 个 USDT 金库同批归集, V3 在归集前被拉黑 ->
//      批内逐项隔离: V1/V2 VaultSwept, V3 VaultQuarantined ->
//      batch PARTIAL_QUARANTINED + item QUARANTINED_REVERT +
//      deposit_addresses.is_quarantined.
//   S2b ReturnBomb : MockReturnBombToken.transfer 恒返回 2MiB -> 内层
//      call 在 120k gas cap 内 OOG -> 金库 revert -> sweeper try/catch ->
//      VaultQuarantined; 外层交易成功且 gas 有界 (2MiB 内存扩张不外溢).
//   S2c Revert Gas : 出向 USDT 退款发往黑名单地址 -> 链上 revert ->
//      attempt FINALIZED receipt=0 + intent FAILED_REVERTED +
//      GAS_SETTLE 记账 + HOT_WALLET_NATIVE 与链上余额 1-Wei 对齐.
//
// Run: node tests/e2e/s2-quarantine-gas.mjs   (WSL, stack up)

import { readFileSync } from "node:fs";
import path from "node:path";
import {
  attemptRows,
  autoMine,
  cancelPayout,
  CHAIN_ID,
  check,
  checkEq,
  coldWalletA,
  COLD_A,
  createOrder,
  createSuspenseRefund,
  depRowsForOrder,
  finish,
  funder,
  generatePrivateKey,
  intentRows,
  isUsdtBlacklisted,
  keccak256,
  ledgerBal,
  MERCHANT_ID,
  mintToken,
  nativeSnapshotCheck,
  orderStatusRow,
  PAYER,
  payerWallet,
  payoutRow,
  PAYOUT_A,
  predictVault,
  privateKeyToAccount,
  psql,
  pub,
  q1,
  quiesce,
  REPO_ROOT,
  relayerRpc,
  RELAYER_A,
  section,
  sendToken,
  setAutomine,
  setUsdtBlacklist,
  SWEEPER_A,
  stopImpersonate,
  impersonate,
  sweepItemRows,
  sweeperAbi,
  tokenAbi,
  tokenBal,
  toBytes,
  uid,
  USDC,
  USDT,
  vaultFlags,
  waitFor,
  waitTx,
  ZERO_UUID,
  qRows,
} from "./lib.mjs";

const M = 1_000_000n;
const SWEPT_TOPIC = keccak256(toBytes("VaultSwept(address,bytes32,uint256)"));
const QUAR_TOPIC = keccak256(toBytes("VaultQuarantined(address,bytes32,uint256)"));

console.log("=== W3-S2 黑名单/ReturnBomb 隔离 + Revert Gas 账平 ===\n");
await setAutomine(true);
let mining = autoMine(2500);

if ((await tokenBal(USDT, PAYER)) < 12_000_000n) await waitTx(await mintToken(USDT, PAYER, 20_000_000n));

// ===========================================================================
section("S2a USDT 黑名单：同批三金库 2 成 1 隔离");
// ===========================================================================
const mk = async (amt, tag) => {
  const { data: o } = await createOrder({ amount: String(amt), asset: "USDT", ref: `w3s2-${tag}` });
  await waitTx(await sendToken(payerWallet, USDT, o.deposit_address, amt));
  return o;
};
// serialized — concurrent writeContract calls race on the payer's nonce.
const o1 = await mk(1_000_000n, "a");
const o2 = await mk(2_000_000n, "b");
const o3 = await mk(3_000_000n, "c");
console.log(`  vaults: ${o1.deposit_address} ${o2.deposit_address} ${o3.deposit_address}`);

// blacklist V3 *before* its vault can be swept (deposit tx already mined —
// MockUSDT blocks transfers in BOTH directions so blacklisting must come after
// the inbound deposit lands).
await waitFor("3 deposits DETECTED", async () =>
  [o1, o2, o3].every((o) => depRowsForOrder(o.id).some((r) => r.includes("DETECTED") || r.includes("FINALIZED")))
    ? true : null, { timeoutMs: 90_000 });
await waitTx(await setUsdtBlacklist(o3.deposit_address, true));
check(await isUsdtBlacklisted(o3.deposit_address), `V3 vault blacklisted on MockUSDT`);

await waitFor("3 orders PAID", async () =>
  [o1, o2, o3].every((o) => orderStatusRow(o.id)?.startsWith("PAID")) ? true : null,
{ timeoutMs: 120_000 });
check(true, "inbound deposits finalize normally — blacklist only blocks OUTBOUND");

const coldUsdt0 = await tokenBal(USDT, COLD_A);
const batchRows = await waitFor("USDT sweep batch terminal", async () => {
  const r1 = sweepItemRows(o1.deposit_address);
  const r2 = sweepItemRows(o2.deposit_address);
  const r3 = sweepItemRows(o3.deposit_address);
  const done = [...r1, ...r2, ...r3].every((l) => /SWEPT|QUARANTINED/.test(l));
  return r1.length && r2.length && r3.length && done ? [...r1, ...r2, ...r3] : null;
}, { timeoutMs: 300_000, intervalMs: 3_000 });
console.log("  items:", batchRows);
check(batchRows.filter((l) => l.includes("SWEPT")).length === 2, "V1+V2 items SWEPT in one batch");
check(batchRows.some((l) => l.includes("QUARANTINED_REVERT") && l.includes("PARTIAL_QUARANTINED")),
  "V3 item QUARANTINED_REVERT; batch PARTIAL_QUARANTINED");
check(vaultFlags(o3.deposit_address).startsWith("true"), "V3 deposit_addresses.is_quarantined == true");
checkEq((await tokenBal(USDT, COLD_A)) - coldUsdt0, 3_000_000n, "cold USDT += exactly V1+V2 (3.0)");

// on-chain receipt: exactly one terminal event per item (2 Swept + 1 Quarantined)
const batchIntent = intentRows(`intent_type='SWEEP_BATCH' AND status='CONFIRMED'`).at(-1);
const finAtt = attemptRows(batchIntent.split(" ")[0]).find((a) => a.split(" ")[3] === "FINALIZED");
const rcpt = await pub.getTransactionReceipt({ hash: finAtt.split(" ")[4] });
const sweptEv = rcpt.logs.filter((l) => l.topics[0] === SWEPT_TOPIC);
const quarEv = rcpt.logs.filter((l) => l.topics[0] === QUAR_TOPIC);
checkEq(sweptEv.length, 2, "receipt: 2× VaultSwept");
checkEq(quarEv.length, 1, "receipt: 1× VaultQuarantined (exactly one terminal event per item)");

// ===========================================================================
section("S2b ReturnBomb：2MiB returndata 被 120k 内层 gas cap 熔断隔离");
// ===========================================================================
const art = JSON.parse(readFileSync(
  path.join(REPO_ROOT, "contracts/out/MockReturnBombToken.sol/MockReturnBombToken.json"), "utf8"));
const bombHash = await funder.sendTransaction({ data: art.bytecode.object });
const bombRcpt = await waitTx(bombHash);
const BOMB = bombRcpt.contractAddress.toLowerCase();
console.log(`  MockReturnBombToken deployed @ ${BOMB}`);

// fabricated vault salt — predict + mint BOMB to it before the lazy deploy.
const salt = `0x${"ab".repeat(32)}`;
const bombVault = (await predictVault(salt)).toLowerCase();
await waitTx(await funder.writeContract({ address: BOMB, abi: tokenAbi, functionName: "mint", args: [bombVault, M] }));
checkEq(await tokenBal(BOMB, bombVault), M, "predicted vault funded with 1.0 BOMB");

// the batch call must come from the relayer hot wallet — impersonate it on anvil.
await impersonate(RELAYER_A);
const bombTx = await relayerRpc.writeContract({
  address: SWEEPER_A, abi: sweeperAbi,
  functionName: "deployAndSweepBatch",
  args: [[{ salt, exactAmount: M }], BOMB, COLD_A],
});
const bombSweepRcpt = await waitTx(bombTx);
await stopImpersonate(RELAYER_A);
// The impersonated call consumed a relayer nonce out-of-band — the txmanager's
// allocator never saw it, so any intent signed at that nonce would hit
// "nonce too low" forever (broadcast-retry loop is a known gap — the
// NONCE_STOLEN_HALT detector only covers already-broadcast attempts; see
// README findings). Re-sync the cursor as the ops-runbook equivalent.
psql(`UPDATE wallet_nonce_cursors SET next_nonce=next_nonce+1
      WHERE chain_id=${CHAIN_ID} AND lower(wallet_address)='${RELAYER_A}'`);
check(bombSweepRcpt.status === "success", `batch tx succeeds despite poisoned item (gasUsed=${bombSweepRcpt.gasUsed})`);
check(bombSweepRcpt.gasUsed < 3_000_000n,
  `outer gas ${bombSweepRcpt.gasUsed} bounded — 2MiB expansion contained by 120k inner cap`);
const bombQuar = bombSweepRcpt.logs.filter((l) => l.topics[0] === QUAR_TOPIC);
checkEq(bombQuar.length, 1, "exactly 1× VaultQuarantined — per-item isolation at the EVM boundary");
checkEq(bombSweepRcpt.logs.filter((l) => l.topics[0] === SWEPT_TOPIC).length, 0, "no VaultSwept for the bomb item");

// ===========================================================================
section("S2c Revert 交易原生 Gas 1-Wei 账平");
// ===========================================================================
// payout hot wallet needs USDT to attempt the (doomed) transfer
await waitTx(await sendToken(coldWalletA, USDT, PAYOUT_A, 1_000_000n));
await waitFor("rebalance FINALIZED", async () =>
  q1(`SELECT status FROM onchain_deposits WHERE chain_id=${CHAIN_ID} AND to_address='${PAYOUT_A}'
        AND deposit_category='TREASURY_REBALANCE' AND amount=1000000 ORDER BY block_number DESC, log_index DESC LIMIT 1`) === "FINALIZED" ? true : null,
{ timeoutMs: 90_000 });

// suspense source: overpaid USDT order
const { data: ov } = await createOrder({ amount: "1000000", asset: "USDT", ref: "w3s2-ov" });
await waitTx(await sendToken(payerWallet, USDT, ov.deposit_address, 1_500_000n));
await waitFor("overpaid order OVERPAID", async () => orderStatusRow(ov.id)?.startsWith("OVERPAID") ? true : null, { timeoutMs: 120_000 });
const suspense0 = ledgerBal("SUSPENSE_OVERPAY_LIAB", { owner: MERCHANT_ID, asset: "USDT" });
check(suspense0 >= 500_000n, `SUSPENSE_OVERPAY_LIAB_USDT = ${suspense0} >= 0.5`);

const victim = privateKeyToAccount(generatePrivateKey()).address.toLowerCase();
await waitTx(await setUsdtBlacklist(victim, true));
check(await isUsdtBlacklisted(victim), `refund recipient ${victim.slice(0, 10)}… blacklisted`);

const { payoutUuid: rp } = await createSuspenseRefund({
  source: "SUSPENSE_OVERPAY_LIAB", asset: "USDT", amount: "500000", to: victim, orderId: ov.id,
});
await waitFor("revert refund FAILED_RETRYABLE", async () =>
  payoutRow(rp)?.startsWith("FAILED_RETRYABLE") ? true : null, { timeoutMs: 240_000 });
check(true, "reverted on-chain refund -> payout FAILED_RETRYABLE");
const rpIntent = intentRows(`intent_type='MERCHANT_PAYOUT' AND reference_id='${rp}'`)[0];
check(rpIntent.includes("FAILED_REVERTED"), `intent FAILED_REVERTED (${rpIntent.split(" ")[2]})`);
const rpAtt = attemptRows(rpIntent.split(" ")[0]).find((a) => a.split(" ")[3] === "FINALIZED");
const [rpAttId, , , , , , rpReceipt, rpGas, rpEgp] = rpAtt.split(" ");
checkEq(rpReceipt, "0", "attempt receipt_status=0 (reverted)");
const gasAmt = BigInt(rpGas) * BigInt(rpEgp);
const gasLegs = q1(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries e
  JOIN ledger_transactions t ON t.id=e.transaction_id
  JOIN ledger_accounts a ON a.id=e.account_id
  WHERE t.reference_type='GAS_SETTLE' AND t.reference_id LIKE 'gas:${rpAttId}:c%'
    AND a.account_type='GAS_EXPENSE' AND e.direction='DEBIT'`);
checkEq(gasLegs, gasAmt, `GAS_SETTLE booked == gas_used(${rpGas})×egp(${rpEgp}) = ${gasAmt} wei`);

// The impersonated S2b call burned real relayer gas off-ledger. Book the ops
// reconciliation posting — the same DR GAS_EXPENSE / CR HOT_WALLET_NATIVE
// template the indexer writes for every finalized attempt — so native custody
// stays 1-wei exact for all later snapshots.
const offbookGas = bombSweepRcpt.gasUsed * bombSweepRcpt.effectiveGasPrice;
const [gasId, gasBal] = q1(`SELECT id||' '||balance FROM ledger_accounts
  WHERE owner_merchant_id='${ZERO_UUID}' AND chain_id=${CHAIN_ID}
    AND account_type='GAS_EXPENSE' AND asset_symbol='ETH'`).split(" ");
const [natId, natBal] = q1(`SELECT id||' '||balance FROM ledger_accounts
  WHERE owner_merchant_id='${ZERO_UUID}' AND chain_id=${CHAIN_ID}
    AND account_type='HOT_WALLET_NATIVE' AND asset_symbol='ETH'`).split(" ");
const wtid = uid();
psql(`BEGIN;
  INSERT INTO ledger_transactions(id,reference_type,reference_id,description)
    VALUES ('${wtid}','GAS_SETTLE','ops-impersonated-gas-${wtid}',
            'ops gas reconcile: anvil-impersonated sweeper call (off-book relayer spend)');
  INSERT INTO ledger_entries(transaction_id,account_id,direction,amount,balance_after)
    VALUES ('${wtid}','${gasId}','DEBIT',${offbookGas},${BigInt(gasBal) + offbookGas});
  INSERT INTO ledger_entries(transaction_id,account_id,direction,amount,balance_after)
    VALUES ('${wtid}','${natId}','CREDIT',${offbookGas},${BigInt(natBal) - offbookGas});
  UPDATE ledger_accounts SET balance=balance+${offbookGas} WHERE id='${gasId}';
  UPDATE ledger_accounts SET balance=balance-${offbookGas} WHERE id='${natId}';
  COMMIT;`);

// Drain every in-flight outbound leg first so all on-chain gas spend is booked
// by GAS_SETTLE before the native balance is compared 1-wei exactly.
await quiesce(CHAIN_ID);
const nat = await nativeSnapshotCheck(CHAIN_ID, pub, RELAYER_A, PAYOUT_A);
checkEq(nat.delta, 0n,
  `HOT_WALLET_NATIVE 1-wei exact vs on-chain after ops gas posting (ledger=${nat.ledger} chain=${nat.onchain} @h${nat.safeHead})`);

const c = await cancelPayout(rp);
check(c.status === 200 && c.data?.status === "CANCELLED_UNLOCKED",
  `failed refund cancelled -> CANCELLED_UNLOCKED (suspense restored)`);
checkEq(ledgerBal("SUSPENSE_OVERPAY_LIAB", { owner: MERCHANT_ID, asset: "USDT" }),
  suspense0, "SUSPENSE_OVERPAY_LIAB restored after cancel");
await waitTx(await setUsdtBlacklist(victim, false));
await waitTx(await setUsdtBlacklist(o3.deposit_address, false));

console.log(`\n  summary: batch PARTIAL_QUARANTINED(2 swept/1 quar) bomb_quar=${bombQuar.length}
  revert_payout=${rp} gas_booked=${gasAmt} native_delta=${nat.delta}`);
finish("W3-S2");

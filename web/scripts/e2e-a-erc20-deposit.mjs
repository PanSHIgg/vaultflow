// I2 — Link A: real ERC-20 cashier deposit E2E.
//   POST /v1/orders -> vault address -> payer USDC transfer -> indexer detects
//   -> SSE snapshot/block_detected/confirmation_progress/paid -> status PAID.
// Run:  node web/scripts/e2e-a-erc20-deposit.mjs

import {
  CHAIN_ID,
  USDC,
  PAYER,
  api,
  autoMine,
  watchOrder,
  createOrder,
  getPublicPayment,
  mineBlocks,
  mintUsdc,
  ok,
  payerWallet,
  pub,
  sleep,
  transferUsdc,
  uid,
  usdcBalanceOf,
  waitOrderStatus,
  waitTx,
} from "./e2e-lib.mjs";

const AMOUNT = "1500000"; // 1.5 USDC (6 decimals)

console.log("=== Link A — ERC-20 收银台入账 E2E (chain", CHAIN_ID, ") ===\n");

// 1) create the order via the real merchant API (X-Api-Key + Idempotency-Key)
const { status: createStatus, data: order } = await createOrder({ amount: AMOUNT, ref: `i2-a-${uid().slice(0, 8)}` });
ok(createStatus === 201, `POST /v1/orders -> 201 (order ${order.id})`);
ok(order.deposit_address?.startsWith("0x"), `deposit_address = ${order.deposit_address}`);
ok(order.expected_amount === AMOUNT, `expected_amount = ${order.expected_amount}`);
ok(order.status === "AWAITING_PAYMENT", `status = ${order.status}`);
ok(order.eip3009?.supported === true, `eip3009 hint present (router ${order.eip3009.router_address})`);
console.log(`  cashier_url: ${order.cashier_url}`);
const orderId = order.id;
const vault = order.deposit_address;

// 2) open the real SSE stream in the background and record every frame
const sse = watchOrder(orderId, { timeoutMs: 480_000, stop: (_f, j) => j?.status === "PAID" });

// 3) fund the payer and pay the vault exactly expected_amount
const bal = await usdcBalanceOf(PAYER);
if (bal < BigInt(AMOUNT)) {
  const m = await mintUsdc(PAYER, 10_000_000n);
  await waitTx(m);
  console.log(`  minted 10 USDC to payer (tx ${m})`);
}
console.log(`  payer ${PAYER} USDC balance: ${await usdcBalanceOf(PAYER)}`);
const txHash = await transferUsdc(payerWallet, vault, AMOUNT);
const receipt = await waitTx(txHash);
ok(receipt.status === "success", `ERC-20 transfer mined: tx_hash=${txHash} block=${receipt.blockNumber}`);

// 4) safe-head depth ~10 blocks on this deployment: keep mining while the
//    indexer scans+finalizes the deposit.
await mineBlocks(45);
const stopMining = autoMine(2500);

// 5) authoritative progression: REST polling; the SSE window stays open in
//    parallel and (post FIX-G BUG-2) MUST carry the real business frames:
//    snapshot -> block_detected -> [confirmation_progress] -> paid.
await waitOrderStatus(orderId, ["PAID", "OVERPAID"], 420_000);
stopMining();
await sleep(15_000); // grace window for the trailing 'paid' frame
sse.stop();
const frames = await sse.done;
const names = frames.map((f) => f.event);
console.log(`\n  SSE frames received: ${names.length ? names.join(" -> ") : "(none beyond snapshot)"}`);
ok(frames.some((f) => f.event === "snapshot"), "SSE snapshot received");
ok(frames.some((f) => f.event === "block_detected"), "SSE 'block_detected' emitted (DEPOSIT_DETECTED post-commit)");
const paid = frames.find((f) => f.event === "paid");
ok(!!paid, "SSE 'paid' emitted (STATUS_CHANGED post-commit)");
console.log(`  SSE 'paid' event: id=${paid.id} tx=${paid.json?.tx_hash}`);

// 6) final REST assertion
const { data: pFinal } = await getPublicPayment(orderId);
ok(pFinal.status === "PAID", `GET /v1/public/payments/{id} status = ${pFinal.status}`);
ok(pFinal.received_amount === AMOUNT, `received_amount = ${pFinal.received_amount}`);
const vaultBal = await usdcBalanceOf(vault);
console.log(`  vault USDC balance now: ${vaultBal} (0 ⇒ already swept to custody hot)`);

console.log("\n=== LINK A PASSED ===");
console.log(JSON.stringify({ order_id: orderId, vault, tx_hash: txHash, sse_events: names, status: pFinal.status }, null, 2));

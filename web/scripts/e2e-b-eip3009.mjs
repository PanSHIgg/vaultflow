// I2 — Link B: EIP-3009 gasless payment via the gateway relay pipeline.
//   POST /v1/orders -> eip3009 hint -> payer signs ReceiveWithAuthorization
//   (EIP-712, domain name="Mock USDC" v2 — the REAL deployed token domain)
//   -> POST /v1/public/payments/{id}/eip3009 -> EIP3009_RELAY intent
//   -> relayer hot wallet broadcasts payOrderWithAuthorization -> PAID.
// Run:  node web/scripts/e2e-b-eip3009.mjs

import { hexToSignature } from "viem";
import {
  CHAIN_ID,
  PAYER,
  ROUTER,
  USDC,
  api,
  autoMine,
  watchOrder,
  createOrder,
  getPublicPayment,
  ledgerAccounts,
  mineBlocks,
  mintUsdc,
  ok,
  outboundQueue,
  payerAccount,
  sleep,
  usdcBalanceOf,
  uid,
  waitOrderStatus,
  waitTx,
} from "./e2e-lib.mjs";

const AMOUNT = "2000000"; // 2 USDC

console.log("=== Link B — EIP-3009 免 Gas 路由 E2E (chain", CHAIN_ID, ") ===\n");

const { data: order } = await createOrder({ amount: AMOUNT, ref: `i2-b-${uid().slice(0, 8)}` });
const orderId = order.id;
const hint = order.eip3009;
ok(hint?.supported === true, `order ${orderId} eip3009 supported`);
ok(hint.router_address === ROUTER, `router_address = ${hint.router_address}`);
console.log(`  auth_nonce = ${hint.auth_nonce}`);
console.log(`  vault      = ${order.deposit_address}`);

// EIP-712 typed data — identical construction to web/src/lib/eip3009.ts
// (buildReceiveWithAuthorization): to=router, value=expected, validAfter=0,
// validBefore=order.expires_at, nonce=auth_nonce; domain = deployed MockUSDC.
const validBefore = BigInt(Math.floor(Date.parse(order.expires_at) / 1000));
const typedData = {
  domain: {
    name: "Mock USDC",
    version: "2",
    chainId: CHAIN_ID,
    verifyingContract: USDC,
  },
  types: {
    ReceiveWithAuthorization: [
      { name: "from", type: "address" },
      { name: "to", type: "address" },
      { name: "value", type: "uint256" },
      { name: "validAfter", type: "uint256" },
      { name: "validBefore", type: "uint256" },
      { name: "nonce", type: "bytes32" },
    ],
  },
  primaryType: "ReceiveWithAuthorization",
  message: {
    from: PAYER,
    to: ROUTER,
    value: BigInt(AMOUNT),
    validAfter: 0n,
    validBefore,
    nonce: hint.auth_nonce,
  },
};
const signature = await payerAccount.signTypedData(typedData);
const { r, s, v } = hexToSignature(signature);
const sub = {
  from: PAYER,
  to: ROUTER,
  value: AMOUNT,
  valid_after: 0,
  valid_before: Number(validBefore),
  nonce: hint.auth_nonce,
  v: Number(v),
  r,
  s,
};
console.log(`  payer signature: ${signature.slice(0, 30)}… v=${sub.v}`);

// fund the payer (the ROUTER pulls USDC from `from` via the authorization)
if ((await usdcBalanceOf(PAYER)) < BigInt(AMOUNT)) {
  const m = await mintUsdc(PAYER, 10_000_000n);
  await waitTx(m);
  console.log(`  minted 10 USDC to payer (tx ${m})`);
}

// SSE listener in background (snapshot -> block_detected -> paid)
const sse = watchOrder(orderId, { timeoutMs: 480_000, stop: (_f, j) => j?.status === "PAID" });

// FIX-G BUG-1 closed: Pool.Nonce(latest) now issues a legal "latest" nonce
// query, so a first-seen relayer hot wallet can queue its intent without any
// wallet_nonce_cursors seeding — the clean-deploy path is exercised as-is.
const submit = () =>
  api("POST", `/v1/public/payments/${orderId}/eip3009`, { apiKey: undefined, idem: `i2-${uid()}`, body: sub });

const stopMining = autoMine(2500); // keep safe_head advancing for relay tx confirm
const { status: subStatus, data: accepted } = await submit();
ok(subStatus === 202, `POST /eip3009 -> 202 Accepted`);
ok(accepted.order_id === orderId, `order_id = ${accepted.order_id}`);
console.log(`  intent_id=${accepted.intent_id} status=${accepted.intent_status}`);

// relayer broadcasts payOrderWithAuthorization; its block then needs +1 mined
// block for safe_head (required_confirmations=1).
await waitOrderStatus(orderId, ["PAID", "OVERPAID"], 300_000).catch(async () => {
  await mineBlocks(45);
  return waitOrderStatus(orderId, ["PAID", "OVERPAID"], 120_000);
});
stopMining();
await sleep(15_000); // grace for the trailing 'paid' frame
sse.stop();
const frames = await sse.done;
const names = frames.map((f) => f.event);
console.log(`\n  SSE frames received: ${names.length ? names.join(" -> ") : "(none beyond snapshot)"}`);
ok(frames.some((f) => f.event === "snapshot"), "SSE snapshot received");
ok(frames.some((f) => f.event === "block_detected"), "SSE 'block_detected' emitted (DEPOSIT_DETECTED post-commit)");
const paid = frames.find((f) => f.event === "paid");
ok(!!paid, "SSE 'paid' emitted (STATUS_CHANGED post-commit)");
console.log(`  SSE 'paid' event: id=${paid.id} tx=${paid.json?.tx_hash}`);

const { data: p } = await getPublicPayment(orderId);
ok(p.status === "PAID", `status = ${p.status}`);
ok(p.received_amount === AMOUNT, `received_amount = ${p.received_amount}`);
// Funds land at the vault then the periodic sweeper pulls them into custody —
// the on-chain vault balance check races the sweep batch, so accept either
// "still at vault" or "already swept" (ledger CUSTODY_COLD proves the move).
const vbal = await usdcBalanceOf(order.deposit_address);
if (vbal >= BigInt(AMOUNT)) {
  ok(true, `vault on-chain balance ${vbal} >= ${AMOUNT}`);
} else {
  const { data: accts } = await ledgerAccounts();
  const cold = (accts.data ?? []).find(
    (a) => a.account_type === "CUSTODY_COLD" && a.asset_symbol === "USDC" && a.chain_id === CHAIN_ID,
  );
  ok(!!cold && BigInt(cold.balance) >= BigInt(AMOUNT),
    `vault swept to custody: CUSTODY_COLD=${cold?.balance} >= ${AMOUNT}`);
  console.log(`  vault already swept — CUSTODY_COLD(USDC)=${cold.balance}`);
}

// outbound queue evidence: EIP3009_RELAY intent + attempt tx hash
const { data: q2 } = await outboundQueue();
const intent = (q2.data ?? []).find((i) => i.id === accepted.intent_id);
const attempt = intent?.attempts?.[intent.attempts.length - 1];
console.log(`  intent status: ${intent?.status}  attempt tx: ${attempt?.tx_hash} (receipt=${attempt?.receipt_status})`);
ok(!!attempt?.tx_hash, "relay attempt broadcasted with tx_hash");

console.log("\n=== LINK B PASSED ===");
console.log(
  JSON.stringify(
    {
      order_id: orderId,
      intent_id: accepted.intent_id,
      relay_tx: attempt?.tx_hash,
      sse_events: names,
      status: p.status,
    },
    null,
    2,
  ),
);

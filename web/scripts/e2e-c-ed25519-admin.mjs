// I2 — Link C: offline admin Ed25519 paths (whitelist ADDR_V1 69B + refund REFUND_V1 127B).
//   whitelist: build 69B VAULTFLOW_ADDR_V1 -> sign -> POST /v1/withdrawal-addresses -> ACTIVE
//   refund   : overpay order -> SUSPENSE_OVERPAY_LIAB -> build+sign 127B VAULTFLOW_REFUND_V1
//              -> POST /v1/refunds -> LOCKED_QUEUED -> signer ticket-verify -> broadcast -> COMPLETED
// Byte layouts mirror web/src/lib/ed25519.ts via e2e-lib.mjs (signer verify = parity proof).
// Run:  node web/scripts/e2e-c-ed25519-admin.mjs

import { generatePrivateKey, privateKeyToAccount } from "viem/accounts";
import {
  CHAIN_ID,
  CUSTOMER,
  MERCHANT_ID,
  PAYER,
  USDC,
  adminSeed,
  api,
  autoMine,
  buildAddrV1,
  buildRefundV1,
  bytesToHex0x,
  createOrder,
  edSign,
  GatewayError,
  getPayout,
  importAdminSeed,
  ledgerAccounts,
  mineBlocks,
  mintUsdc,
  ok,
  outboundQueue,
  payerWallet,
  transferUsdc,
  uid,
  usdcBalanceOf,
  uuidToBytes16,
  waitOrderStatus,
  waitFor,
  waitTx,
} from "./e2e-lib.mjs";

const ORDER_AMOUNT = "1000000"; // 1.0 USDC expected
const EXTRA = "500000"; //         0.5 USDC overpay -> SUSPENSE_OVERPAY_LIAB -> refund amount
const PAYOUT_HOT = "0x90f79bf6eb2c4f870365e785982e1f101e93b906";

console.log("=== Link C — 离线管理员 Ed25519 (白名单 69B + 退款票据 127B) ===\n");

// --- admin key (deploy/admin-ed25519.json — read-only dev material) ----------
const seed = adminSeed();
const priv = await importAdminSeed(seed.private_key_hex);
console.log(`  admin pubkey: ${seed.public_key_hex}`);

// ===========================================================================
// C-1) 提现白名单 — 69B VAULTFLOW_ADDR_V1
// ===========================================================================
const wlAccount = privateKeyToAccount(generatePrivateKey()); // fresh per run — avoids signer 60s cooldown slot collision
const wlAddr = wlAccount.address.toLowerCase();
const validUntil = Math.floor(Date.now() / 1000) + 7 * 86400;

const wlPayload = buildAddrV1({ chainId: CHAIN_ID, merchantUuid: MERCHANT_ID, recipient: wlAddr, validUntil });
ok(wlPayload.length === 69, `ADDR_V1 payload length = 69B (magic VAULTFLOW_ADDR_V1)`);
ok(
  bytesToHex0x(wlPayload.slice(25, 41)) === bytesToHex0x(uuidToBytes16(MERCHANT_ID)),
  `payload[25:41] == merchant_uuid raw16`,
);
const wlSig = await edSign(priv, wlPayload);
ok(/^0x[0-9a-f]{128}$/.test(wlSig), `Ed25519 signature = 64B hex (${wlSig.slice(0, 26)}…)`);

// negative control: signature over address A submitted with address B ->
// gateway rebuilds payload with B -> signer MUST reject (proves real verify).
try {
  const other = privateKeyToAccount(generatePrivateKey()).address.toLowerCase();
  await api("POST", "/v1/withdrawal-addresses", {
    idem: `i2-${uid()}`,
    body: { chain_id: CHAIN_ID, address: other, valid_until_unix: validUntil, admin_signature: wlSig },
  });
  console.log("  [neg] unexpected: mismatched signature was accepted!");
} catch (e) {
  if (e instanceof GatewayError) {
    ok(e.status === 422, `[neg] tampered (address,signature) pair rejected: HTTP ${e.status} ${e.code}`);
  } else throw e;
}

const { status: wlStatus, data: wl } = await api("POST", "/v1/withdrawal-addresses", {
  idem: `i2-${uid()}`,
  body: { chain_id: CHAIN_ID, address: wlAddr, valid_until_unix: validUntil, admin_signature: wlSig },
});
ok(wlStatus === 201, `POST /v1/withdrawal-addresses -> 201`);
ok(wl.status === "ACTIVE", `whitelist status = ${wl.status} (signer accepted; gateway ACTIVE projection)`);
ok(wl.address === wlAddr, `whitelisted address = ${wl.address}`);
console.log(`  withdrawal_address_id = ${wl.id}  valid_until = ${wl.valid_until}`);

const { data: wlList } = await api("GET", "/v1/withdrawal-addresses");
ok(wlList.data.some((w) => w.id === wl.id && w.status === "ACTIVE"), "GET /v1/withdrawal-addresses lists ACTIVE record");

// ===========================================================================
// C-2) 超付订单 -> SUSPENSE_OVERPAY_LIAB (退款资金来源)
// ===========================================================================
const { data: order } = await createOrder({ amount: ORDER_AMOUNT, ref: `i2-c-${uid().slice(0, 8)}` });
console.log(`\n  order ${order.id}  expected=${order.expected_amount}  vault=${order.deposit_address}`);
if ((await usdcBalanceOf(PAYER)) < BigInt(ORDER_AMOUNT) + BigInt(EXTRA)) {
  const m = await mintUsdc(PAYER, 10_000_000n);
  await waitTx(m);
}
const payHash = await transferUsdc(payerWallet, order.deposit_address, BigInt(ORDER_AMOUNT) + BigInt(EXTRA));
await waitTx(payHash);
await mineBlocks(45);
const stopMining = autoMine(2500);
console.log(`  overpay transfer mined: ${payHash}`);

const settled = await waitOrderStatus(order.id, ["PAID", "OVERPAID"], 300_000);
ok(settled.status === "OVERPAID", `order status = ${settled.status} (received ${settled.received_amount})`);

const { data: accts } = await ledgerAccounts();
const ovr = accts.data.find(
  (a) => a.owner_merchant_id === MERCHANT_ID && a.account_type === "SUSPENSE_OVERPAY_LIAB" && a.asset_symbol === "USDC" && a.chain_id === CHAIN_ID,
);
ok(!!ovr && BigInt(ovr.balance) >= BigInt(EXTRA), `SUSPENSE_OVERPAY_LIAB balance = ${ovr?.balance} >= ${EXTRA}`);

// ===========================================================================
// C-3) 客户退款 — 127B VAULTFLOW_REFUND_V1
// ===========================================================================
// fund the payout hot wallet so the signed transfer actually settles on-chain
if ((await usdcBalanceOf(PAYOUT_HOT)) < BigInt(EXTRA)) {
  const m = await mintUsdc(PAYOUT_HOT, BigInt(EXTRA));
  await waitTx(m);
  console.log(`  minted ${EXTRA} USDC to payout hot wallet (tx ${m})`);
}
const custBefore = await usdcBalanceOf(CUSTOMER);

const payoutUuid = uid();
const expiry = Math.floor(Date.now() / 1000) + 3600;
const ticket = buildRefundV1({
  chainId: CHAIN_ID,
  token: USDC,
  to: CUSTOMER,
  amount: EXTRA,
  payoutUuid,
  retrySeq: 0,
  expiry,
});
ok(ticket.length === 127, `REFUND_V1 payload length = 127B (magic VAULTFLOW_REFUND_V1)`);
ok(
  bytesToHex0x(ticket.slice(99, 115)) === bytesToHex0x(uuidToBytes16(payoutUuid)),
  `payload[99:115] == payout_uuid raw16`,
);
const ticketSig = await edSign(priv, ticket);
ok(/^0x[0-9a-f]{128}$/.test(ticketSig), `refund Ed25519 signature = 64B (${ticketSig.slice(0, 26)}…)`);

// FIX-G BUG-1/3 closed: no nonce-cursor seeding and no manual TREASURY_TOPUP —
// the deploy seed books HOT_WALLET_NATIVE from real hot-wallet ETH balances and
// the gateway nonce path is legal, so the refund intent queues on a clean deploy.

const { status: rfStatus, data: refund } = await api("POST", "/v1/refunds", {
  idem: `i2-${uid()}`,
  body: {
    payout_uuid: payoutUuid,
    source_account: "SUSPENSE_OVERPAY_LIAB",
    chain_id: CHAIN_ID,
    asset_symbol: "USDC",
    amount: EXTRA,
    refund_ticket: {
      to_address: CUSTOMER,
      payout_uuid: payoutUuid,
      retry_seq: 0,
      expiry_unix: expiry,
      admin_signature: ticketSig,
    },
  },
});
ok(rfStatus === 201, `POST /v1/refunds -> 201`);
ok(refund.id === payoutUuid, `payout.id == ticket payout_uuid (${refund.id})`);
ok(refund.payout_kind === "CUSTOMER_REFUND", `payout_kind = ${refund.payout_kind}`);
ok(refund.status === "LOCKED_QUEUED", `status = ${refund.status}`);
console.log(`  refund payout ${refund.id} to=${refund.to_address} amount=${refund.amount}`);

// txmanager picks it up -> signer verifies ticket byte-for-byte -> broadcast.
const final = await waitFor(
  `refund payout terminal status`,
  async () => {
    const { data } = await getPayout(payoutUuid);
    // FAILED_RETRYABLE 非业务终态 — payout dispatcher 会重试驱动, 继续等.
    return ["COMPLETED", "CANCELLED_UNLOCKED"].includes(data.status) ? data : null;
  },
  { timeoutMs: 240_000, intervalMs: 2_000 },
).catch(async (e) => {
  await mineBlocks(45);
  const { data } = await getPayout(payoutUuid);
  if (!["COMPLETED"].includes(data.status)) throw e;
  return data;
});
stopMining();
await mineBlocks(1);

const { data: q } = await outboundQueue();
const intent = (q.data ?? []).find((i) => i.reference_id === payoutUuid || i.id === refund.outbound_intent_id);
const attempt = intent?.attempts?.[intent.attempts.length - 1];
console.log(`  intent ${intent?.id} status=${intent?.status} attempt tx=${attempt?.tx_hash} receipt=${attempt?.receipt_status}`);
ok(!!attempt?.tx_hash, "outbound attempt broadcasted — vault-signer verified the 127B ticket");
ok(final.status === "COMPLETED", `refund payout status = ${final.status} (tx_hash=${final.tx_hash ?? attempt?.tx_hash})`);

const custAfter = await usdcBalanceOf(CUSTOMER);
ok(custAfter - custBefore === BigInt(EXTRA), `customer on-chain USDC +${custAfter - custBefore} == ${EXTRA}`);

console.log("\n=== LINK C PASSED ===");
console.log(
  JSON.stringify(
    {
      whitelist: { id: wl.id, address: wlAddr, status: wl.status },
      overpay_order: order.id,
      overpay_tx: payHash,
      refund_payout: payoutUuid,
      refund_tx: final.tx_hash ?? attempt?.tx_hash,
      payout_status: final.status,
    },
    null,
    2,
  ),
);

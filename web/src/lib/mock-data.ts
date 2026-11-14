// W6-1 — seed data for the local mock engine. Shapes mirror docs/openapi.yaml
// exactly (decimal-string amounts, snake_case fields, frozen enums).
// Demo ids reuse the GOLDEN_VECTORS bytes16 merchant/order ids so the derived
// salt/auth_nonce/vault line up byte-for-byte with the frozen vectors.

import type {
  LedgerAccount,
  LedgerEntry,
  LedgerTransaction,
  Order,
  OutboundTxIntent,
  Payout,
  QuarantinedVault,
  SignerHealth,
  WithdrawalAddress,
  WalletNonceCursor,
} from "./types";
import { DEV_CONTRACTS } from "./chains";
import { deriveAuthNonce, deriveOrderVault, deriveSalt } from "./create2";

export const MOCK_MERCHANT_ID = "00112233-4455-6677-8899-aabbccddeeff";
export const MOCK_MERCHANT_NAME = "VaultFlow Demo Merch Co.";
/** Cashier demo order — bytes16 equals the GOLDEN_VECTORS order_id. */
export const DEMO_ORDER_ID = "ffeeddcc-bbaa-9988-7766-554433221100";
/** Live demo order that settles into PARTIALLY_PAID (60% deposit). */
export const DEMO_PARTIAL_ORDER_ID = "11112222-3333-4444-5555-666677778888";

export const ANVIL = 31337;
export const USDC: `0x${string}` = "0x4444444444444444444444444444444444444444";
export const USDT: `0x${string}` = "0x6666666666666666666666666666666666666666";

/** `"0x" + s` with the template-literal hex type preserved. */
const hx = (s: string): `0x${string}` => `0x${s}` as `0x${string}`;

const HOUR = 3600_000;
const now = () => new Date().toISOString();
const iso = (ms: number) => new Date(ms).toISOString();

function orderSaltAuth(orderId: string) {
  const salt = deriveSalt(ANVIL, MOCK_MERCHANT_ID, orderId);
  const authNonce = deriveAuthNonce(ANVIL, salt);
  return { salt, authNonce };
}

const USDC_HINT = (orderId: string) => ({
  supported: true,
  router_address: DEV_CONTRACTS.eip3009Router,
  auth_nonce: orderSaltAuth(orderId).authNonce,
  token_contract: USDC,
});

export function mockCreate2Salt(orderId: string) {
  return orderSaltAuth(orderId).salt;
}

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

export interface MockOrder extends Order {
  /** mock orders always materialize a created_at (the real API emits null). */
  created_at: string;
  /** mock-only progression driver; not part of the wire shape. */
  __live?: "to_paid" | "to_partial" | "idle";
  __liveStart?: number;
}

export function seedOrders(): MockOrder[] {
  const t = Date.now();
  const mk = (o: Partial<MockOrder> & Pick<MockOrder, "id" | "status">): MockOrder => ({
    merchant_id: MOCK_MERCHANT_ID,
    merchant_order_ref: `ORD-${o.id.slice(0, 8).toUpperCase()}`,
    chain_id: ANVIL,
    asset_symbol: "USDC",
    token_contract: USDC,
    expected_amount: "1500000",
    received_amount: "0",
    deposit_address_id: `d${o.id.slice(1)}`,
    // Each order gets its true CREATE2 vault for (chainId, merchant, orderId):
    // DEMO_ORDER_ID resolves to the golden vector vault 0x7fc0…1557.
    deposit_address: deriveOrderVault({
      chainId: ANVIL,
      merchantId: MOCK_MERCHANT_ID,
      orderId: o.id,
      factory: DEV_CONTRACTS.factory,
      implementation: DEV_CONTRACTS.implementation,
    }).vaultAddress.toLowerCase() as `0x${string}`,
    deposit_address_deployed: false,
    create2_salt: mockCreate2Salt(o.id),
    cashier_url: `/pay/${o.id}`,
    eip3009: USDC_HINT(o.id),
    expires_at: iso(t + 30 * 60_000),
    settled_at: null,
    created_at: iso(t - 5 * 60_000),
    ...o,
  });

  return [
    mk({ id: DEMO_ORDER_ID, status: "AWAITING_PAYMENT", __live: "idle", expected_amount: "1500000" }),
    mk({ id: DEMO_PARTIAL_ORDER_ID, status: "AWAITING_PAYMENT", __live: "idle", expected_amount: "2500000" }),
    mk({
      id: "22222222-1111-4111-8111-222222222222",
      status: "PAID",
      received_amount: "1500000",
      settled_at: iso(t - 2 * HOUR),
      created_at: iso(t - 3 * HOUR),
      deposit_address_deployed: true,
    }),
    mk({
      id: "33333333-1111-4111-8111-333333333333",
      status: "OVERPAID",
      received_amount: "1800000",
      settled_at: iso(t - 90 * 60_000),
      created_at: iso(t - 4 * HOUR),
      deposit_address_deployed: true,
    }),
    mk({
      id: "44444444-1111-4111-8111-444444444444",
      status: "EXPIRED",
      created_at: iso(t - 26 * HOUR),
      expires_at: iso(t - 25 * HOUR),
    }),
    mk({
      id: "55555555-1111-4111-8111-555555555555",
      status: "UNDERPAID_EXPIRED_REVIEW",
      received_amount: "900000",
      created_at: iso(t - 30 * HOUR),
      expires_at: iso(t - 29 * HOUR),
    }),
    mk({
      id: "66666666-1111-4111-8111-666666666666",
      status: "LATE_PAID_REVIEW",
      received_amount: "1500000",
      created_at: iso(t - 30 * HOUR),
      expires_at: iso(t - 29 * HOUR),
    }),
    mk({
      id: "77777777-1111-4111-8111-777777777777",
      status: "REFUND_IN_PROGRESS",
      received_amount: "1500000",
      created_at: iso(t - 40 * HOUR),
      expires_at: iso(t - 39 * HOUR),
    }),
    mk({
      id: "88888888-1111-4111-8111-888888888888",
      status: "SETTLED_REVERSED",
      received_amount: "0",
      settled_at: iso(t - 50 * 60_000),
      created_at: iso(t - 3 * HOUR),
      expires_at: iso(t + 60 * 60_000),
    }),
    mk({
      id: "99999999-1111-4111-8111-999999999999",
      status: "AWAITING_PAYMENT",
      asset_symbol: "USDT",
      token_contract: USDT,
      expected_amount: "88000000",
      eip3009: null,
      created_at: iso(t - 60_000),
    }),
  ];
}

// ---------------------------------------------------------------------------
// Ledger — NUMERIC(78,0) strings; USDC pair trial-balances at 7_300_000_000.
// ---------------------------------------------------------------------------

let acctSeq = 0;
const acct = (
  account_type: LedgerAccount["account_type"],
  balance: string,
  asset_symbol: string,
  owner: string,
): LedgerAccount => ({
  id: `a0000000-0000-4000-8000-${String(++acctSeq).padStart(12, "0")}`,
  owner_merchant_id: owner,
  chain_id: ANVIL,
  asset_symbol,
  account_type,
  normal_side: (
    [
      "CUSTODY_UNSWEPT",
      "CUSTODY_COLD",
      "CUSTODY_HOT_PAYOUT",
      "HOT_WALLET_NATIVE",
      "GAS_EXPENSE",
      "MERCHANT_REORG_RECEIVABLE",
    ] as string[]
  ).includes(account_type)
    ? "DEBIT"
    : "CREDIT",
  balance,
});

const SENT = "00000000-0000-0000-0000-000000000000";

export function seedLedgerAccounts(): LedgerAccount[] {
  acctSeq = 0;
  return [
    // --- DEBIT normal-side (6), USDC ---
    acct("CUSTODY_UNSWEPT", "1500000000", "USDC", SENT),
    acct("CUSTODY_COLD", "5000000000", "USDC", SENT),
    acct("CUSTODY_HOT_PAYOUT", "800000000", "USDC", SENT),
    acct("MERCHANT_REORG_RECEIVABLE", "0", "USDC", MOCK_MERCHANT_ID),
    // --- DEBIT normal-side, native ETH ---
    acct("HOT_WALLET_NATIVE", "512000000000000000", "ETH", SENT),
    acct("GAS_EXPENSE", "12000000000000000", "ETH", SENT),
    // --- CREDIT normal-side (8), USDC ---
    acct("ORDER_ESCROW_LIAB", "300000000", "USDC", SENT),
    acct("MERCHANT_AVAILABLE", "6400000000", "USDC", MOCK_MERCHANT_ID),
    acct("PAYOUT_LOCKED_LIAB", "200000000", "USDC", SENT),
    acct("SUSPENSE_OVERPAY_LIAB", "5000000", "USDC", SENT),
    acct("SUSPENSE_REFUNDABLE_LIAB", "12000000", "USDC", SENT),
    acct("SUSPENSE_UNMATCHED_LIAB", "3000000", "USDC", SENT),
    acct("TREASURY_EQUITY_CAPITAL", "350000000", "USDC", SENT),
    acct("FEE_REVENUE", "30000000", "USDC", SENT),
    // --- CREDIT normal-side, native ETH (tops up both hot wallets) ---
    acct("TREASURY_EQUITY_CAPITAL", "524000000000000000", "ETH", SENT),
  ];
}

export function seedLedgerTransactions(): LedgerTransaction[] {
  const t = Date.now();
  const rows: Array<[string, LedgerTransaction["reference_type"], string, number]> = [
    ["t1000000-0000-4000-8000-000000000001", "TREASURY_TOPUP", "hot-wallet-gas-init", -26 * HOUR],
    ["t1000000-0000-4000-8000-000000000002", "DEPOSIT_FINALIZED", "22222222-1111-4111-8111-222222222222", -3 * HOUR],
    ["t1000000-0000-4000-8000-000000000003", "ORDER_SETTLE", "22222222-1111-4111-8111-222222222222", -3 * HOUR],
    ["t1000000-0000-4000-8000-000000000004", "SWEEP_SETTLE", "b0000000-0000-4000-8000-0000000000aa", -2 * HOUR],
    ["t1000000-0000-4000-8000-000000000005", "PAYOUT_FLOW", "c0000000-0000-4000-8000-0000000000bb", -1 * HOUR],
    ["t1000000-0000-4000-8000-000000000006", "ORDER_SUSPENSE", "55555555-1111-4111-8111-555555555555", -29 * HOUR],
    ["t1000000-0000-4000-8000-000000000007", "GAS_SETTLE", "intent-gas-0007", -50 * 60_000],
    ["t1000000-0000-4000-8000-000000000008", "REORG_REVERSAL", "reorg-audit-0008", -20 * 60_000],
  ];
  return rows.map(([id, reference_type, reference_id, dt], i) => ({
    id,
    reference_type,
    reference_id,
    description: `mock journal #${i + 1}`,
    created_at: iso(t + dt),
  }));
}

export function seedLedgerEntries(accounts: LedgerAccount[], txs: LedgerTransaction[]): LedgerEntry[] {
  const find = (type: string, asset = "USDC") =>
    accounts.find((a) => a.account_type === type && a.asset_symbol === asset)!;
  const t = Date.now();
  let n = 0;
  const e = (
    txId: string,
    account: LedgerAccount,
    direction: "DEBIT" | "CREDIT",
    amount: string,
    balance_after: string,
    dtMin = -60,
  ): LedgerEntry => ({
    id: `e0000000-0000-4000-8000-${String(++n).padStart(12, "0")}`,
    transaction_id: txId,
    account_id: account.id,
    account_type: account.account_type,
    owner_merchant_id: account.owner_merchant_id,
    direction,
    amount,
    balance_after,
    created_at: iso(t + dtMin * 60_000),
  });
  return [
    // DEPOSIT_FINALIZED: DR CUSTODY_UNSWEPT / CR ORDER_ESCROW_LIAB
    e(txs[1].id, find("CUSTODY_UNSWEPT"), "DEBIT", "1500000", "1500000000", -170),
    e(txs[1].id, find("ORDER_ESCROW_LIAB"), "CREDIT", "1500000", "300000000", -170),
    // ORDER_SETTLE: DR ORDER_ESCROW_LIAB / CR MERCHANT_AVAILABLE + FEE_REVENUE
    e(txs[2].id, find("ORDER_ESCROW_LIAB"), "DEBIT", "1500000", "298500000", -160),
    e(txs[2].id, find("MERCHANT_AVAILABLE"), "CREDIT", "1485000", "6400000000", -160),
    e(txs[2].id, find("FEE_REVENUE"), "CREDIT", "15000", "30000000", -160),
    // REORG_REVERSAL (deep): DR MERCHANT_AVAILABLE / CR CUSTODY_UNSWEPT (+receivable)
    e(txs[7].id, find("MERCHANT_AVAILABLE"), "DEBIT", "200000000", "6400000000", -18),
    e(txs[7].id, find("MERCHANT_REORG_RECEIVABLE"), "DEBIT", "0", "0", -18),
    e(txs[7].id, find("CUSTODY_UNSWEPT"), "CREDIT", "200000000", "1500000000", -18),
  ];
}

// ---------------------------------------------------------------------------
// Payouts / whitelist
// ---------------------------------------------------------------------------

export function seedPayouts(): Payout[] {
  const t = Date.now();
  return [
    {
      id: "c0000000-0000-4000-8000-0000000000bb",
      merchant_id: MOCK_MERCHANT_ID,
      payout_kind: "WITHDRAWAL",
      source_account_type: "MERCHANT_AVAILABLE",
      source_order_id: null,
      chain_id: ANVIL,
      asset_symbol: "USDC",
      to_address: "0x5555555555555555555555555555555555555555",
      amount: "200000000",
      retry_seq: 0,
      status: "IN_FLIGHT",
      outbound_intent_id: "f0000000-0000-4000-8000-000000000102",
      tx_hash: hx("ab".repeat(32)),
      created_at: iso(t - 60 * 60_000),
      updated_at: iso(t - 55 * 60_000),
    },
    {
      id: "c0000000-0000-4000-8000-0000000000cc",
      merchant_id: MOCK_MERCHANT_ID,
      payout_kind: "CUSTOMER_REFUND",
      source_account_type: "SUSPENSE_REFUNDABLE_LIAB",
      source_order_id: "66666666-1111-4111-8111-666666666666",
      chain_id: ANVIL,
      asset_symbol: "USDC",
      to_address: "0x5555555555555555555555555555555555555555",
      amount: "1500000",
      retry_seq: 0,
      status: "FAILED_RETRYABLE",
      outbound_intent_id: "f0000000-0000-4000-8000-000000000105",
      tx_hash: null,
      created_at: iso(t - 5 * HOUR),
      updated_at: iso(t - 40 * 60_000),
    },
    {
      id: "c0000000-0000-4000-8000-0000000000dd",
      merchant_id: MOCK_MERCHANT_ID,
      payout_kind: "WITHDRAWAL",
      source_account_type: "MERCHANT_AVAILABLE",
      source_order_id: null,
      chain_id: ANVIL,
      asset_symbol: "USDC",
      to_address: "0x5555555555555555555555555555555555555555",
      amount: "50000000",
      retry_seq: 0,
      status: "LOCKED_QUEUED",
      outbound_intent_id: null,
      tx_hash: null,
      created_at: iso(t - 10 * 60_000),
      updated_at: iso(t - 10 * 60_000),
    },
    {
      id: "c0000000-0000-4000-8000-0000000000ee",
      merchant_id: MOCK_MERCHANT_ID,
      payout_kind: "CUSTOMER_REFUND",
      source_account_type: "SUSPENSE_OVERPAY_LIAB",
      source_order_id: "33333333-1111-4111-8111-333333333333",
      chain_id: ANVIL,
      asset_symbol: "USDC",
      to_address: "0x5555555555555555555555555555555555555555",
      amount: "300000",
      retry_seq: 1,
      status: "COMPLETED",
      outbound_intent_id: "f0000000-0000-4000-8000-000000000106",
      tx_hash: hx("cd".repeat(32)),
      created_at: iso(t - 6 * HOUR),
      updated_at: iso(t - 5 * HOUR),
    },
  ];
}

export function seedWhitelist(): WithdrawalAddress[] {
  const t = Date.now();
  return [
    {
      id: "w0000000-0000-4000-8000-0000000000a1",
      merchant_id: MOCK_MERCHANT_ID,
      chain_id: ANVIL,
      address: "0x5555555555555555555555555555555555555555",
      status: "ACTIVE",
      valid_until: iso(t + 20 * 24 * HOUR),
      first_seen_at: iso(t - 26 * HOUR),
      signer_revoked: false,
      created_at: iso(t - 26 * HOUR),
    },
    {
      id: "w0000000-0000-4000-8000-0000000000a2",
      merchant_id: MOCK_MERCHANT_ID,
      chain_id: ANVIL,
      address: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      status: "PENDING_COOLDOWN",
      valid_until: iso(t + 29 * 24 * HOUR),
      first_seen_at: iso(t - 1 * HOUR),
      signer_revoked: false,
      created_at: iso(t - 1 * HOUR),
    },
    {
      id: "w0000000-0000-4000-8000-0000000000a3",
      merchant_id: MOCK_MERCHANT_ID,
      chain_id: ANVIL,
      address: "0xcccccccccccccccccccccccccccccccccccccccc",
      status: "EXPIRED",
      valid_until: iso(t - 2 * HOUR),
      first_seen_at: iso(t - 30 * 24 * HOUR),
      signer_revoked: false,
      created_at: iso(t - 30 * 24 * HOUR),
    },
  ];
}

// ---------------------------------------------------------------------------
// Outbound queue / signer health / quarantine / reconciliation
// ---------------------------------------------------------------------------

export const MOCK_INTENT_STOLEN = "f0000000-0000-4000-8000-000000000101";
export const MOCK_INTENT_INFLIGHT = "f0000000-0000-4000-8000-000000000102";
export const MOCK_INTENT_SWEEP = "f0000000-0000-4000-8000-000000000103";
export const MOCK_INTENT_3009 = "f0000000-0000-4000-8000-000000000104";
export const MOCK_INTENT_REVERTED = "f0000000-0000-4000-8000-000000000105";

export function seedNonceCursors(): WalletNonceCursor[] {
  const t = Date.now();
  return [
    {
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.relayerHot,
      wallet_role: "RELAYER_HOT",
      next_nonce: 8,
      halted: false,
      updated_at: iso(t - 3 * 60_000),
    },
    {
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.payoutHot,
      wallet_role: "PAYOUT_HOT",
      next_nonce: 14,
      halted: true,
      updated_at: iso(t - 2 * 60_000),
    },
  ];
}

export function seedOutboundIntents(): OutboundTxIntent[] {
  const t = Date.now();
  const att = (
    id: string,
    attempt_no: number,
    kind: "PRIMARY_INTENT" | "GAP_FILLER",
    tx: `0x${string}`,
    status: OutboundTxIntent["attempts"][number]["status"],
    extra: Partial<OutboundTxIntent["attempts"][number]> = {},
  ): OutboundTxIntent["attempts"][number] => ({
    id,
    attempt_no,
    attempt_kind: kind,
    tx_hash: tx,
    status,
    block_number: null,
    block_hash: null,
    receipt_status: null,
    max_fee_per_gas: "2000000000",
    max_priority_fee_per_gas: "1000000000",
    gas_used: null,
    effective_gas_price: null,
    broadcasted_at: iso(t - 30 * 60_000),
    ...extra,
  });
  return [
    {
      id: MOCK_INTENT_STOLEN,
      intent_type: "MERCHANT_PAYOUT",
      reference_id: "c0000000-0000-4000-8000-0000000000ff",
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.payoutHot,
      nonce: 13,
      status: "NONCE_STOLEN_HALT",
      created_at: iso(t - 25 * 60_000),
      attempts: [
        att("aa000000-0000-4000-8000-000000000201", 1, "PRIMARY_INTENT", hx("51".repeat(32)), "BROADCASTED"),
      ],
    },
    {
      id: MOCK_INTENT_INFLIGHT,
      intent_type: "MERCHANT_PAYOUT",
      reference_id: "c0000000-0000-4000-8000-0000000000bb",
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.payoutHot,
      nonce: 12,
      status: "IN_FLIGHT",
      created_at: iso(t - 55 * 60_000),
      attempts: [
        att("aa000000-0000-4000-8000-000000000202", 1, "PRIMARY_INTENT", hx("ab".repeat(32)), "SUPERSEDED", {
          max_fee_per_gas: "2000000000",
        }),
        att("aa000000-0000-4000-8000-000000000203", 2, "PRIMARY_INTENT", hx("ac".repeat(32)), "BROADCASTED", {
          max_fee_per_gas: "3000000000",
          max_priority_fee_per_gas: "1500000000",
          broadcasted_at: iso(t - 20 * 60_000),
        }),
      ],
    },
    {
      id: MOCK_INTENT_SWEEP,
      intent_type: "SWEEP_BATCH",
      reference_id: "b0000000-0000-4000-8000-0000000000aa",
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.relayerHot,
      nonce: 7,
      status: "INCLUDED_PENDING_SAFE",
      created_at: iso(t - 15 * 60_000),
      attempts: [
        att("aa000000-0000-4000-8000-000000000204", 1, "PRIMARY_INTENT", hx("5e".repeat(32)), "INCLUDED_UNSAFE", {
          block_number: 1088,
          block_hash: hx("bb".repeat(32)),
        }),
      ],
    },
    {
      id: MOCK_INTENT_3009,
      intent_type: "EIP3009_RELAY",
      reference_id: DEMO_ORDER_ID,
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.relayerHot,
      nonce: 6,
      status: "CONFIRMED",
      created_at: iso(t - 40 * 60_000),
      attempts: [
        att("aa000000-0000-4000-8000-000000000205", 1, "PRIMARY_INTENT", hx("30".repeat(32)), "FINALIZED", {
          block_number: 1070,
          block_hash: hx("cc".repeat(32)),
          receipt_status: 1,
          gas_used: 168420,
          effective_gas_price: "1100000000",
        }),
      ],
    },
    {
      id: MOCK_INTENT_REVERTED,
      intent_type: "MERCHANT_PAYOUT",
      reference_id: "c0000000-0000-4000-8000-0000000000cc",
      chain_id: ANVIL,
      wallet_address: DEV_CONTRACTS.payoutHot,
      nonce: 11,
      status: "FAILED_REVERTED",
      created_at: iso(t - 45 * 60_000),
      attempts: [
        att("aa000000-0000-4000-8000-000000000206", 1, "PRIMARY_INTENT", hx("fa".repeat(32)), "FINALIZED", {
          block_number: 1066,
          block_hash: hx("dd".repeat(32)),
          receipt_status: 0,
          gas_used: 46231,
          effective_gas_price: "1200000000",
        }),
      ],
    },
  ];
}

export function seedSignerHealth(): SignerHealth {
  return {
    reachable: true,
    mtls_ok: true,
    signer_version: "vault-signer 0.1.0 (mock)",
    uptime_seconds: 14520,
    wallets: [
      { role: "RELAYER_HOT", address: DEV_CONTRACTS.relayerHot, chain_ids: [ANVIL], loaded: true },
      { role: "PAYOUT_HOT", address: DEV_CONTRACTS.payoutHot, chain_ids: [ANVIL], loaded: true },
    ],
    circuit_breaker: {
      tripped: false,
      rolling_24h_used: "205000000",
      rolling_24h_limit: "1000000000",
    },
    whitelist: { active_count: 4, pending_cooldown_count: 1, revoked_count: 1 },
    intent_slots: { open_slots: 2, filler_signed_slots: 1 },
    audit_chain: {
      valid: true,
      last_seq: 128,
      last_entry_hash: hx("9e".repeat(32)),
      verified_at: now(),
      verification_interval_seconds: 300,
    },
  };
}

export function seedQuarantinedVaults(): QuarantinedVault[] {
  const t = Date.now();
  return [
    {
      deposit_address_id: "d0000000-0000-4000-8000-00000000dead",
      chain_id: ANVIL,
      address: "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      is_quarantined: true,
      asset_symbol: "USDT",
      reason: "SWEEP_REVERT_BLACKLISTED",
      sweep_batch_id: "b0000000-0000-4000-8000-0000000000aa",
      sweep_batch_item_id: "b1000000-0000-4000-8000-0000000000a1",
      item_status: "QUARANTINED_REVERT",
      related_order_id: "99999999-1111-4111-8111-999999999999",
      quarantined_at: iso(t - 15 * 60_000),
    },
  ];
}

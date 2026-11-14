// W6-1 — TypeScript projection of docs/openapi.yaml (P0-5 frozen contract).
// Field names keep the wire's snake_case exactly; every amount field is a
// decimal string parsed via src/lib/amount.ts — never a JSON number.

export type Hex = `0x${string}`;
export type EvmAddress = Hex;
export type Bytes32 = Hex;
export type TxHash = Hex;
export type Uuid = string;

// ---------------------------------------------------------------------------
// Enums — EXACTLY ARCHITECTURE §5 / migrations CHECK constraints
// ---------------------------------------------------------------------------

export type OnchainDepositStatus =
  | "DETECTED"
  | "ORPHANED_SHALLOW"
  | "FINALIZED"
  | "ORPHANED_DEEP";

export type PaymentOrderStatus =
  | "AWAITING_PAYMENT"
  | "PARTIALLY_PAID"
  | "PAID"
  | "OVERPAID"
  | "EXPIRED"
  | "UNDERPAID_EXPIRED_REVIEW"
  | "LATE_PAID_REVIEW"
  | "REFUND_IN_PROGRESS"
  | "REFUNDED"
  | "SETTLED_REVERSED"
  | "REVERSAL_RESOLVED";

export type OutboundTxIntentStatus =
  | "QUEUED"
  | "IN_FLIGHT"
  | "INCLUDED_PENDING_SAFE"
  | "CONFIRMED"
  | "FAILED_REVERTED"
  | "CANCELLED_BY_FILLER"
  | "NONCE_STOLEN_HALT";

export type MerchantPayoutStatus =
  | "LOCKED_QUEUED"
  | "IN_FLIGHT"
  | "COMPLETED"
  | "FAILED_RETRYABLE"
  | "CANCELLED_UNLOCKED";

export type OutboundTxAttemptStatus =
  | "BROADCASTED"
  | "INCLUDED_UNSAFE"
  | "FINALIZED"
  | "SUPERSEDED"
  | "DROPPED";

export type OutboundIntentType =
  | "SWEEP_BATCH"
  | "MERCHANT_PAYOUT"
  | "EIP3009_RELAY"
  | "NONCE_GAP_FILLER";

export type OutboundAttemptKind = "PRIMARY_INTENT" | "GAP_FILLER";

export type OnchainDepositCategory =
  | "ORDER_MATCH"
  | "WRONG_ASSET"
  | "POST_TERMINAL"
  | "TREASURY_REBALANCE"
  | "TOPUP";

export type WithdrawalAddressStatus =
  | "PENDING_COOLDOWN"
  | "ACTIVE"
  | "EXPIRED"
  | "REVOKED";

export type PayoutKind = "WITHDRAWAL" | "CUSTOMER_REFUND";

export type PayoutSourceAccount =
  | "MERCHANT_AVAILABLE"
  | "SUSPENSE_OVERPAY_LIAB"
  | "SUSPENSE_REFUNDABLE_LIAB"
  | "SUSPENSE_UNMATCHED_LIAB";

export type RefundSourceAccount = Exclude<PayoutSourceAccount, "MERCHANT_AVAILABLE">;

export type LedgerAccountType =
  | "CUSTODY_UNSWEPT"
  | "CUSTODY_COLD"
  | "CUSTODY_HOT_PAYOUT"
  | "HOT_WALLET_NATIVE"
  | "GAS_EXPENSE"
  | "MERCHANT_REORG_RECEIVABLE"
  | "ORDER_ESCROW_LIAB"
  | "MERCHANT_AVAILABLE"
  | "PAYOUT_LOCKED_LIAB"
  | "SUSPENSE_OVERPAY_LIAB"
  | "SUSPENSE_REFUNDABLE_LIAB"
  | "SUSPENSE_UNMATCHED_LIAB"
  | "TREASURY_EQUITY_CAPITAL"
  | "FEE_REVENUE";

export type LedgerDirection = "DEBIT" | "CREDIT";

export type LedgerReferenceType =
  | "TREASURY_TOPUP"
  | "INTERNAL_TREASURY_REBALANCE"
  | "DEPOSIT_FINALIZED"
  | "ORDER_SETTLE"
  | "ORDER_SUSPENSE"
  | "SWEEP_SETTLE"
  | "GAS_SETTLE"
  | "PAYOUT_FLOW"
  | "REORG_REVERSAL";

export type ReviewAction =
  | "ACCEPT_UNDERPAID_DISCOUNT"
  | "ACCEPT_LATE_PAYMENT"
  | "APPROVE_REFUND";

export type WalletRole = "RELAYER_HOT" | "PAYOUT_HOT";

export type CustodyCheckType =
  | "UNSWEPT_DEPOSIT_VAULTS"
  | "COLD_WALLET_BALANCE"
  | "PAYOUT_HOT_WALLET_BALANCE"
  | "HOT_WALLETS_NATIVE_BALANCE";

export type ReconciliationRunStatus = "NEVER_RUN" | "RUNNING" | "PASSED" | "FAILED";
export type ReconciliationTrigger = "MANUAL" | "SCHEDULED";
export type SimAnvilAction = "snapshot" | "mine" | "revert" | "drop-transaction";
export type SimScenario =
  | "shallow-reorg"
  | "deep-reorg"
  | "blacklist-returnbomb"
  | "signer-penetration";
export type SimRunStatus = "RUNNING" | "COMPLETED" | "FAILED";
export type SimStepStatus = "PENDING" | "RUNNING" | "OK" | "FAILED" | "SKIPPED";

export type SseEventType =
  | "snapshot"
  | "block_detected"
  | "confirmation_progress"
  | "paid"
  | "overpaid"
  | "partially_paid"
  | "expired"
  | "underpaid_expired_review"
  | "late_paid_review"
  | "refund_in_progress"
  | "refunded"
  | "settled_reversed"
  | "reversal_resolved";

export type ErrorCode =
  | "VALIDATION_FAILED"
  | "IDEMPOTENCY_KEY_REQUIRED"
  | "IDEMPOTENCY_CONFLICT"
  | "UNAUTHORIZED"
  | "FORBIDDEN"
  | "NOT_FOUND"
  | "RATE_LIMITED"
  | "ORDER_STATE_INVALID"
  | "PAYOUT_STATE_INVALID"
  | "INSUFFICIENT_BALANCE"
  | "PAYOUT_FROZEN"
  | "REORG_RECEIVABLE_OUTSTANDING"
  | "ADDRESS_NOT_WHITELISTED"
  | "SIGNER_REJECTED"
  | "REFUND_TICKET_INVALID"
  | "ALREADY_TERMINAL"
  | "RECONCILIATION_RUNNING"
  | "FEATURE_DISABLED"
  | "UPSTREAM_UNAVAILABLE"
  | "INTERNAL";

export interface ApiErrorBody {
  error: { code: ErrorCode; message: string; details?: Record<string, unknown>; request_id?: string };
}

// ---------------------------------------------------------------------------
// Ledger chart (ARCHITECTURE §6.1 — closed 14-account set)
// ---------------------------------------------------------------------------

export const LEDGER_DEBIT_ACCOUNTS: readonly LedgerAccountType[] = [
  "CUSTODY_UNSWEPT",
  "CUSTODY_COLD",
  "CUSTODY_HOT_PAYOUT",
  "HOT_WALLET_NATIVE",
  "GAS_EXPENSE",
  "MERCHANT_REORG_RECEIVABLE",
];

export const LEDGER_CREDIT_ACCOUNTS: readonly LedgerAccountType[] = [
  "ORDER_ESCROW_LIAB",
  "MERCHANT_AVAILABLE",
  "PAYOUT_LOCKED_LIAB",
  "SUSPENSE_OVERPAY_LIAB",
  "SUSPENSE_REFUNDABLE_LIAB",
  "SUSPENSE_UNMATCHED_LIAB",
  "TREASURY_EQUITY_CAPITAL",
  "FEE_REVENUE",
];

export const ACCOUNT_NORMAL_SIDE: Record<LedgerAccountType, LedgerDirection> = {
  CUSTODY_UNSWEPT: "DEBIT",
  CUSTODY_COLD: "DEBIT",
  CUSTODY_HOT_PAYOUT: "DEBIT",
  HOT_WALLET_NATIVE: "DEBIT",
  GAS_EXPENSE: "DEBIT",
  MERCHANT_REORG_RECEIVABLE: "DEBIT",
  ORDER_ESCROW_LIAB: "CREDIT",
  MERCHANT_AVAILABLE: "CREDIT",
  PAYOUT_LOCKED_LIAB: "CREDIT",
  SUSPENSE_OVERPAY_LIAB: "CREDIT",
  SUSPENSE_REFUNDABLE_LIAB: "CREDIT",
  SUSPENSE_UNMATCHED_LIAB: "CREDIT",
  TREASURY_EQUITY_CAPITAL: "CREDIT",
  FEE_REVENUE: "CREDIT",
};

/** Sentinel owner for system-level ledger accounts (AGENTS.md §2.5.2). */
export const ZERO_SENTINEL_UUID = "00000000-0000-0000-0000-000000000000";

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

export interface PageInfo {
  next_cursor: string | null;
  has_more: boolean;
}

export interface Eip3009Hint {
  supported: boolean;
  router_address: EvmAddress;
  auth_nonce: Bytes32;
  token_contract?: EvmAddress;
  /**
   * RFC3339 upper bound the payer must echo back as `valid_before` unix
   * seconds (== order.expires_at; server-enforced equality, FIX-D5).
   */
  valid_before?: string;
}

/** POST /v1/orders request (merchant). `amount` is a decimal string. */
export interface CreateOrderRequest {
  merchant_order_ref?: string | null;
  chain_id: number;
  asset_symbol: string;
  amount: string;
  /** TTL seconds, [60, 86400]; server defaults 1800. */
  expires_in_seconds?: number;
}

export interface Order {
  id: Uuid;
  merchant_id: Uuid;
  merchant_order_ref: string | null;
  chain_id: number;
  asset_symbol: string;
  token_contract?: EvmAddress;
  expected_amount: string;
  received_amount: string;
  status: PaymentOrderStatus;
  deposit_address_id?: Uuid;
  deposit_address: EvmAddress;
  deposit_address_deployed?: boolean;
  create2_salt?: Bytes32;
  cashier_url: string;
  eip3009?: Eip3009Hint | null;
  expires_at: string;
  settled_at?: string | null;
  /** payment_orders has no created_at column — the API emits null (reported). */
  created_at?: string | null;
}

export interface OrderPage {
  data: Order[];
  page: PageInfo;
}

export interface PublicPayment {
  order_id: Uuid;
  status: PaymentOrderStatus;
  merchant_name?: string;
  merchant_order_ref?: string | null;
  chain_id: number;
  asset_symbol: string;
  token_contract: EvmAddress;
  decimals: number;
  expected_amount: string;
  received_amount: string;
  deposit_address: EvmAddress;
  deposit_address_deployed?: boolean;
  expires_at: string;
  seconds_remaining?: number;
  settled_at?: string | null;
  safe_head_height?: number;
  required_confirmations?: number;
  eip3009?: Eip3009Hint | null;
}

export interface Eip3009Submission {
  from: EvmAddress;
  to: EvmAddress;
  value: string;
  valid_after: number;
  valid_before: number;
  nonce: Bytes32;
  v: number;
  r: Bytes32;
  s: Bytes32;
}

export interface Eip3009Accepted {
  order_id: Uuid;
  intent_id: Uuid;
  intent_status: OutboundTxIntentStatus;
}

export interface SseDepositData {
  deposit_id: Uuid;
  chain_id?: number;
  tx_hash: TxHash;
  log_index?: number;
  block_number: number;
  from_address?: EvmAddress;
  amount: string;
  asset_symbol: string;
  deposit_category?: OnchainDepositCategory;
  status: OnchainDepositStatus;
  detected_at?: string;
}

export interface SseConfirmationData {
  deposit_id: Uuid;
  tx_hash: TxHash;
  block_number: number;
  current_confirmations: number;
  required_confirmations: number;
  safe_head_height: number;
}

export interface SseOrderStatusData {
  order_id: Uuid;
  status: PaymentOrderStatus;
  expected_amount: string;
  received_amount: string;
  tx_hash?: TxHash | null;
  expires_at?: string;
  settled_at?: string | null;
  occurred_at?: string;
}

// --- payouts / refunds ------------------------------------------------------

export interface RefundTicketFields {
  to_address: EvmAddress;
  /**
   * Ticket-bound payout anchor — the `payout_uuid(16B)` embedded in the signed
   * 127-byte payload. **Required inside `refund` for `APPROVE_REFUND`** and
   * inside `refund_ticket` whenever the caller wants the binding explicit: it
   * becomes `merchant_payouts.id` (`intent.reference_id` /
   * `SignTransactionRequest.payout_uuid`), byte-compared by the signer. Inside
   * `refund_ticket` it may be omitted and then defaults to the request's
   * top-level `payout_uuid`; if both are given they MUST be equal.
   * (docs/openapi.yaml `RefundTicketFields.payout_uuid`)
   */
  payout_uuid?: Uuid;
  retry_seq: number;
  expiry_unix: number;
  admin_signature: string;
}

export interface ReviewOrderRequest {
  action: ReviewAction;
  note?: string;
  refund?: RefundTicketFields;
}

export interface ReviewOrderResponse {
  order: Order;
  payout?: Payout;
}

export interface Payout {
  id: Uuid;
  merchant_id: Uuid;
  payout_kind: PayoutKind;
  source_account_type: PayoutSourceAccount;
  source_order_id?: string | null;
  chain_id: number;
  asset_symbol: string;
  to_address: EvmAddress;
  amount: string;
  retry_seq: number;
  status: MerchantPayoutStatus;
  outbound_intent_id?: Uuid | null;
  tx_hash?: TxHash | null;
  created_at: string;
  updated_at?: string;
}

export interface PayoutPage {
  data: Payout[];
  page: PageInfo;
}

export interface CreatePayoutRequest {
  payout_uuid: Uuid;
  source_account: PayoutSourceAccount;
  source_order_id?: string | null;
  chain_id: number;
  asset_symbol: string;
  amount: string;
  to_address: EvmAddress;
}

export interface CreateRefundRequest {
  payout_uuid: Uuid;
  source_account: RefundSourceAccount;
  source_order_id?: string | null;
  chain_id: number;
  asset_symbol: string;
  amount: string;
  refund_ticket: RefundTicketFields;
}

// --- withdrawal whitelist ---------------------------------------------------

export interface RegisterWithdrawalAddressRequest {
  chain_id: number;
  address: EvmAddress;
  valid_until_unix: number;
  admin_signature: string;
}

export interface WithdrawalAddress {
  id: Uuid;
  merchant_id: Uuid;
  chain_id: number;
  address: EvmAddress;
  status: WithdrawalAddressStatus;
  valid_until: string;
  first_seen_at?: string | null;
  signer_revoked?: boolean;
  created_at?: string;
}

export interface WithdrawalAddressPage {
  data: WithdrawalAddress[];
  page: PageInfo;
}

// --- ledger introspection ---------------------------------------------------

export interface LedgerAccount {
  id: Uuid;
  owner_merchant_id: Uuid;
  chain_id: number;
  asset_symbol: string;
  account_type: LedgerAccountType;
  normal_side: LedgerDirection;
  balance: string;
}

export interface LedgerAccountsResponse {
  data: LedgerAccount[];
}

export interface LedgerTransaction {
  id: Uuid;
  reference_type: LedgerReferenceType;
  reference_id: string;
  description?: string | null;
  created_at: string;
}

export interface LedgerTransactionPage {
  data: LedgerTransaction[];
  page: PageInfo;
}

export interface LedgerEntry {
  id: Uuid;
  transaction_id: Uuid;
  account_id: Uuid;
  account_type?: LedgerAccountType;
  owner_merchant_id?: Uuid;
  direction: LedgerDirection;
  amount: string;
  balance_after: string;
  created_at: string;
}

export interface LedgerEntryPage {
  data: LedgerEntry[];
  page: PageInfo;
}

// --- admin / ops ------------------------------------------------------------

export interface TrialBalanceRow {
  chain_id: number;
  asset_symbol: string;
  total_debit: string;
  total_credit: string;
  balanced: boolean;
  all_balances_non_negative: boolean;
}

export interface CustodyCheck {
  check: CustodyCheckType;
  chain_id: number;
  asset_symbol: string;
  ledger_account: LedgerAccountType;
  ledger_balance: string;
  onchain_balance: string;
  delta: string;
  matched: boolean;
  safe_head_height: number;
}

export interface ReconciliationReport {
  run_id?: Uuid | null;
  trigger?: ReconciliationTrigger;
  status: ReconciliationRunStatus;
  started_at?: string | null;
  finished_at?: string | null;
  trial_balances?: TrialBalanceRow[];
  custody_checks?: CustodyCheck[];
  failures?: string[];
}

export interface ReconciliationRunAccepted {
  run_id: Uuid;
  status: ReconciliationRunStatus;
}

export interface OutboundTxAttempt {
  id: Uuid;
  attempt_no: number;
  attempt_kind: OutboundAttemptKind;
  tx_hash: TxHash;
  status: OutboundTxAttemptStatus;
  block_number?: number | null;
  block_hash?: string | null;
  receipt_status?: 0 | 1 | null;
  max_fee_per_gas?: string;
  max_priority_fee_per_gas?: string;
  gas_used?: number | null;
  effective_gas_price?: string | null;
  broadcasted_at?: string;
}

export interface OutboundTxIntent {
  id: Uuid;
  intent_type: OutboundIntentType;
  reference_id?: Uuid | null;
  chain_id: number;
  wallet_address: EvmAddress;
  nonce: number;
  status: OutboundTxIntentStatus;
  created_at: string;
  attempts: OutboundTxAttempt[];
}

export interface WalletNonceCursor {
  chain_id: number;
  wallet_address: EvmAddress;
  wallet_role: WalletRole;
  next_nonce: number;
  halted: boolean;
  updated_at?: string;
}

export interface OutboundQueueView {
  nonce_cursors: WalletNonceCursor[];
  halted_wallets: Array<{ chain_id: number; wallet_address: EvmAddress; intent_id: Uuid }>;
  data: OutboundTxIntent[];
  page: PageInfo;
}

export interface SignerHealth {
  reachable: boolean;
  mtls_ok?: boolean;
  signer_version?: string;
  uptime_seconds?: number;
  wallets?: Array<{ role: WalletRole; address: EvmAddress; chain_ids: number[]; loaded: boolean }>;
  circuit_breaker?: {
    tripped?: boolean;
    rolling_24h_used?: string;
    rolling_24h_limit?: string;
  };
  whitelist?: {
    active_count?: number;
    pending_cooldown_count?: number;
    revoked_count?: number;
  };
  intent_slots?: { open_slots?: number; filler_signed_slots?: number };
  audit_chain?: {
    valid?: boolean;
    last_seq?: number;
    last_entry_hash?: string;
    verified_at?: string;
    verification_interval_seconds?: number;
  };
}

export interface QuarantinedVault {
  deposit_address_id: Uuid;
  chain_id: number;
  address: EvmAddress;
  is_quarantined: boolean;
  asset_symbol?: string;
  reason?: string;
  sweep_batch_id?: Uuid | null;
  sweep_batch_item_id?: Uuid | null;
  item_status?: "PENDING" | "SWEPT" | "QUARANTINED_REVERT";
  related_order_id?: Uuid | null;
  quarantined_at?: string | null;
}

export interface QuarantinedVaultPage {
  data: QuarantinedVault[];
  page: PageInfo;
}

export interface MerchantFreezeState {
  merchant_id: Uuid;
  payout_frozen: boolean;
  reorg_receivable_balance: string;
  unfrozen_at?: string | null;
}

// --- dev simulator ----------------------------------------------------------

export interface SimStep {
  seq: number;
  name: string;
  status: SimStepStatus;
  detail?: string;
  tx_hash?: TxHash | null;
  at?: string;
}

export interface SimRun {
  run_id: Uuid;
  scenario: SimScenario;
  status: SimRunStatus;
  steps: SimStep[];
  summary?: Record<string, unknown>;
  error?: string | null;
  started_at: string;
  finished_at?: string | null;
}

// --- UI helpers -------------------------------------------------------------

/** Order states parked on human review (workbench focus). */
export const REVIEW_ORDER_STATUSES: readonly PaymentOrderStatus[] = [
  "UNDERPAID_EXPIRED_REVIEW",
  "LATE_PAID_REVIEW",
];

/** Order states where the checkout stops accepting new payment actions. */
export const CLOSED_ORDER_STATUSES: readonly PaymentOrderStatus[] = [
  "PAID",
  "OVERPAID",
  "EXPIRED",
  "REFUND_IN_PROGRESS",
  "REFUNDED",
  "SETTLED_REVERSED",
  "REVERSAL_RESOLVED",
];

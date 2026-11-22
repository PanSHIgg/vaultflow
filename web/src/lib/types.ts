// API contract types — exchange deposit app. Amounts on the wire are
// NUMERIC(78,0) decimal strings (see lib/amount.ts).

export interface AuthResponse {
  user_id: string;
  token: string;
}

export interface Asset {
  token_symbol: string;
  chain_id: number;
  /** Network display name, e.g. "Anvil Local" (EVM chains only). */
  chain_name?: string;
  token_contract: string;
  /** Explorer URL for the token contract (查看详情); absent → plain text. */
  token_explorer_url?: string;
  /** Approximate seconds per block — drives the 充币到账时间 estimate. */
  block_time_seconds?: number;
  required_confirmations: number;
  /** Atomic units (NUMERIC(78,0) decimal string); display via `decimals`. */
  min_deposit: string;
  decimals: number;
}

export interface DepositAddress {
  address: string;
  chain_id: number;
  token_contract: string;
  qr_payload: string;
}

export type DepositStatus = "PENDING" | "CONFIRMED" | "CREDITED" | "SWEPT";

export interface DepositRecord {
  tx_hash: string;
  token_symbol: string;
  /** Atomic units (NUMERIC(78,0) decimal string); display via `decimals`. */
  amount: string;
  decimals: number;
  confirmations: number;
  required_confirmations: number;
  status: DepositStatus;
  created_at: string;
}

export interface DepositPage {
  items: DepositRecord[];
  next_cursor?: string | null;
}

export interface Balance {
  token_symbol: string;
  /** Atomic units (NUMERIC(78,0) decimal strings); display via `decimals`. */
  available: string;
  pending: string;
  decimals: number;
}

export interface Withdrawal {
  id: string;
  chain_id: number;
  token_symbol: string;
  token_contract: string;
  to_address: string;
  /** Atomic units (NUMERIC(78,0) decimal string). */
  amount: string;
  status: string;
  tx_hash: string;
  fail_reason: string;
  created_at: string;
  updated_at: string;
}

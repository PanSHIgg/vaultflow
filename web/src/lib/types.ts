// API contract types — exchange deposit app. Amounts on the wire are
// NUMERIC(78,0) decimal strings (see lib/amount.ts).

export interface AuthResponse {
  user_id: string;
  token: string;
}

export interface Asset {
  token_symbol: string;
  chain_id: number;
  token_contract: string;
  required_confirmations: number;
  min_deposit: string;
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
  amount: string;
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
  chain_id: number;
  token_symbol: string;
  available: string;
  pending: string;
}

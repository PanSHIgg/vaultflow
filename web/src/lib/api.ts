// Typed REST client for the exchange wallet API contract.

import { apiBaseUrl } from "./env";
import { getToken } from "./auth";
import type {
  Asset,
  AuthResponse,
  Balance,
  DepositAddress,
  DepositPage,
} from "./types";

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(method: string, path: string, opts: { body?: unknown; auth?: boolean } = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (opts.body !== undefined) headers["content-type"] = "application/json";
  if (opts.auth !== false) {
    const token = getToken();
    if (token) headers["Authorization"] = `Bearer ${token}`;
  }
  const res = await fetch(`${apiBaseUrl()}${path}`, {
    method,
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = undefined;
  }
  if (!res.ok) {
    const err = (data as { error?: { code?: string; message?: string } } | undefined)?.error;
    throw new ApiError(res.status, err?.code ?? "INTERNAL", err?.message ?? `${res.status} ${res.statusText}`);
  }
  return data as T;
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

export function register(email: string, password: string): Promise<AuthResponse> {
  return request("POST", "/v1/auth/register", { body: { email, password }, auth: false });
}

export function login(email: string, password: string): Promise<AuthResponse> {
  return request("POST", "/v1/auth/login", { body: { email, password }, auth: false });
}

// ---------------------------------------------------------------------------
// Assets
// ---------------------------------------------------------------------------

export function listAssets(): Promise<Asset[]> {
  return request("GET", "/v1/assets", { auth: false });
}

// ---------------------------------------------------------------------------
// Me
// ---------------------------------------------------------------------------

export function getDepositAddress(chainId: number): Promise<DepositAddress> {
  return request("GET", `/v1/me/deposit-address/${chainId}`);
}

export function listDeposits(q: { chain_id?: number; cursor?: string } = {}): Promise<DepositPage> {
  const params = new URLSearchParams();
  if (q.chain_id != null) params.set("chain_id", String(q.chain_id));
  if (q.cursor) params.set("cursor", q.cursor);
  const qs = params.toString();
  return request("GET", `/v1/me/deposits${qs ? `?${qs}` : ""}`);
}

export function listBalances(): Promise<Balance[]> {
  return request("GET", "/v1/me/balances");
}

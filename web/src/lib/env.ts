// I2 — centralized NEXT_PUBLIC_* env accessors for the real-backend mode.
// Read lazily (functions, not module consts) so tests / Next runtime env stay
// authoritative. All values are optional; absent keys keep mock mode working.

/** Base URL of gateway-core REST/SSE. Empty string = same-origin (Next rewrite proxies /v1/*). */
export function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

/** Merchant X-Api-Key for the seeded demo merchant (deploy/merchant-demo.json). */
export function demoApiKey(): string | undefined {
  return process.env.NEXT_PUBLIC_DEMO_API_KEY;
}

/** Admin HS256 JWT for /v1/admin/* + /v1/sim/* (Authorization: Bearer). */
export function adminToken(): string | undefined {
  return process.env.NEXT_PUBLIC_ADMIN_TOKEN;
}

/** Anvil JSON-RPC endpoint used by the wagmi anvilLocal chain. */
export function anvilRpcUrl(): string {
  return process.env.NEXT_PUBLIC_ANVIL_RPC ?? "http://127.0.0.1:8545";
}

/**
 * Merchant UUID bound into signed 69B ADDR_V1 payloads. In real mode the
 * gateway rebuilds the payload with the *authenticated* merchant id, so this
 * must equal the merchant that owns NEXT_PUBLIC_DEMO_API_KEY — the seeded
 * `11111111-2222-3333-4444-555555555555`. Mock mode keeps the golden-vector id.
 */
export function demoMerchantId(): string {
  return process.env.NEXT_PUBLIC_DEMO_MERCHANT_ID ?? "00112233-4455-6677-8899-aabbccddeeff";
}

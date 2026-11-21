// Centralized NEXT_PUBLIC_* env accessors, read lazily.

/** Base URL of the gateway REST API. Empty string = same-origin (Next rewrite proxies /v1/*). */
export function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

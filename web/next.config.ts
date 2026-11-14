import type { NextConfig } from "next";

/**
 * I2 — real-backend integration.
 *
 * gateway-core emits no CORS headers, so the browser cannot call
 * http://localhost:8080 directly from the :3000 origin (the X-Api-Key /
 * Idempotency-Key headers would force a preflighted request the server
 * doesn't answer). Instead the app calls same-origin `/v1/*` and Next proxies
 * it upstream — this also keeps the SSE stream inside the page origin.
 * `NEXT_PUBLIC_API_BASE_URL` therefore defaults to "" (same-origin) in
 * .env.local; set GATEWAY_UPSTREAM to change the proxy target
 * (e.g. http://gateway-core:8080 inside compose).
 */
const GATEWAY_UPSTREAM = (process.env.GATEWAY_UPSTREAM ?? "http://localhost:8080").replace(/\/+$/, "");

const nextConfig: NextConfig = {
  async rewrites() {
    return [{ source: "/v1/:path*", destination: `${GATEWAY_UPSTREAM}/v1/:path*` }];
  },
};

export default nextConfig;

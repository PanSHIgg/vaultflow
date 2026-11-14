// W6-1 placeholder: local mock engine gate — when true the frontend runs fully
// against in-repo mock data aligned with docs/openapi.yaml (no gateway-core).
// Flip via NEXT_PUBLIC_USE_MOCK=false in Wave-2 window I2 for real integration.
export const USE_MOCK: boolean = process.env.NEXT_PUBLIC_USE_MOCK !== "false";

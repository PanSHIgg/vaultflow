// W6-1 — wagmi/viem client configuration.
// Chains: local Anvil (31337) + Base Sepolia + Sepolia testnets.
// In mock mode an extra `mock` connector exposes a deterministic demo payer so
// the cashier wallet flow is exercisable end-to-end without an extension.

import { createConfig, http } from "wagmi";
import { injected, mock } from "wagmi/connectors";
import { DEMO_PAYER, SUPPORTED_CHAINS } from "./chains";
import { USE_MOCK } from "./mock";

const transports = Object.fromEntries(
  SUPPORTED_CHAINS.map((c) => [c.id, http(c.rpcUrls.default.http[0])]),
) as Record<number, ReturnType<typeof http>>;

export const wagmiConfig = createConfig({
  chains: SUPPORTED_CHAINS,
  connectors: [
    injected({ shimDisconnect: true }),
    ...(USE_MOCK ? [mock({ accounts: [DEMO_PAYER] })] : []),
  ],
  transports,
  ssr: true,
});

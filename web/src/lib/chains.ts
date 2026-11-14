// W6-1 — chain & asset metadata for wagmi/viem wiring and UI badges.
// Mock chain values line up with docs/GOLDEN_VECTORS.json canonical addresses:
// factory 0x2222…, implementation 0x1111…, USDC 0x4444…, refund-to 0x5555….

import { defineChain, type Chain } from "viem";
import { baseSepolia, sepolia } from "viem/chains";
import { anvilRpcUrl } from "./env";
import type { EvmAddress } from "./types";

export const ANVIL_CHAIN_ID = 31337;

/** Local Anvil dev chain (docker-compose stack). */
export const anvilLocal: Chain = defineChain({
  id: ANVIL_CHAIN_ID,
  name: "Anvil Local",
  nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
  rpcUrls: {
    default: {
      http: [anvilRpcUrl()],
    },
  },
  testnet: true,
});

export const SUPPORTED_CHAINS: readonly [Chain, ...Chain[]] = [
  anvilLocal,
  baseSepolia,
  sepolia,
];

export function chainName(chainId: number): string {
  const c = SUPPORTED_CHAINS.find((x) => x.id === chainId);
  return c ? `${c.name} (${chainId})` : `Chain ${chainId}`;
}

export interface AssetInfo {
  chainId: number;
  symbol: string;
  decimals: number;
  tokenContract: EvmAddress;
  supportsEip3009: boolean;
  /** EIP-712 domain for `ReceiveWithAuthorization` (FiatTokenV2 USDC). */
  eip712Name?: string;
  eip712Version?: string;
}

/**
 * Dev asset registry — contract addresses of the Foundry deploy script.
 * The anvil entries are the REAL I1 docker-compose deployment
 * (deploy/deployed.json, deterministic Foundry addresses); the deployed
 * MockUSDC's EIP-712 domain is name="Mock USDC" version="2" — NOT "USD Coin",
 * so the cashier's ReceiveWithAuthorization signature must use it verbatim.
 */
export const ASSETS: readonly AssetInfo[] = [
  {
    chainId: ANVIL_CHAIN_ID,
    symbol: "USDC",
    decimals: 6,
    tokenContract: "0xa513e6e4b8f2a923d98304ec87f64353c4d5c853",
    supportsEip3009: true,
    eip712Name: "Mock USDC",
    eip712Version: "2",
  },
  {
    chainId: ANVIL_CHAIN_ID,
    symbol: "USDT",
    decimals: 6,
    tokenContract: "0x0165878a594ca255338adfa4d48449f69242eb8f",
    supportsEip3009: false,
  },
  {
    chainId: baseSepolia.id,
    symbol: "USDC",
    decimals: 6,
    tokenContract: "0x036cbd53842c5426634e7929541ec2318f3dcf7e",
    supportsEip3009: true,
    eip712Name: "USD Coin",
    eip712Version: "2",
  },
];

export function assetInfo(chainId: number, symbol: string): AssetInfo | undefined {
  return ASSETS.find((a) => a.chainId === chainId && a.symbol === symbol);
}

/**
 * Golden-vector placeholder addresses consumed ONLY by the in-browser mock
 * engine (mock-data/mock-engine + their tests) — do not point real calls at
 * these. The real I1 deployment is DEVNET_CONTRACTS below.
 */
export const DEV_CONTRACTS = {
  factory: "0x2222222222222222222222222222222222222222" as EvmAddress,
  implementation: "0x1111111111111111111111111111111111111111" as EvmAddress,
  eip3009Router: "0x7777777777777777777777777777777777777777" as EvmAddress,
  coldWallet: "0x8888888888888888888888888888888888888888" as EvmAddress,
  relayerHot: "0x9999999999999999999999999999999999999999" as EvmAddress,
  payoutHot: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" as EvmAddress,
};

/**
 * REAL contract/wallet addresses of the I1 docker-compose devnet
 * (deploy/deployed.json + deploy/gateway.env CHAIN_CONFIG_JSON, chain 31337).
 */
export const DEVNET_CONTRACTS = {
  factory: "0x2279b7a0a67db372996a5fab50d91eaa73d2ebe6" as EvmAddress,
  implementation: "0x06b1d212b8da92b83af328de5eef4e211da02097" as EvmAddress,
  eip3009Router: "0x8a791620dd6260079bf849dc5567adc3f2fdc318" as EvmAddress,
  batchSweeper: "0x94099942864ea81ccf197e9d71ac53310b1468d8" as EvmAddress,
  coldWallet: "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc" as EvmAddress,
  relayerHot: "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" as EvmAddress,
  payoutHot: "0x90f79bf6eb2c4f870365e785982e1f101e93b906" as EvmAddress,
};

/**
 * Demo payer account used by the wagmi `mock` connector — Anvil account #9
 * (0xa0Ee…9720, a free account that is NOT a platform hot/cold wallet).
 */
export const DEMO_PAYER = "0xa0ee7a142d267c1f36714e4a8f75612f20a79720" as EvmAddress;

export function eip3009RouterFor(chainId: number): EvmAddress | undefined {
  if (chainId === ANVIL_CHAIN_ID) return DEVNET_CONTRACTS.eip3009Router;
  return undefined;
}

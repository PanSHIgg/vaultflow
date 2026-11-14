// W6-2 — USDC EIP-3009 `ReceiveWithAuthorization` EIP-712 typed-data builder.
// Frozen bindings enforced by both gateway validation and the on-chain router
// (openapi.yaml / ARCHITECTURE §2.3):
//   to          == chain's EIP3009PayRouter address
//   value       == order.expected_amount (atomic units)
//   validAfter  == 0
//   validBefore == order.expires_at (unix seconds)
//   nonce       == authNonce == keccak256(abi.encode(chainId, salt))

import { hexToSignature } from "viem";
import type { AssetInfo } from "./chains";
import type { Eip3009Submission, EvmAddress, Hex, PublicPayment } from "./types";

export const RECEIVE_WITH_AUTHORIZATION_TYPES = {
  ReceiveWithAuthorization: [
    { name: "from", type: "address" },
    { name: "to", type: "address" },
    { name: "value", type: "uint256" },
    { name: "validAfter", type: "uint256" },
    { name: "validBefore", type: "uint256" },
    { name: "nonce", type: "bytes32" },
  ],
} as const;

// `type` (not `interface`) so it gets an implicit index signature and is
// assignable to wagmi's `Record<string, unknown>` message parameter.
export type ReceiveWithAuthMessage = {
  from: EvmAddress;
  to: EvmAddress;
  value: bigint;
  validAfter: bigint;
  validBefore: bigint;
  nonce: Hex;
};

export interface BuiltTypedData {
  domain: {
    name: string;
    version: string;
    chainId: number;
    verifyingContract: EvmAddress;
  };
  types: typeof RECEIVE_WITH_AUTHORIZATION_TYPES;
  primaryType: "ReceiveWithAuthorization";
  message: ReceiveWithAuthMessage;
}

export function unixSeconds(iso: string): bigint {
  return BigInt(Math.floor(Date.parse(iso) / 1000));
}

/**
 * Build the exact EIP-712 payload the payer must sign. Throws when the order
 * is not EIP-3009 eligible or the hint block is missing fields.
 */
export function buildReceiveWithAuthorization(
  payment: PublicPayment,
  asset: AssetInfo,
  payer: EvmAddress,
): BuiltTypedData {
  const hint = payment.eip3009;
  if (!hint?.supported) throw new Error("order asset does not support EIP-3009");
  if (!asset.supportsEip3009) throw new Error(`asset ${asset.symbol} lacks EIP-3009 metadata`);
  const validBefore = unixSeconds(payment.expires_at);
  return {
    domain: {
      name: asset.eip712Name ?? "USD Coin",
      version: asset.eip712Version ?? "2",
      chainId: payment.chain_id,
      verifyingContract: payment.token_contract,
    },
    types: RECEIVE_WITH_AUTHORIZATION_TYPES,
    primaryType: "ReceiveWithAuthorization",
    message: {
      from: payer,
      to: hint.router_address,
      value: BigInt(payment.expected_amount),
      validAfter: 0n,
      validBefore,
      nonce: hint.auth_nonce,
    },
  };
}

/** Split a 65-byte ECDSA signature into the v/r/s fields the API expects. */
export function signatureToSubmission(
  msg: ReceiveWithAuthMessage,
  signature: Hex,
): Eip3009Submission {
  const { r, s, v } = hexToSignature(signature);
  const vNum = Number(v);
  if (vNum !== 27 && vNum !== 28) {
    throw new Error(`unexpected signature v=${v} (want 27/28)`);
  }
  return {
    from: msg.from,
    to: msg.to,
    value: msg.value.toString(10),
    valid_after: Number(msg.validAfter),
    valid_before: Number(msg.validBefore),
    nonce: msg.nonce,
    v: vNum,
    r,
    s,
  };
}

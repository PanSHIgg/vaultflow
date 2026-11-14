// W6-1/W6-2 — FROZEN CREATE2 derivations shared across stacks (AGENTS.md §2.1).
//
//   salt         = keccak256(abi.encode(uint256(chainId), bytes16(merchantId), bytes16(orderId)))
//   auth_nonce   = keccak256(abi.encode(uint256(chainId), bytes32(salt)))
//   init_code    = 0x3d602d80600a3d3981f3 || 363d3d373d3d3d363d73 || <impl 20B> || 5af43d82803e903d91602b57fd5bf3
//   vault_addr   = last20(keccak256(0xff || factory(20B) || salt(32B) || keccak256(init_code)(32B)))
//
// bytes16 abi.encode is RIGHT-padded to 32B — viem's encodeAbiParameters does
// this natively (the #1 cross-stack divergence pitfall; never left-pad).

import {
  concat,
  encodeAbiParameters,
  getCreate2Address,
  keccak256,
  type Address,
  type Hex,
} from "viem";
import { bytesToHex0x, uuidToBytes16 } from "./bytes";

export const INIT_CODE_CREATION_PREFIX = "0x3d602d80600a3d3981f3" as const;
export const INIT_CODE_RUNTIME_PREFIX = "0x363d3d373d3d3d363d73" as const;
export const INIT_CODE_RUNTIME_SUFFIX = "0x5af43d82803e903d91602b57fd5bf3" as const;
export const INIT_CODE_BYTES = 55;
export const RUNTIME_CODE_BYTES = 45;

/**
 * Normalize a merchant/order id into `bytes16` hex.
 * Accepts: hyphenated UUID text, 0x/!0x 32-hex raw16.
 */
export function toBytes16Hex(id: string): Hex {
  if (id.includes("-")) {
    return bytesToHex0x(uuidToBytes16(id)) as Hex;
  }
  const h = id.startsWith("0x") ? id.slice(2) : id;
  if (!/^[0-9a-fA-F]{32}$/.test(h)) {
    throw new Error(`bytes16 id must be a UUID or 32-hex raw16, got ${JSON.stringify(id)}`);
  }
  return `0x${h.toLowerCase()}` as Hex;
}

/** salt = keccak256(abi.encode(uint256(chainId), bytes16(merchantId), bytes16(orderId))) */
export function deriveSalt(chainId: number | bigint, merchantId: string, orderId: string): Hex {
  return keccak256(
    encodeAbiParameters(
      [
        { name: "chainId", type: "uint256" },
        { name: "merchantId", type: "bytes16" },
        { name: "orderId", type: "bytes16" },
      ],
      [BigInt(chainId), toBytes16Hex(merchantId), toBytes16Hex(orderId)],
    ),
  );
}

/** auth_nonce = keccak256(abi.encode(uint256(chainId), bytes32(salt))) — USDC EIP-3009 nonce. */
export function deriveAuthNonce(chainId: number | bigint, salt: Hex): Hex {
  return keccak256(
    encodeAbiParameters(
      [
        { name: "chainId", type: "uint256" },
        { name: "salt", type: "bytes32" },
      ],
      [BigInt(chainId), salt],
    ),
  );
}

/** EIP-1167 minimal-proxy creation code (55B): prefix(10B) || runtime(45B). */
export function buildInitCode(implementation: Hex): Hex {
  const impl = implementation.toLowerCase();
  if (!/^0x[0-9a-f]{40}$/.test(impl)) {
    throw new Error(`implementation must be a 20-byte hex address, got ${implementation}`);
  }
  return concat([
    INIT_CODE_CREATION_PREFIX,
    INIT_CODE_RUNTIME_PREFIX,
    impl as Hex,
    INIT_CODE_RUNTIME_SUFFIX,
  ]);
}

export function initCodeHash(implementation: Hex): Hex {
  return keccak256(buildInitCode(implementation));
}

/** vault_address = last20(keccak256(0xff || factory || salt || init_code_hash)). */
export function deriveVaultAddress(factory: Hex, salt: Hex, codeHash: Hex): Address {
  return getCreate2Address({ from: factory as Address, salt, bytecodeHash: codeHash });
}

/** Full pipeline convenience: ids -> {salt, authNonce, vaultAddress}. */
export function deriveOrderVault(params: {
  chainId: number | bigint;
  merchantId: string;
  orderId: string;
  factory: Hex;
  implementation: Hex;
}): { salt: Hex; authNonce: Hex; vaultAddress: Address } {
  const salt = deriveSalt(params.chainId, params.merchantId, params.orderId);
  const authNonce = deriveAuthNonce(params.chainId, salt);
  const vaultAddress = deriveVaultAddress(
    params.factory,
    salt,
    initCodeHash(params.implementation),
  );
  return { salt, authNonce, vaultAddress };
}

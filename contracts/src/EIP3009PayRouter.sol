// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IVaultFactory} from "./interfaces/IVaultFactory.sol";

/// @notice Minimal USDC surface: EIP-3009 receiveWithAuthorization + ERC-20 transfer.
interface IEIP3009Token {
    function receiveWithAuthorization(
        address from,
        address to,
        uint256 value,
        uint256 validAfter,
        uint256 validBefore,
        bytes32 nonce,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external;
    function transfer(address to, uint256 value) external returns (bool);
}

/// @title EIP3009PayRouter
/// @notice Gasless USDC payment router (ARCHITECTURE §2.3). Recomputes the
///         CREATE2 vault on-chain from `salt` and self-enforces
///         `authNonce == keccak256(chain_id, salt)` so a compromised off-chain
///         path cannot hijack the recipient (double-locked with the Rust signer).
contract EIP3009PayRouter {
    /// @notice VaultFactory used for on-chain vault address self-derivation.
    IVaultFactory public immutable vaultFactory;
    /// @notice USDC token contract (EIP-3009 capable).
    IEIP3009Token public immutable usdc;

    constructor(address vaultFactory_, address usdc_) {
        require(vaultFactory_ != address(0) && usdc_ != address(0), "EIP3009PayRouter: zero address");
        vaultFactory = IVaultFactory(vaultFactory_);
        usdc = IEIP3009Token(usdc_);
    }

    /// @notice Relay a payer-signed EIP-3009 ReceiveWithAuthorization into the
    ///         order's unique DepositVault. Anyone may relay; funds cannot be
    ///         redirected because `vault` is derived on-chain.
    /// @dev validBefore is bound to order.expiresAt by the payer's signature —
    ///      after expiry the authorization is dead on-chain automatically.
    ///      TODO(W1): decide whether `to` stays router-then-transfer or signs
    ///      directly to vault (current: router pulls then pushes — one hop).
    /// @param salt        Order salt — vault = CREATE2(factory, salt, proxyHash).
    /// @param payer       EIP-3009 `from` (signed ReceiveWithAuthorization).
    /// @param amount      Exact order amount in token base units.
    /// @param validAfter  EIP-3009 validAfter (0 for immediate).
    /// @param validBefore EIP-3009 validBefore (== order.expiresAt).
    /// @param authNonce   MUST equal keccak256(abi.encode(block.chainid, salt)).
    /// @param v,r,s       Payer's EIP-712 signature over ReceiveWithAuthorization.
    function payOrderWithAuthorization(
        bytes32 salt,
        address payer,
        uint256 amount,
        uint256 validAfter,
        uint256 validBefore,
        bytes32 authNonce,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external {
        address vault = vaultFactory.predictVaultAddress(salt);
        require(
            authNonce == keccak256(abi.encode(block.chainid, salt)),
            "EIP3009PayRouter: authNonce mismatch"
        );
        usdc.receiveWithAuthorization(
            payer, address(this), amount, validAfter, validBefore, authNonce, v, r, s
        );
        require(usdc.transfer(vault, amount), "EIP3009PayRouter: vault transfer failed");
    }
}

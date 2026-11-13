// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title DepositVault
/// @notice EIP-1167 minimal-proxy IMPLEMENTATION. The immutable `SWEEPER` address
///         is baked into the bytecode at construction — proxies delegatecall into
///         this code and inherit the same SWEEPER gate with zero SLOAD cost.
/// @dev NO initialize() — immune to takeover front-runs (ARCHITECTURE §2.1).
///      W1-1 implements the inner 32-byte returndata-truncated transfer call.
contract DepositVault {
    /// @notice Sole caller allowed to trigger sweeps (the BatchSweeper contract).
    address public immutable SWEEPER;

    /// @param sweeper_ BatchSweeper address, injected once by VaultFactory.
    constructor(address sweeper_) {
        SWEEPER = sweeper_;
    }

    /// @notice Sweep exactly `exactAmount` of `token` to `to`.
    /// @dev Params (commented out until W1 lands the body):
    ///      `token`       — ERC-20 contract to sweep;
    ///      `to`          — destination (BatchSweeper's current coldWallet);
    ///      `exactAmount` — ledger-FINALIZED amount, never balanceOf.
    ///      TODO(W1-1): inline-asm `token.call` performing
    ///      `transfer(to, exactAmount)` that reads AT MOST the first 32 bytes of
    ///      returndata — both USDT no-return-value compatibility AND the inner
    ///      ReturnBomb memory-expansion defense (§2.2.2).
    function sweep(address /*token*/, address /*to*/, uint256 /*exactAmount*/) external {
        require(msg.sender == SWEEPER, "DepositVault: caller is not SWEEPER");
        // TODO(W1-1): inline-asm 32B-truncated transfer call (see dev note above).
    }
}

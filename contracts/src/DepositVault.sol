// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title DepositVault
/// @notice EIP-1167 minimal-proxy IMPLEMENTATION. The immutable `SWEEPER` address
///         is baked into the bytecode at construction — proxies delegatecall into
///         this code and inherit the same SWEEPER gate with zero SLOAD cost.
/// @dev NO initialize() — immune to takeover front-runs (ARCHITECTURE §2.1).
///      W1-1: the inner token call reads AT MOST the first 32 bytes of
///      returndata — both USDT no-return-value compatibility AND the inner
///      ReturnBomb memory-expansion defense (§2.2.2). The callee's returndata
///      is never copied beyond that bound, so a hostile token cannot force the
///      vault (and hence BatchSweeper's 120k-capped outer call) to pay
///      quadratic memory-expansion gas for a multi-megabyte payload.
contract DepositVault {
    /// @notice Sole caller allowed to trigger sweeps (the BatchSweeper contract).
    address public immutable SWEEPER;

    /// @param sweeper_ BatchSweeper address, injected once by VaultFactory.
    constructor(address sweeper_) {
        SWEEPER = sweeper_;
    }

    /// @notice Sweep exactly `exactAmount` of `token` to `to`.
    /// @param token       ERC-20 contract to sweep (must have code).
    /// @param to          destination (BatchSweeper's current coldWallet).
    /// @param exactAmount ledger-FINALIZED amount, never balanceOf (§2.2.1).
    /// @dev Success semantics: the inner `token.call` must return `true` at the
    ///      EVM level AND (if it returns data) its first 32-byte word must be
    ///      non-zero. USDT-style empty returndata counts as success; a token
    ///      returning `false` reverts; a token reverting (blacklist / balance
    ///      underflow / OOG inside its own frame) propagates a bounded <=32B
    ///      reason so BatchSweeper's try/catch can quarantine the single vault.
    ///      Returndata is read into a dedicated PRE-ZEROED buffer disjoint
    ///      from the calldata region, so a nonstandard short (1..31B) return
    ///      is judged on its own bytes only (FIX-C) — never on stale memory.
    function sweep(address token, address to, uint256 exactAmount) external {
        require(msg.sender == SWEEPER, "DepositVault: caller is not SWEEPER");
        require(to != address(0) && token.code.length != 0, "DepositVault: bad sweep params");
        assembly {
            let ptr := mload(0x40)
            // calldata = transfer(address,uint256) selector || to || exactAmount
            mstore(ptr, shl(224, 0xa9059cbb))
            mstore(add(ptr, 0x04), to)
            mstore(add(ptr, 0x24), exactAmount)
            // Dedicated, explicitly-zeroed 32-byte return buffer at ptr+0x60,
            // DISJOINT from the calldata region. (FIX-C / audit P2): reusing
            // `ptr` left stale calldata bytes in the high word when a token
            // returned a nonstandard 1..31 bytes — an all-zero "false" return
            // would have been misjudged as truthy. The retSize=0x20 cap still
            // applies: AT MOST 32 bytes of returndata ever enter memory, the
            // entire ReturnBomb defense (§2.2.2).
            let rbuf := add(ptr, 0x60)
            mstore(rbuf, 0)
            let ok := call(gas(), token, 0, ptr, 0x44, rbuf, 0x20)
            if iszero(ok) {
                // Propagate a bounded (<= 32 bytes) slice of the revert reason
                // for quarantine forensics; never a full returndatacopy.
                let n := returndatasize()
                if gt(n, 0x20) { n := 0x20 }
                returndatacopy(ptr, 0, n)
                revert(ptr, n)
            }
            // Non-empty returndata must decode as truthy bool (first word != 0).
            // Empty returndata (mainnet USDT shape) is success. Thanks to the
            // pre-zeroed buffer, a nonstandard 1..31B return is judged purely
            // on its own bytes: any nonzero byte => truthy, all-zero => false.
            if gt(returndatasize(), 0) {
                if iszero(mload(rbuf)) { revert(0, 0) }
            }
        }
    }
}

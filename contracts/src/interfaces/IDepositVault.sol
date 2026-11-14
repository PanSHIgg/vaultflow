// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Minimal call surface of a DepositVault clone, used by BatchSweeper.
interface IDepositVault {
    /// @notice The immutable BatchSweeper address baked into bytecode.
    function SWEEPER() external view returns (address);

    /// @notice Move `exactAmount` of `token` to `to`; only callable by SWEEPER.
    function sweep(address token, address to, uint256 exactAmount) external;
}

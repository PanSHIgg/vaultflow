// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Minimal call surface of VaultFactory used by BatchSweeper / EIP3009PayRouter.
///         Concrete implementation lives in src/VaultFactory.sol — kept as an
///         interface here so callers avoid circular imports.
interface IVaultFactory {
    /// @notice The BatchSweeper deployed by this factory (sole sweep authority).
    function sweeper() external view returns (address);

    /// @notice The DepositVault implementation cloned by deployProxy.
    function implementation() external view returns (address);

    /// @notice keccak256 of the frozen EIP-1167 45-byte proxy init code.
    ///         gateway-core asserts this against GOLDEN_VECTORS.json at startup.
    function initCodeHash() external view returns (bytes32);

    /// @notice CREATE2 prediction of the vault clone for `salt`.
    function predictVaultAddress(bytes32 salt) external view returns (address vault);

    /// @notice Deploy the EIP-1167 minimal proxy for `salt` (CREATE2). Permissioned
    ///         to the sweeper — BatchSweeper calls this on first sweep per vault.
    function deployProxy(bytes32 salt) external returns (address vault);
}

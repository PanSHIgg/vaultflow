// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {BatchSweeper} from "./BatchSweeper.sol";
import {DepositVault} from "./DepositVault.sol";
import {IVaultFactory} from "./interfaces/IVaultFactory.sol";

/// @title VaultFactory
/// @notice Zero-circular-dependency coordinator (ARCHITECTURE §2.1): the
///         constructor atomically deploys `BatchSweeper(this, ...)` then
///         `DepositVault(sweeper)`. Vault addresses are CREATE2 EIP-1167 clones
///         whose init code embeds only the invariant `implementation`, so hot
///         wallet / cold wallet rotation never changes `initCodeHash()`.
contract VaultFactory is IVaultFactory {
    /// @dev Standard 45-byte EIP-1167 minimal-proxy template (frozen by P0-1
    ///      GOLDEN_VECTORS): prefix(20B) || implementation(20B) || suffix(15B).
    bytes internal constant PROXY_PREFIX = hex"3d602d80600a3d3981f3363d3d373d3d3d363d73";
    bytes internal constant PROXY_SUFFIX = hex"5af43d82803e903d91602b57fd5bf3";

    /// @inheritdoc IVaultFactory
    address public immutable override sweeper;

    /// @inheritdoc IVaultFactory
    address public immutable override implementation;

    /// @notice Emitted when a new vault proxy is deployed via CREATE2.
    event VaultDeployed(address indexed vault, bytes32 indexed salt);

    /// @param initialRelayer      First relayerHotWallet (rotatable via 48h timelock).
    /// @param initialColdWallet   First coldWallet (rotatable via 48h timelock).
    /// @param governanceTimelock  Governance executor allowed to propose rotations
    ///                            and call rescueAsset on the sweeper.
    constructor(address initialRelayer, address initialColdWallet, address governanceTimelock) {
        sweeper = address(new BatchSweeper(address(this), initialRelayer, initialColdWallet, governanceTimelock));
        implementation = address(new DepositVault(sweeper));
    }

    /// @notice The frozen 55-byte proxy init code (public for W1/W3 verification).
    function proxyInitCode() public view returns (bytes memory initCode) {
        initCode = abi.encodePacked(PROXY_PREFIX, implementation, PROXY_SUFFIX);
    }

    /// @inheritdoc IVaultFactory
    function initCodeHash() public view override returns (bytes32) {
        return keccak256(proxyInitCode());
    }

    /// @inheritdoc IVaultFactory
    function predictVaultAddress(bytes32 salt) public view override returns (address vault) {
        // CREATE2: keccak256(0xff ++ address(this) ++ salt ++ keccak256(initCode))[12:]
        vault = address(uint160(uint256(keccak256(abi.encodePacked(
            bytes1(0xff), address(this), salt, initCodeHash()
        )))));
    }

    /// @inheritdoc IVaultFactory
    /// @dev Only the sweeper deploys vaults (first step of deployAndSweepBatch).
    ///      Idempotent for a never-deployed salt; CREATE2 to an already-occupied
    ///      address returns address(0) and is reverted defensively.
    function deployProxy(bytes32 salt) external override returns (address vault) {
        require(msg.sender == sweeper, "VaultFactory: caller is not sweeper");
        bytes memory initCode = proxyInitCode();
        assembly {
            vault := create2(0, add(initCode, 0x20), mload(initCode), salt)
        }
        require(vault != address(0), "VaultFactory: CREATE2 deploy failed");
        emit VaultDeployed(vault, salt);
    }
}

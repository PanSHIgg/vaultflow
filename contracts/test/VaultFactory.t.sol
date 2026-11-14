// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {VaultFactory} from "../src/VaultFactory.sol";
import {BatchSweeper} from "../src/BatchSweeper.sol";
import {DepositVault} from "../src/DepositVault.sol";
import {IDepositVault as IVault} from "../src/interfaces/IDepositVault.sol";

/// @title VaultFactoryTest
/// @notice W1-2: atomic construction, byte-exact EIP-1167 init code, CREATE2
///         predict/deploy round-trip, and sweeper-only deployment gating.
contract VaultFactoryTest is Test {
    VaultFactory internal factory;
    BatchSweeper internal sweeper;
    address internal relayer;
    address internal cold;
    address internal gov;

    bytes internal constant PROXY_PREFIX = hex"3d602d80600a3d3981f3363d3d373d3d3d363d73";
    bytes internal constant PROXY_SUFFIX = hex"5af43d82803e903d91602b57fd5bf3";

    function setUp() public {
        relayer = makeAddr("relayer");
        cold = makeAddr("cold");
        gov = address(this);
        factory = new VaultFactory(relayer, cold, gov);
        sweeper = BatchSweeper(payable(factory.sweeper()));
    }

    /// @notice Single-constructor wiring: sweeper knows the factory; the
    ///         implementation's immutable SWEEPER is the deployed sweeper.
    function test_ConstructorAtomicWiring() public view {
        assertEq(factory.sweeper(), address(sweeper));
        assertEq(sweeper.factory(), address(factory));
        assertEq(DepositVault(factory.implementation()).SWEEPER(), address(sweeper));
        assertEq(sweeper.relayerHotWallet(), relayer);
        assertEq(sweeper.coldWallet(), cold);
        assertEq(sweeper.timelock(), gov);
    }

    /// @notice proxyInitCode must be the byte-exact 55B template with the
    ///         implementation embedded at offset 20.
    function test_ProxyInitCodeByteExact() public view {
        bytes memory initCode = factory.proxyInitCode();
        bytes memory expected =
            abi.encodePacked(PROXY_PREFIX, factory.implementation(), PROXY_SUFFIX);
        assertEq(initCode.length, 55, "init code must be 55 bytes");
        assertEq(initCode, expected, "init code bytes mismatch");
        assertEq(factory.initCodeHash(), keccak256(expected), "initCodeHash mismatch");
    }

    /// @notice On-chain predictVaultAddress == independently recomputed CREATE2.
    function test_PredictMatchesManualCREATE2() public view {
        bytes32 salt = keccak256("order-salt-1");
        address predicted = factory.predictVaultAddress(salt);
        address manual = address(
            uint160(
                uint256(
                    keccak256(
                        abi.encodePacked(
                            bytes1(0xff), address(factory), salt, factory.initCodeHash()
                        )
                    )
                )
            )
        );
        assertEq(predicted, manual, "predict != manual CREATE2");
    }

    /// @notice deployProxy (sweeper-gated) lands at the predicted address with
    ///         the 45-byte runtime code whose delegatecall exposes SWEEPER.
    function test_DeployProxyLandsAtPrediction() public {
        bytes32 salt = keccak256("order-salt-2");
        address predicted = factory.predictVaultAddress(salt);

        vm.prank(address(sweeper));
        address deployed = factory.deployProxy(salt);

        assertEq(deployed, predicted, "deployed != predicted");
        assertEq(deployed.code.length, 45, "runtime code must be 45 bytes");
        // Through the proxy: immutable SWEEPER resolves via delegatecall.
        assertEq(IVault(deployed).SWEEPER(), address(sweeper));
    }

    /// @notice Only the sweeper may deploy proxies.
    function test_DeployProxyOnlySweeper() public {
        bytes32 salt = keccak256("order-salt-3");
        vm.expectRevert("VaultFactory: caller is not sweeper");
        factory.deployProxy(salt);
    }

    /// @notice A vault has no receive(): a plain ETH send reverts instead of
    ///         trapping native value forever (§9.3 stray-ETH boundary).
    function test_VaultRejectsPlainETH() public {
        bytes32 salt = keccak256("order-salt-eth");
        vm.prank(address(sweeper));
        address vault = factory.deployProxy(salt);
        vm.deal(address(this), 1 ether);
        (bool ok,) = vault.call{value: 1 ether}("");
        assertFalse(ok, "plain ETH send to vault must revert");
    }

    /// @notice CREATE2 to an occupied address returns address(0) -> defensive
    ///         revert (the batch path skips re-deploy via code.length check).
    function test_DeployProxySameSaltReverts() public {
        bytes32 salt = keccak256("order-salt-4");
        vm.prank(address(sweeper));
        factory.deployProxy(salt);
        vm.prank(address(sweeper));
        vm.expectRevert("VaultFactory: CREATE2 deploy failed");
        factory.deployProxy(salt);
    }
}

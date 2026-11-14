// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {Deploy} from "../script/Deploy.s.sol";
import {VaultFactory} from "../src/VaultFactory.sol";
import {BatchSweeper} from "../src/BatchSweeper.sol";
import {EIP3009PayRouter} from "../src/EIP3009PayRouter.sol";
import {MockUSDT} from "../src/mocks/MockUSDT.sol";
import {MockUSDC} from "../src/mocks/MockUSDC.sol";

/// @title DeployScriptTest
/// @notice Smoke test for script/Deploy.s.sol — full bundle deploys, custody
///         tokens are whitelisted, initCodeHash is queryable for gateway-core.
contract DeployScriptTest is Test {
    function test_DeployRun() public {
        Deploy deploy = new Deploy();
        (
            VaultFactory factory,
            BatchSweeper sweeper,
            EIP3009PayRouter router,
            MockUSDT usdt,
            MockUSDC usdc
        ) = deploy.run();

        assertTrue(address(factory) != address(0));
        assertTrue(address(sweeper) != address(0));
        assertTrue(address(router) != address(0));
        assertEq(factory.sweeper(), address(sweeper));
        assertTrue(factory.implementation() != address(0));
        assertTrue(factory.initCodeHash() != bytes32(0), "initCodeHash must be set");

        // Custody whitelist wired when governance == deployer (default env).
        assertTrue(sweeper.isSupportedCustodyToken(address(usdt)));
        assertTrue(sweeper.isSupportedCustodyToken(address(usdc)));
    }
}

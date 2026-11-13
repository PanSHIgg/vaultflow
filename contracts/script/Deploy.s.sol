// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Script} from "forge-std/Script.sol";

/// @title Deploy
/// @notice Deployment skeleton (W1-5 / Wave-2 I1). Broadcasts VaultFactory plus
///         MockUSDT/MockUSDC and exports addresses + initCodeHash() so
///         gateway-core can assert them at startup.
/// @dev Run: forge script script/Deploy.s.sol --rpc-url $ANVIL --broadcast
contract Deploy is Script {
    function run() external {
        // TODO(I1): vm.startBroadcast(deployerPk);
        //           new VaultFactory(initialRelayer, initialColdWallet, governanceTimelock);
        //           new MockUSDT(); new MockUSDC(); setSupportedCustodyToken(...)
        //           write factory / initCodeHash() / token addrs to a config file
        //           for gateway-core's VerifyChainFactoryHash startup check.
    }
}

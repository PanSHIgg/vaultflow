// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Script} from "forge-std/Script.sol";
import {console2} from "forge-std/console2.sol";
import {VaultFactory} from "../src/VaultFactory.sol";
import {BatchSweeper} from "../src/BatchSweeper.sol";
import {EIP3009PayRouter} from "../src/EIP3009PayRouter.sol";
import {MockUSDT} from "../src/mocks/MockUSDT.sol";
import {MockUSDC} from "../src/mocks/MockUSDC.sol";

/// @title Deploy
/// @notice Deployment script (W1-5 / Wave-2 I1). Broadcasts VaultFactory plus
///         MockUSDT/MockUSDC and prints addresses + initCodeHash() so
///         gateway-core can assert them at startup (VerifyChainFactoryHash).
/// @dev Run: forge script script/Deploy.s.sol --rpc-url $ANVIL --broadcast
///      Env overrides (defaults are Anvil deterministic accounts):
///        DEPLOYER_PK          (default anvil#0 private key)
///        RELAYER_HOT_WALLET   (default anvil#1)
///        COLD_WALLET          (default anvil#2)
///        GOVERNANCE_TIMELOCK  (default = deployer)
contract Deploy is Script {
    uint256 internal constant ANVIL_PK0 =
        0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80;
    address internal constant ANVIL_ADDR1 = 0x70997970C51812dc3A010C7d01b50e0d17dc79C8;
    address internal constant ANVIL_ADDR2 = 0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC;

    function run()
        external
        returns (VaultFactory factory, BatchSweeper sweeper, EIP3009PayRouter router, MockUSDT usdt, MockUSDC usdc)
    {
        uint256 deployerPk = vm.envOr("DEPLOYER_PK", ANVIL_PK0);
        address deployer = vm.addr(deployerPk);
        address relayer = vm.envOr("RELAYER_HOT_WALLET", ANVIL_ADDR1);
        address cold = vm.envOr("COLD_WALLET", ANVIL_ADDR2);
        address governance = vm.envOr("GOVERNANCE_TIMELOCK", deployer);

        vm.startBroadcast(deployerPk);
        usdt = new MockUSDT();
        usdc = new MockUSDC();
        factory = new VaultFactory(relayer, cold, governance);
        sweeper = BatchSweeper(payable(factory.sweeper()));
        router = new EIP3009PayRouter(address(factory), address(usdc));
        if (governance == deployer) {
            // Whitelist the custody stablecoins so rescueAsset hard-refuses them.
            sweeper.setSupportedCustodyToken(address(usdt), true);
            sweeper.setSupportedCustodyToken(address(usdc), true);
        }
        vm.stopBroadcast();

        console2.log("=== VaultFlow deployment ===");
        console2.log("MockUSDT:             ", address(usdt));
        console2.log("MockUSDC:             ", address(usdc));
        console2.log("VaultFactory:         ", address(factory));
        console2.log("BatchSweeper:         ", address(sweeper));
        console2.log("DepositVault impl:    ", factory.implementation());
        console2.log("EIP3009PayRouter:     ", address(router));
        console2.log("relayerHotWallet:     ", relayer);
        console2.log("coldWallet:           ", cold);
        console2.log("governanceTimelock:   ", governance);
        console2.log("initCodeHash (gateway-core startup assert):");
        console2.logBytes32(factory.initCodeHash());
    }
}

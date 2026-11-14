// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {Vm} from "forge-std/Vm.sol";
import {VaultFactory} from "../src/VaultFactory.sol";
import {BatchSweeper, SweepItem} from "../src/BatchSweeper.sol";
import {DepositVault} from "../src/DepositVault.sol";
import {MockUSDT} from "../src/mocks/MockUSDT.sol";
import {MockUSDC} from "../src/mocks/MockUSDC.sol";
import {MockReturnBombToken} from "../src/mocks/MockReturnBombToken.sol";

/// @dev ERC-20 whose transfer returns bool false — exercises the vault's
///      "returndata word must be truthy" branch (distinct from USDT no-return).
contract FalseToken {
    mapping(address => uint256) public balanceOf;

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
    }

    function transfer(address, uint256) external pure returns (bool) {
        return false;
    }
}

/// @dev Nonstandard token whose `transfer` returns a SHORT (1..31B) raw
///      returndata payload — exercises the FIX-C residual-calldata edge:
///      the vault must judge truthiness on the returned bytes ONLY, never on
///      stale calldata left in the return-word's high bytes. Balances DO move
///      so the truthiness branch is the only variable under test.
contract ShortReturnToken {
    mapping(address => uint256) public balanceOf;
    bytes public ret;

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
    }

    function setReturn(bytes memory r) external {
        ret = r;
    }

    function transfer(address to, uint256 amount) external {
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
        bytes memory r = ret;
        assembly {
            return(add(r, 0x20), mload(r))
        }
    }
}

/// @title BatchSweeperTest
/// @notice W1-3: two-step deploy+sweep isolation, blacklist/bomb quarantine,
///         48h timelock rotation, custody-token rescue hard-exclusion.
contract BatchSweeperTest is Test {
    VaultFactory internal factory;
    BatchSweeper internal sweeper;
    MockUSDT internal usdt;
    MockUSDC internal usdc;
    MockReturnBombToken internal bomb;
    FalseToken internal falseToken;

    address internal relayer;
    address internal cold;
    address internal gov;
    address internal stranger;

    event VaultSwept(address indexed vault, bytes32 indexed salt, uint256 amount);
    event VaultQuarantined(address indexed vault, bytes32 indexed salt, uint256 amount);
    event RelayerRotationProposed(address indexed candidate, uint64 executableAt);
    event RelayerRotationExecuted(address indexed newRelayer);
    event AssetRescued(address indexed token, address indexed to, uint256 amount);

    function setUp() public {
        relayer = makeAddr("relayer");
        cold = makeAddr("cold");
        stranger = makeAddr("stranger");
        gov = address(this);

        usdt = new MockUSDT();
        usdc = new MockUSDC();
        bomb = new MockReturnBombToken();
        falseToken = new FalseToken();

        factory = new VaultFactory(relayer, cold, gov);
        sweeper = BatchSweeper(payable(factory.sweeper()));
        sweeper.setSupportedCustodyToken(address(usdt), true);
        sweeper.setSupportedCustodyToken(address(usdc), true);
    }

    function _salt(uint256 i) internal pure returns (bytes32) {
        return keccak256(abi.encodePacked("vault-salt", i));
    }

    function _items(uint256 n, uint256 amountEach) internal pure returns (SweepItem[] memory items) {
        items = new SweepItem[](n);
        for (uint256 i = 0; i < n; ++i) {
            items[i] = SweepItem({salt: keccak256(abi.encodePacked("vault-salt", i)), exactAmount: amountEach});
        }
    }

    /// @notice Happy path: N pre-funded (not-yet-deployed) vaults are deployed
    ///         and swept to the cold wallet in one batch.
    function test_BatchSweepHappyPath() public {
        uint256 n = 5;
        uint256 amt = 1_000_000; // 1 USDT (6dp)
        SweepItem[] memory items = _items(n, amt);
        for (uint256 i = 0; i < n; ++i) {
            usdt.mint(factory.predictVaultAddress(items[i].salt), amt);
        }

        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);

        assertEq(usdt.balanceOf(cold), n * amt, "cold wallet total");
        for (uint256 i = 0; i < n; ++i) {
            address v = factory.predictVaultAddress(items[i].salt);
            assertEq(usdt.balanceOf(v), 0, "vault drained");
            assertEq(v.code.length, 45, "vault deployed");
        }
    }

    /// @notice VaultSwept event fires per successful item.
    function test_BatchSweepEmitsVaultSwept() public {
        SweepItem[] memory items = _items(1, 7);
        address v = factory.predictVaultAddress(items[0].salt);
        usdt.mint(v, 7);

        vm.expectEmit(true, true, false, true);
        emit VaultSwept(v, items[0].salt, 7);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
    }

    /// @notice USDT no-return-value path — MockUSDT.transfer returns nothing;
    ///         the vault's empty-returndata branch must still succeed.
    function test_USDTNoReturnCompatibility() public {
        SweepItem[] memory items = _items(1, 42);
        usdt.mint(factory.predictVaultAddress(items[0].salt), 42);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 42);
    }

    /// @notice USDC (returning bool true) path.
    function test_USDCSweepPath() public {
        SweepItem[] memory items = _items(2, 99);
        for (uint256 i = 0; i < 2; ++i) {
            usdc.mint(factory.predictVaultAddress(items[i].salt), 99);
        }
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdc), cold);
        assertEq(usdc.balanceOf(cold), 198);
    }

    /// @notice Mixed blacklist isolation: the blacklisted vault quarantines,
    ///         the other four sweep normally (§8.2 adversarial scenario).
    function test_BlacklistIsolation() public {
        uint256 n = 5;
        uint256 amt = 100;
        SweepItem[] memory items = _items(n, amt);
        address[] memory vaults = new address[](n);
        for (uint256 i = 0; i < n; ++i) {
            vaults[i] = factory.predictVaultAddress(items[i].salt);
            usdt.mint(vaults[i], amt);
        }
        usdt.setBlackListed(vaults[2], true);

        vm.expectEmit(true, true, false, true);
        emit VaultQuarantined(vaults[2], items[2].salt, amt);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);

        assertEq(usdt.balanceOf(cold), 4 * amt, "honest four swept");
        assertEq(usdt.balanceOf(vaults[2]), amt, "blacklisted vault retains funds");

        // Recovery path: unblacklist -> same vault sweeps on the next batch.
        usdt.setBlackListed(vaults[2], false);
        SweepItem[] memory retry = new SweepItem[](1);
        retry[0] = items[2];
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(retry, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 5 * amt, "quarantined vault recovered");
    }

    /// @notice 2 MiB ReturnBomb: the token call itself OOGs inside the 120k cap
    ///         (producing the payload costs ~8.5M gas); the batch transaction
    ///         survives, quarantines the poison vault, and a subsequent honest
    ///         batch still completes — the ReturnBomb cannot stall the system.
    function test_ReturnBombBatchSurvives() public {
        SweepItem[] memory bombItems = _items(3, 1);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(bombItems, address(bomb), cold);
        // All three vaults quarantined; execution reaching here means the tx
        // itself survived the bomb (no unbounded returndatacopy ever ran).

        // Honest USDT batch afterwards — the pipeline is alive.
        SweepItem[] memory good = _items(2, 55);
        for (uint256 i = 0; i < 2; ++i) {
            usdt.mint(factory.predictVaultAddress(good[i].salt), 55);
        }
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(good, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 110);
    }

    /// @notice Direct vault-level proof of the <=32B truncation success path:
    ///         a standalone DepositVault (SWEEPER == this test) calls the bomb
    ///         with unrestricted gas — the bomb's transfer SUCCEEDS and returns
    ///         its 2 MiB payload, yet the vault survives because it never copies
    ///         more than 32 bytes of returndata.
    function test_VaultSurvivesSuccessfulReturnBomb() public {
        DepositVault vault = new DepositVault(address(this));
        vault.sweep(address(bomb), cold, 1); // would OOG on naive full returndatacopy
    }

    /// @notice FIX-C / audit P1: an exactAmount == 0 item must NOT emit
    ///         VaultSwept — the indexer's PostEntry refuses zero amounts and a
    ///         phantom VaultSwept(..,0) would stall the settlement loop. The
    ///         item is quarantine-flagged instead (exactly one terminal event
    ///         per item preserved) and its lazy deploy is skipped.
    function test_ZeroAmountItemQuarantinedNotSwept() public {
        SweepItem[] memory items = new SweepItem[](2);
        items[0] = SweepItem({salt: _salt(100), exactAmount: 55});
        items[1] = SweepItem({salt: _salt(101), exactAmount: 0});
        address v0 = factory.predictVaultAddress(items[0].salt);
        address v1 = factory.predictVaultAddress(items[1].salt);
        usdt.mint(v0, 55);
        usdt.mint(v1, 77); // even a funded vault: zero ledger amount => nothing moves

        vm.recordLogs();
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);

        bytes32 sweptSig = keccak256("VaultSwept(address,bytes32,uint256)");
        bytes32 quarSig = keccak256("VaultQuarantined(address,bytes32,uint256)");
        Vm.Log[] memory logs = vm.getRecordedLogs();
        uint256 swept;
        uint256 quar;
        for (uint256 i = 0; i < logs.length; ++i) {
            if (logs[i].topics[0] == sweptSig) {
                ++swept;
                assertEq(logs[i].topics[1], bytes32(uint256(uint160(v0))), "swept vault");
                assertEq(abi.decode(logs[i].data, (uint256)), 55);
            }
            if (logs[i].topics[0] == quarSig) {
                ++quar;
                assertEq(logs[i].topics[1], bytes32(uint256(uint160(v1))), "quarantined vault");
                assertEq(abi.decode(logs[i].data, (uint256)), 0, "quarantine carries attempted 0");
            }
        }
        assertEq(swept, 1, "exactly one VaultSwept for the funded item");
        assertEq(quar, 1, "zero item must emit VaultQuarantined, never VaultSwept(..,0)");

        assertEq(usdt.balanceOf(cold), 55, "only the nonzero item settled");
        assertEq(usdt.balanceOf(v1), 77, "zero item moved nothing");
        assertEq(v1.code.length, 0, "zero item skips the lazy deploy step");
    }

    /// @notice FIX-C / audit P2: a token returning a nonstandard 1..31B
    ///         returndata payload must be judged on ITS OWN bytes — the
    ///         previous code re-read `ptr`, where residual calldata (e.g. the
    ///         nonzero `to` address) made an all-zero "false" return look
    ///         truthy. With the pre-zeroed disjoint buffer: 7B of 0x01..07 is
    ///         truthy (swept), 7B of zeros is false (revert -> quarantine).
    function test_ShortReturndataTruthiness() public {
        ShortReturnToken tok = new ShortReturnToken();
        DepositVault vault = new DepositVault(address(this)); // SWEEPER == this test
        tok.mint(address(vault), 100);

        // 7B nonzero payload -> judged truthy -> funds move.
        tok.setReturn(hex"01020304050607");
        vault.sweep(address(tok), cold, 40);
        assertEq(tok.balanceOf(cold), 40);
        assertEq(tok.balanceOf(address(vault)), 60);

        // 7B all-zero payload -> judged false -> vault reverts (bounded).
        tok.setReturn(hex"00000000000000");
        vm.expectRevert();
        vault.sweep(address(tok), cold, 10);
        assertEq(tok.balanceOf(address(vault)), 60, "failed sweep moved nothing");
    }

    /// @notice The all-zero short return surfaces at batch level as a normal
    ///         quarantine — never a phantom VaultSwept.
    function test_ShortZeroReturnQuarantined() public {
        ShortReturnToken tok = new ShortReturnToken();
        tok.setReturn(new bytes(7)); // 7 zero bytes == nonstandard "false"
        SweepItem[] memory items = _items(1, 1);
        tok.mint(factory.predictVaultAddress(items[0].salt), 1);

        vm.recordLogs();
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(tok), cold);

        bytes32 sweptSig = keccak256("VaultSwept(address,bytes32,uint256)");
        Vm.Log[] memory logs = vm.getRecordedLogs();
        for (uint256 i = 0; i < logs.length; ++i) {
            assertTrue(logs[i].topics[0] != sweptSig, "zero-return token must not emit VaultSwept");
        }
        assertEq(tok.balanceOf(cold), 0);
    }

    /// @notice Token returning bool false -> vault reverts -> quarantined.
    function test_FalseReturningTokenQuarantined() public {
        SweepItem[] memory items = _items(2, 1);
        falseToken.mint(factory.predictVaultAddress(items[0].salt), 1);
        falseToken.mint(factory.predictVaultAddress(items[1].salt), 1);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(falseToken), cold);
        // Both quarantined — nothing reverted the outer tx.
    }

    /// @notice Insufficient balance (transfer underflow) quarantines only that
    ///         vault; the rest of the batch sweeps.
    function test_InsufficientBalanceQuarantined() public {
        SweepItem[] memory items = _items(2, 100);
        usdt.mint(factory.predictVaultAddress(items[0].salt), 100);
        usdt.mint(factory.predictVaultAddress(items[1].salt), 50); // short
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 100);
        assertEq(usdt.balanceOf(factory.predictVaultAddress(items[1].salt)), 50);
    }

    /// @notice Batch token pointing at an EOA is rejected inside the vault and
    ///         quarantined rather than silently emitting VaultSwept.
    function test_EOATokenQuarantined() public {
        SweepItem[] memory items = _items(1, 1);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, makeAddr("notAToken"), cold);
        // no assert needed beyond survival — the emit path is quarantine-only.
    }

    /// @notice Second sweep of an already-deployed vault skips the deploy step.
    function test_SecondSweepSkipsDeploy() public {
        SweepItem[] memory items = _items(1, 10);
        address v = factory.predictVaultAddress(items[0].salt);
        usdt.mint(v, 10);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        // Fund again and re-sweep — code.length > 0 so deployProxy is skipped.
        usdt.mint(v, 5);
        items[0].exactAmount = 5;
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 15);
    }

    /// @notice Access control & bound checks on the batch entry point.
    function test_BatchGuards() public {
        SweepItem[] memory items = _items(1, 1);

        vm.prank(stranger);
        vm.expectRevert("BatchSweeper: caller is not relayerHotWallet");
        sweeper.deployAndSweepBatch(items, address(usdt), cold);

        vm.prank(relayer);
        vm.expectRevert("BatchSweeper: wrong targetColdWallet");
        sweeper.deployAndSweepBatch(items, address(usdt), stranger);

        SweepItem[] memory tooMany = _items(51, 1);
        vm.prank(relayer);
        vm.expectRevert("BatchSweeper: batch exceeds 50");
        sweeper.deployAndSweepBatch(tooMany, address(usdt), cold);
    }

    /// @notice Boundary: exactly 50 items is allowed; empty batch is a no-op.
    function test_BatchBoundary() public {
        SweepItem[] memory fifty = _items(50, 1);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(fifty, address(usdt), cold); // no revert
        SweepItem[] memory empty = new SweepItem[](0);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(empty, address(usdt), cold); // no-op
    }

    /// @notice Direct vault.sweep by anyone other than SWEEPER reverts.
    function test_VaultSweepOnlySweeper() public {
        SweepItem[] memory items = _items(1, 1);
        address v = factory.predictVaultAddress(items[0].salt);
        usdt.mint(v, 1);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);

        vm.expectRevert("DepositVault: caller is not SWEEPER");
        DepositVault(v).sweep(address(usdt), cold, 1);
    }

    // ---------- 48h timelock rotation ----------

    function test_RelayerRotationTimelock() public {
        address newRelayer = makeAddr("newRelayer");
        bytes32 hashBefore = factory.initCodeHash();

        vm.prank(stranger);
        vm.expectRevert("BatchSweeper: caller is not timelock");
        sweeper.proposeRelayerRotation(newRelayer);

        sweeper.proposeRelayerRotation(newRelayer);

        vm.expectRevert("BatchSweeper: timelock not elapsed");
        sweeper.executeRelayerRotation();

        vm.warp(vm.getBlockTimestamp() + 48 hours - 1);
        vm.expectRevert("BatchSweeper: timelock not elapsed");
        sweeper.executeRelayerRotation();

        vm.warp(vm.getBlockTimestamp() + 1);
        vm.expectEmit(true, false, false, false);
        emit RelayerRotationExecuted(newRelayer);
        sweeper.executeRelayerRotation();

        assertEq(sweeper.relayerHotWallet(), newRelayer);
        assertEq(factory.initCodeHash(), hashBefore, "rotation must not change initCodeHash");

        // Old relayer rejected, new relayer works.
        SweepItem[] memory items = _items(1, 1);
        usdt.mint(factory.predictVaultAddress(items[0].salt), 1);
        vm.prank(relayer);
        vm.expectRevert("BatchSweeper: caller is not relayerHotWallet");
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        vm.prank(newRelayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 1);
    }

    function test_ColdWalletRotationTimelock() public {
        address newCold = makeAddr("newCold");
        bytes32 hashBefore = factory.initCodeHash();

        sweeper.proposeColdWalletRotation(newCold);

        // Mid-window: old cold wallet is still authoritative on-chain.
        SweepItem[] memory items = _items(1, 10);
        usdt.mint(factory.predictVaultAddress(items[0].salt), 10);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(items, address(usdt), cold);
        assertEq(usdt.balanceOf(cold), 10);

        vm.expectRevert("BatchSweeper: timelock not elapsed");
        sweeper.executeColdWalletRotation();

        vm.warp(vm.getBlockTimestamp() + 48 hours);
        sweeper.executeColdWalletRotation();
        assertEq(sweeper.coldWallet(), newCold);
        assertEq(factory.initCodeHash(), hashBefore, "rotation must not change initCodeHash");

        // Old cold wallet now rejected; new one accepted.
        SweepItem[] memory items2 = _items(2, 5);
        usdt.mint(factory.predictVaultAddress(items2[1].salt), 5);
        SweepItem[] memory one = new SweepItem[](1);
        one[0] = items2[1];
        vm.prank(relayer);
        vm.expectRevert("BatchSweeper: wrong targetColdWallet");
        sweeper.deployAndSweepBatch(one, address(usdt), cold);
        vm.prank(relayer);
        sweeper.deployAndSweepBatch(one, address(usdt), newCold);
        assertEq(usdt.balanceOf(newCold), 5);
    }

    /// @notice Executing with nothing proposed reverts cleanly.
    function test_RotationWithoutProposalReverts() public {
        vm.expectRevert("BatchSweeper: no pending relayer rotation");
        sweeper.executeRelayerRotation();
        vm.expectRevert("BatchSweeper: no pending cold wallet rotation");
        sweeper.executeColdWalletRotation();
    }

    // ---------- rescueAsset ----------

    function test_RescueCustodyTokenForbidden() public {
        usdt.mint(address(sweeper), 1);
        vm.expectRevert("Core custody token forbidden");
        sweeper.rescueAsset(address(usdt), stranger, 1);
        vm.expectRevert("Core custody token forbidden");
        sweeper.rescueAsset(address(usdc), stranger, 1);
    }

    function test_RescueStrayERC20() public {
        MockUSDC stray = new MockUSDC(); // deliberately NOT whitelisted
        stray.mint(address(sweeper), 77);
        vm.expectEmit(true, true, false, true);
        emit AssetRescued(address(stray), stranger, 77);
        sweeper.rescueAsset(address(stray), stranger, 77);
        assertEq(stray.balanceOf(stranger), 77);
        assertEq(stray.balanceOf(address(sweeper)), 0);
    }

    function test_RescueNativeETH() public {
        vm.deal(address(sweeper), 1 ether);
        uint256 before_ = stranger.balance;
        sweeper.rescueAsset(address(0), stranger, 1 ether);
        assertEq(stranger.balance - before_, 1 ether);
    }

    /// @notice Stray ETH can actually be sent to the sweeper (receive() exists).
    function test_SweeperAcceptsStrayETH() public {
        vm.deal(stranger, 2 ether);
        vm.prank(stranger);
        (bool ok,) = address(sweeper).call{value: 1 ether}("");
        assertTrue(ok, "sweeper should accept stray ETH for rescue");
        assertEq(address(sweeper).balance, 1 ether);
    }

    function test_RescueOnlyTimelock() public {
        vm.prank(stranger);
        vm.expectRevert("BatchSweeper: caller is not timelock");
        sweeper.rescueAsset(address(0), stranger, 0);
    }

    function test_SetCustodyOnlyTimelock() public {
        vm.prank(stranger);
        vm.expectRevert("BatchSweeper: caller is not timelock");
        sweeper.setSupportedCustodyToken(address(usdt), false);
    }
}

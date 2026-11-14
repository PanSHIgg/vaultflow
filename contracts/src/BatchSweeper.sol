// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IDepositVault} from "./interfaces/IDepositVault.sol";
import {IVaultFactory} from "./interfaces/IVaultFactory.sol";

/// @notice One vault per order — `salt` selects the CREATE2 vault, `exactAmount`
///         is the ledger-FINALIZED amount to move (never balanceOf; §2.2.1).
struct SweepItem {
    bytes32 salt;
    uint256 exactAmount;
}

/// @title BatchSweeper
/// @notice Two-step exact-amount batch sweeper with 48h two-step timelock
///         rotation for `relayerHotWallet` / `coldWallet`, and a rescue path that
///         hard-excludes custody tokens (§2.2).
/// @dev Deployed inside VaultFactory's constructor; address is immutable-baked
///      into every DepositVault proxy. `initCodeHash` therefore survives all
///      key/wallet rotations.
contract BatchSweeper {
    uint256 public constant MAX_BATCH_ITEMS = 50;
    /// @dev Outer-call gas cap for the per-vault sweep step (§2.2.2).
    uint256 public constant SWEEP_GAS_CAP = 120_000;
    uint256 public constant ROTATION_TIMELOCK = 48 hours;

    /// @notice The VaultFactory that deployed this contract (deploys proxies).
    address public immutable factory;
    /// @notice Governance/timelock executor: proposes rotations, rescues assets.
    address public immutable timelock;

    /// @notice Only EOA allowed to call deployAndSweepBatch (holds gas ETH only).
    address public relayerHotWallet;
    /// @notice Cold treasury — sweep destination; checked on-chain per batch.
    address public coldWallet;

    /// @notice Pending two-step timelock rotation slot.
    /// @param candidate     Replacement address awaiting activation.
    /// @param executableAt  Earliest unix timestamp the rotation may execute.
    struct PendingRotation {
        address candidate;
        uint64 executableAt;
    }

    PendingRotation public pendingRelayerRotation;
    PendingRotation public pendingColdWalletRotation;

    /// @notice Custody-token registry — rescueAsset MUST refuse these (§2.2.3).
    ///         Populated by governance (deploy script marks USDT/USDC).
    mapping(address => bool) public isSupportedCustodyToken;

    event VaultSwept(address indexed vault, bytes32 indexed salt, uint256 amount);
    event VaultQuarantined(address indexed vault, bytes32 indexed salt, uint256 amount);
    event RelayerRotationProposed(address indexed candidate, uint64 executableAt);
    event RelayerRotationExecuted(address indexed newRelayer);
    event ColdWalletRotationProposed(address indexed candidate, uint64 executableAt);
    event ColdWalletRotationExecuted(address indexed newColdWallet);
    event AssetRescued(address indexed token, address indexed to, uint256 amount);

    constructor(address factory_, address relayerHotWallet_, address coldWallet_, address timelock_) {
        require(
            factory_ != address(0) && relayerHotWallet_ != address(0) && coldWallet_ != address(0)
                && timelock_ != address(0),
            "BatchSweeper: zero address"
        );
        factory = factory_;
        relayerHotWallet = relayerHotWallet_;
        coldWallet = coldWallet_;
        timelock = timelock_;
    }

    /// @dev Stray native ETH may land here; rescuable via rescueAsset(token==0).
    ///      DepositVault proxies intentionally have NO receive() — a plain ETH
    ///      send to a vault reverts instead of being trapped forever.
    receive() external payable {}

    /// @notice Deploy (if needed) + sweep up to 50 vaults of one `token` into the
    ///         current cold wallet. Per-item failures are quarantined, never fatal.
    ///         Zero-`exactAmount` items are NOT swept: they emit
    ///         `VaultQuarantined(vault, salt, 0)` (off-chain review flag) so the
    ///         "one terminal event per item" indexer convention still holds —
    ///         no `VaultSwept(..,0)` is ever emitted (FIX-C).
    /// @dev Two steps per item (§2.2.2):
    ///      1. deploy proxy WITHOUT a gas cap (trusted internal CREATE2);
    ///      2. `sweep{gas: SWEEP_GAS_CAP}` under try/catch — a ReturnBomb/USDT-
    ///         blacklist revert is isolated to that single vault.
    ///      Reentrancy: any token-level reenter into this function sees
    ///      msg.sender == vault != relayerHotWallet and reverts; reentering
    ///      deployProxy sees msg.sender == vault != sweeper and reverts.
    function deployAndSweepBatch(SweepItem[] calldata items, address token, address targetColdWallet)
        external
    {
        require(items.length <= MAX_BATCH_ITEMS, "BatchSweeper: batch exceeds 50");
        require(msg.sender == relayerHotWallet, "BatchSweeper: caller is not relayerHotWallet");
        require(targetColdWallet == coldWallet, "BatchSweeper: wrong targetColdWallet");

        for (uint256 i = 0; i < items.length; ++i) {
            bytes32 salt = items[i].salt;
            uint256 exactAmount = items[i].exactAmount;
            address vault = IVaultFactory(factory).predictVaultAddress(salt);

            // FIX-C (audit P1): a zero exactAmount must never reach the sweep
            // boundary — `transfer(0)` succeeds on USDT/USDC and would emit
            // VaultSwept(..,0), which the indexer's ledger PostEntry refuses
            // (zero amounts), stalling the settlement loop. The item is
            // quarantine-flagged instead for off-chain review: every item
            // still emits EXACTLY ONE terminal event (VaultSwept XOR
            // VaultQuarantined) and the lazy deploy is skipped until a real
            // (nonzero) sweep needs the vault.
            if (exactAmount == 0) {
                emit VaultQuarantined(vault, salt, 0);
                continue;
            }

            // Step 1 — deploy on demand, intentionally uncapped (pure CREATE2, no external reentry).
            if (vault.code.length == 0) {
                IVaultFactory(factory).deployProxy(salt);
            }

            // Step 2 — isolated per-vault sweep under the 120k gas cap. The
            // vault itself truncates returndata to <=32B at the token boundary.
            try IDepositVault(vault).sweep{gas: SWEEP_GAS_CAP}(token, targetColdWallet, exactAmount) {
                emit VaultSwept(vault, salt, exactAmount);
            } catch {
                emit VaultQuarantined(vault, salt, exactAmount);
            }
        }
    }

    /// @notice Step 1 of relayer rotation — propose under the 48h timelock.
    function proposeRelayerRotation(address newRelayer) external {
        require(msg.sender == timelock, "BatchSweeper: caller is not timelock");
        require(newRelayer != address(0), "BatchSweeper: zero relayer");
        uint64 executableAt = uint64(block.timestamp + ROTATION_TIMELOCK);
        pendingRelayerRotation = PendingRotation(newRelayer, executableAt);
        emit RelayerRotationProposed(newRelayer, executableAt);
    }

    /// @notice Step 2 — execute after the timelock has fully elapsed.
    /// @dev `initCodeHash` is unaffected (§2.1.3): it only binds `implementation`.
    function executeRelayerRotation() external {
        PendingRotation memory p = pendingRelayerRotation;
        require(p.candidate != address(0), "BatchSweeper: no pending relayer rotation");
        require(block.timestamp >= p.executableAt, "BatchSweeper: timelock not elapsed");
        relayerHotWallet = p.candidate;
        delete pendingRelayerRotation;
        emit RelayerRotationExecuted(p.candidate);
    }

    /// @notice Step 1 of cold wallet rotation — propose under the 48h timelock.
    /// @dev Mid-window both old+new wallets sit in the signer's allowed list (§2.1.3 SOP).
    function proposeColdWalletRotation(address newColdWallet) external {
        require(msg.sender == timelock, "BatchSweeper: caller is not timelock");
        require(newColdWallet != address(0), "BatchSweeper: zero cold wallet");
        uint64 executableAt = uint64(block.timestamp + ROTATION_TIMELOCK);
        pendingColdWalletRotation = PendingRotation(newColdWallet, executableAt);
        emit ColdWalletRotationProposed(newColdWallet, executableAt);
    }

    /// @notice Step 2 — execute after the timelock has fully elapsed.
    function executeColdWalletRotation() external {
        PendingRotation memory p = pendingColdWalletRotation;
        require(p.candidate != address(0), "BatchSweeper: no pending cold wallet rotation");
        require(block.timestamp >= p.executableAt, "BatchSweeper: timelock not elapsed");
        coldWallet = p.candidate;
        delete pendingColdWalletRotation;
        emit ColdWalletRotationExecuted(p.candidate);
    }

    /// @notice Rescue non-custody assets (native ETH via token==address(0), or
    ///         stray non-whitelisted ERC-20). Custody tokens are HARD-excluded in
    ///         code so governance can never drain user deposits (§2.2.3).
    /// @dev The low-level call is acceptable here: rescue paths are governance-
    ///      gated, out of the hot path, and non-custody tokens are not the
    ///      ReturnBomb threat model of the per-vault sweep boundary.
    function rescueAsset(address token, address to, uint256 amount) external {
        require(msg.sender == timelock, "BatchSweeper: caller is not timelock");
        require(to != address(0), "BatchSweeper: zero to");
        // Contract-layer hard exclusion — DO NOT weaken (§2.2.3).
        require(!isSupportedCustodyToken[token], "Core custody token forbidden");

        if (token == address(0)) {
            (bool ok,) = to.call{value: amount}("");
            require(ok, "BatchSweeper: ETH rescue failed");
        } else {
            // ERC-20 transfer(address,uint256) — tolerant of no-return tokens.
            (bool ok, bytes memory ret) =
                token.call(abi.encodeWithSelector(0xa9059cbb, to, amount));
            require(ok && (ret.length == 0 || abi.decode(ret, (bool))), "BatchSweeper: token rescue failed");
        }
        emit AssetRescued(token, to, amount);
    }

    /// @notice Register/deregister a custody stablecoin (guards rescueAsset).
    /// @dev Governance-only; called by the deploy script for USDT/USDC.
    function setSupportedCustodyToken(address token, bool supported) external {
        require(msg.sender == timelock, "BatchSweeper: caller is not timelock");
        isSupportedCustodyToken[token] = supported;
    }
}

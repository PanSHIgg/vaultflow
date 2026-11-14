// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {VaultFactory} from "../src/VaultFactory.sol";
import {EIP3009PayRouter} from "../src/EIP3009PayRouter.sol";
import {MockUSDC} from "../src/mocks/MockUSDC.sol";

/// @title EIP3009PayRouterTest
/// @notice W1-4: on-chain CREATE2 self-verified gasless USDC payments —
///         happy path, golden-vector authNonce, expiry, tamper and replay.
contract EIP3009PayRouterTest is Test {
    VaultFactory internal factory;
    EIP3009PayRouter internal router;
    MockUSDC internal usdc;

    address internal relayer;
    uint256 internal payerKey = 0xB0B;
    address internal payer;

    bytes32 internal constant RECEIVE_TYPEHASH = keccak256(
        "ReceiveWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"
    );

    /// @dev Golden-vector #1 order salt (chain_id_31337).
    bytes32 internal constant GOLDEN_SALT =
        0x55b28547100539cc5b92b67e6265adcc0b81e207dcb758222285a14b58e16834;
    bytes32 internal constant GOLDEN_NONCE =
        0x3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225;

    function setUp() public {
        relayer = makeAddr("relayer");
        payer = vm.addr(payerKey);
        usdc = new MockUSDC();
        factory = new VaultFactory(relayer, makeAddr("cold"), address(this));
        router = new EIP3009PayRouter(address(factory), address(usdc));
    }

    /// @dev Payer signs ReceiveWithAuthorization(to == router) bound to authNonce.
    function _sign(
        uint256 key,
        address to,
        uint256 amount,
        uint256 validAfter,
        uint256 validBefore,
        bytes32 nonce
    ) internal view returns (uint8 v, bytes32 r, bytes32 s) {
        bytes32 structHash = keccak256(
            abi.encode(RECEIVE_TYPEHASH, vm.addr(key), to, amount, validAfter, validBefore, nonce)
        );
        bytes32 digest =
            keccak256(abi.encodePacked("\x19\x01", usdc.DOMAIN_SEPARATOR(), structHash));
        (v, r, s) = vm.sign(key, digest);
    }

    function _authNonce(bytes32 salt) internal view returns (bytes32) {
        return keccak256(abi.encode(block.chainid, salt));
    }

    /// @notice Happy path on the GOLDEN salt: Foundry's default chainid 31337
    ///         makes _authNonce(GOLDEN_SALT) == GOLDEN_NONCE, proving the
    ///         on-chain check matches the frozen cross-stack formula.
    function test_PayOrderGoldenVector() public {
        assertEq(_authNonce(GOLDEN_SALT), GOLDEN_NONCE, "on-chain nonce != golden vector");

        uint256 amount = 1_500_000;
        uint256 validAfter = 0;
        uint256 validBefore = block.timestamp + 1 hours;
        usdc.mint(payer, amount);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(payerKey, address(router), amount, validAfter, validBefore, GOLDEN_NONCE);

        vm.prank(relayer);
        router.payOrderWithAuthorization(
            GOLDEN_SALT, payer, amount, validAfter, validBefore, GOLDEN_NONCE, v, r, s
        );

        address vault = factory.predictVaultAddress(GOLDEN_SALT);
        assertEq(usdc.balanceOf(vault), amount, "vault must hold the payment");
        assertEq(usdc.balanceOf(address(router)), 0, "router must not retain funds");
        assertEq(usdc.balanceOf(payer), 0);
    }

    /// @notice Tampered authNonce is rejected before any signature work —
    ///         the on-chain self-verification is the hijack firewall (§2.3).
    function test_TamperedAuthNonceRejected() public {
        bytes32 salt = keccak256("order-A");
        bytes32 wrongNonce = keccak256(abi.encode(block.chainid, keccak256("order-B")));
        vm.expectRevert("EIP3009PayRouter: authNonce mismatch");
        router.payOrderWithAuthorization(salt, payer, 1, 0, block.timestamp + 1, wrongNonce, 0, 0, 0);
    }

    /// @notice A nonce valid on another chain id is rejected on this chain.
    function test_CrossChainNonceRejected() public {
        bytes32 salt = keccak256("order-X");
        bytes32 foreignNonce = keccak256(abi.encode(uint256(1), salt)); // mainnet nonce on anvil chain
        vm.expectRevert("EIP3009PayRouter: authNonce mismatch");
        router.payOrderWithAuthorization(
            salt, payer, 1, 0, block.timestamp + 1, foreignNonce, 0, 0, 0
        );
    }

    /// @notice Signature over a different amount fails ecrecover inside USDC.
    function test_TamperedAmountRejected() public {
        bytes32 salt = keccak256("order-C");
        bytes32 nonce = _authNonce(salt);
        uint256 validBefore = block.timestamp + 1 hours;
        usdc.mint(payer, 200);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(payerKey, address(router), 100, 0, validBefore, nonce);
        vm.prank(relayer);
        vm.expectRevert("MockUSDC: invalid signature");
        router.payOrderWithAuthorization(salt, payer, 200, 0, validBefore, nonce, v, r, s);
    }

    /// @notice Signature by a different key than `payer` fails ecrecover.
    function test_WrongSignerRejected() public {
        bytes32 salt = keccak256("order-D");
        bytes32 nonce = _authNonce(salt);
        uint256 validBefore = block.timestamp + 1 hours;
        usdc.mint(payer, 100);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(0xD00D, address(router), 100, 0, validBefore, nonce); // attacker key
        vm.prank(relayer);
        vm.expectRevert("MockUSDC: invalid signature");
        router.payOrderWithAuthorization(salt, payer, 100, 0, validBefore, nonce, v, r, s);
    }

    /// @notice validBefore bound to order.expiresAt — after expiry the same
    ///         authorization is dead on-chain (§2.3.3 late-payment boundary).
    function test_ExpiredAuthorizationRejected() public {
        bytes32 salt = keccak256("order-E");
        bytes32 nonce = _authNonce(salt);
        uint256 validBefore = vm.getBlockTimestamp() + 1 hours;
        usdc.mint(payer, 100);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(payerKey, address(router), 100, 0, validBefore, nonce);
        vm.warp(validBefore); // now == validBefore -> expired (strict <)
        vm.prank(relayer);
        vm.expectRevert("MockUSDC: authorization expired");
        router.payOrderWithAuthorization(salt, payer, 100, 0, validBefore, nonce, v, r, s);
    }

    /// @notice Authorization is single-use — replay reverts on the second call.
    function test_ReplayRejected() public {
        bytes32 salt = keccak256("order-F");
        bytes32 nonce = _authNonce(salt);
        uint256 validBefore = block.timestamp + 1 hours;
        usdc.mint(payer, 200);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(payerKey, address(router), 100, 0, validBefore, nonce);
        vm.prank(relayer);
        router.payOrderWithAuthorization(salt, payer, 100, 0, validBefore, nonce, v, r, s);
        vm.prank(relayer);
        vm.expectRevert("MockUSDC: authorization already used");
        router.payOrderWithAuthorization(salt, payer, 100, 0, validBefore, nonce, v, r, s);
    }

    /// @notice Authorization signed `to` someone other than the router cannot
    ///         be used through it (the signed `to` binds the pull target).
    function test_SignedToOtherRejected() public {
        bytes32 salt = keccak256("order-G");
        bytes32 nonce = _authNonce(salt);
        uint256 validBefore = block.timestamp + 1 hours;
        usdc.mint(payer, 100);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(payerKey, makeAddr("attacker"), 100, 0, validBefore, nonce);
        vm.prank(relayer);
        vm.expectRevert("MockUSDC: invalid signature");
        router.payOrderWithAuthorization(salt, payer, 100, 0, validBefore, nonce, v, r, s);
    }

    /// @notice Anyone may relay — funds still land in the CREATE2-derived vault.
    function test_AnyoneCanRelaySafely() public {
        bytes32 salt = keccak256("order-H");
        bytes32 nonce = _authNonce(salt);
        uint256 validBefore = block.timestamp + 1 hours;
        usdc.mint(payer, 50);
        (uint8 v, bytes32 r, bytes32 s) =
            _sign(payerKey, address(router), 50, 0, validBefore, nonce);
        vm.prank(makeAddr("randomRelayer"));
        router.payOrderWithAuthorization(salt, payer, 50, 0, validBefore, nonce, v, r, s);
        assertEq(usdc.balanceOf(factory.predictVaultAddress(salt)), 50);
    }
}

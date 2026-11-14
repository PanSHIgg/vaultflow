// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";

/// @title GoldenVectorsTest
/// @notice Byte-level alignment proof against docs/GOLDEN_VECTORS.json — the
///         cross-stack arbitration baseline (AGENTS.md §2.1). Every formula is
///         recomputed in Solidity from the JSON inputs and asserted equal to
///         the JSON expected outputs.
contract GoldenVectorsTest is Test {
    string internal json;

    // Canonical addresses from GOLDEN_VECTORS.canonical_addresses.
    address internal constant IMPL = 0x1111111111111111111111111111111111111111;
    address internal constant FACTORY = 0x2222222222222222222222222222222222222222;

    bytes internal constant PROXY_PREFIX = hex"3d602d80600a3d3981f3363d3d373d3d3d363d73";
    bytes internal constant PROXY_SUFFIX = hex"5af43d82803e903d91602b57fd5bf3";

    function setUp() public {
        json = vm.readFile("../docs/GOLDEN_VECTORS.json");
    }

    /// @dev Reads a bytes16 hex value (e.g. "0x0011...eeff") from the JSON.
    function _jsonBytes16(string memory key) internal view returns (bytes16 out_) {
        bytes memory raw = vm.parseJsonBytes(json, key);
        require(raw.length == 16, "bad bytes16 vector input");
        assembly {
            out_ := mload(add(raw, 32))
        }
    }

    /// @dev Full formula chain for vectors[i]: salt -> authNonce, then
    ///      initCodeHash -> vaultAddress, all asserted byte-exact.
    function _checkVector(uint256 i) internal {
        bytes32 salt = _checkSaltAndNonce(i);
        _checkInitCodeAndVault(i, salt);
    }

    function _checkSaltAndNonce(uint256 i) internal returns (bytes32 salt) {
        string memory idx = vm.toString(i);
        uint256 chainId =
            vm.parseJsonUint(json, string.concat("$.vectors[", idx, "].chain_id"));
        bytes16 merchantId =
            _jsonBytes16(string.concat("$.vectors[", idx, "].merchant_id"));
        bytes16 orderId =
            _jsonBytes16(string.concat("$.vectors[", idx, "].order_id"));

        // salt = keccak256(abi.encode(uint256(chainId), bytes16, bytes16))
        // abi.encode right-pads bytes16 to 32B -> the frozen 96B blob.
        salt = keccak256(abi.encode(chainId, merchantId, orderId));
        assertEq(
            salt,
            vm.parseJsonBytes32(json, string.concat("$.vectors[", idx, "].salt")),
            "salt mismatch"
        );

        // authNonce = keccak256(abi.encode(uint256(chainId), bytes32(salt)))
        bytes32 authNonce = keccak256(abi.encode(chainId, salt));
        assertEq(
            authNonce,
            vm.parseJsonBytes32(json, string.concat("$.vectors[", idx, "].auth_nonce")),
            "authNonce mismatch"
        );
    }

    function _checkInitCodeAndVault(uint256 i, bytes32 salt) internal {
        string memory idx = vm.toString(i);

        // init_code = 55B EIP-1167 template (prefix 20B || impl 20B || suffix 15B)
        bytes memory initCode = abi.encodePacked(PROXY_PREFIX, IMPL, PROXY_SUFFIX);
        assertEq(initCode.length, 55, "init code length");
        assertEq(
            initCode,
            vm.parseJsonBytes(json, string.concat("$.vectors[", idx, "].init_code")),
            "init code bytes"
        );

        bytes32 initCodeHash = keccak256(initCode);
        assertEq(
            initCodeHash,
            vm.parseJsonBytes32(json, string.concat("$.vectors[", idx, "].init_code_hash")),
            "initCodeHash mismatch"
        );

        // vault = last20(keccak256(0xff || factory || salt || initCodeHash))
        address vault = address(
            uint160(
                uint256(
                    keccak256(abi.encodePacked(bytes1(0xff), FACTORY, salt, initCodeHash))
                )
            )
        );
        assertEq(
            vault,
            vm.parseJsonAddress(json, string.concat("$.vectors[", idx, "].vault_address")),
            "vaultAddress mismatch"
        );
    }

    function test_GoldenVector_chainId31337() public {
        _checkVector(0);
    }

    function test_GoldenVector_chainId1() public {
        _checkVector(1);
    }

    /// @notice Guard rail: abi.encodePacked (compact, no right-padding) must NOT
    ///         reproduce the golden salt — documents the bytes16 right-padding
    ///         requirement that divergent stacks get wrong.
    function test_SaltRequiresAbiEncodeNotPacked() public {
        uint256 chainId = 31337;
        bytes16 merchantId = bytes16(0x00112233445566778899aabbccddeeff);
        bytes16 orderId = bytes16(0xffeeddccbbaa99887766554433221100);
        bytes32 packedSalt = keccak256(abi.encodePacked(chainId, merchantId, orderId));
        bytes32 expectedSalt =
            vm.parseJsonBytes32(json, "$.vectors[0].salt");
        assertTrue(packedSalt != expectedSalt, "packed encoding must diverge");
    }

    /// @notice The canonical init-code hash is independent of salt/chain —
    ///         sanity for the "rotation never changes initCodeHash" invariant.
    function test_InitCodeHashInvariantAcrossVectors() public {
        bytes32 h0 =
            vm.parseJsonBytes32(json, "$.vectors[0].init_code_hash");
        bytes32 h1 =
            vm.parseJsonBytes32(json, "$.vectors[1].init_code_hash");
        assertEq(h0, h1, "initCodeHash must be vector-independent");
    }
}

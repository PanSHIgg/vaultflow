// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title MockUSDC
/// @notice FiatTokenV2-shaped mock with an EIP-3009 surface for router tests (W1-5).
/// @dev TODO(W1): replace the naive receiveWithAuthorization with real EIP-712
///      domain separation + ecrecover + authorizationState[nonce] single-use +
///      validAfter/validBefore window checks.
contract MockUSDC {
    string public constant name = "Mock USDC";
    string public constant symbol = "USDC";
    uint8 public constant decimals = 6;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    /// @dev TODO(W1): mark used EIP-3009 nonces here once real verification lands.
    mapping(address => mapping(bytes32 => bool)) public authorizationState;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);
    event AuthorizationUsed(address indexed authorizer, bytes32 indexed nonce);

    /// @notice Test helper — unrestricted mint (mock only).
    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    function transfer(address to, uint256 amount) external returns (bool) {
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
        emit Transfer(msg.sender, to, amount);
        return true;
    }

    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        emit Approval(msg.sender, spender, amount);
        return true;
    }

    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        allowance[from][msg.sender] -= amount;
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
        emit Transfer(from, to, amount);
        return true;
    }

    /// @notice EIP-3009 entry used by EIP3009PayRouter.
    /// @dev STUB semantics: moves funds without signature/nonce checks so the
    ///      router skeleton is exercisable end-to-end. TODO(W1): full EIP-712
    ///      verification — ecrecover must equal `from`, !authorizationState[from][nonce],
    ///      block.timestamp within [validAfter, validBefore].
    function receiveWithAuthorization(
        address from,
        address to,
        uint256 value,
        uint256 /*validAfter*/,
        uint256 /*validBefore*/,
        bytes32 /*nonce*/,
        uint8 /*v*/,
        bytes32 /*r*/,
        bytes32 /*s*/
    ) external {
        balanceOf[from] -= value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}

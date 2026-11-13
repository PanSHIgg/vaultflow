// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title MockUSDT
/// @notice Mainnet-Tether-shaped mock for adversarial tests (W1-5).
/// @dev Two special knobs:
///      1. `transfer` returns NO bool (legacy USDT signature) — exercises the
///         vault's 32B-truncated/no-returndata success path;
///      2. `isBlackListed` — frozen addresses must revert sweeps so BatchSweeper
///         marks them quarantined.
contract MockUSDT {
    string public constant name = "Mock USDT";
    string public constant symbol = "USDT";
    uint8 public constant decimals = 6;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    /// @dev Tether-style freeze list — sweep attempts on listed addrs must revert.
    mapping(address => bool) public isBlackListed;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    /// @notice Test helper — unrestricted mint (mock only).
    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    /// @notice Test helper — toggle Tether-style blacklist (W1-5 scenarios).
    function setBlackListed(address who, bool listed) external {
        isBlackListed[who] = listed;
    }

    /// @dev USDT shape: NO return value, reverts on blacklisted parties.
    function transfer(address to, uint256 amount) external {
        require(!isBlackListed[msg.sender] && !isBlackListed[to], "MockUSDT: blacklisted");
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
        emit Transfer(msg.sender, to, amount);
    }

    /// @dev TODO(W1): approve/transferFrom fidelity if tests need allowance flows.
    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        emit Approval(msg.sender, spender, amount);
        return true;
    }

    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        require(!isBlackListed[from] && !isBlackListed[to], "MockUSDT: blacklisted");
        allowance[from][msg.sender] -= amount;
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
        emit Transfer(from, to, amount);
        return true;
    }
}

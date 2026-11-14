// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title MockReturnBombToken
/// @notice Adversarial token whose `transfer` returns a ~2 MiB returndata blob.
/// @dev Exercises the DepositVault inner-boundary defense: the vault's inline-asm
///      token.call must read AT MOST 32 bytes of returndata, so an honest sweep
///      survives this token while a naive `returndatacopy`-to-memory design gets
///      memory-expansion-bombed inside BatchSweeper's 120k gas cap (§2.2.2, W1-5).
contract MockReturnBombToken {
    string public constant name = "ReturnBomb";
    string public constant symbol = "BOMB";
    uint8 public constant decimals = 6;

    mapping(address => uint256) public balanceOf;

    event Transfer(address indexed from, address indexed to, uint256 value);

    /// @notice Test helper — unrestricted mint (mock only).
    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    /// @dev Always "succeeds" but returns a 2 MiB blob — the memory bomb.
    ///      TODO(W1): tune payload size if the harness needs a different volume.
    ///      NOTE: real balance moves intentionally skipped; mock is pure poison.
    function transfer(address /*to*/, uint256 /*amount*/) external pure returns (bool) {
        assembly {
            mstore(0x00, 0x01)
            return(0x00, 0x200000) // 2 MiB returndata bomb
        }
    }
}

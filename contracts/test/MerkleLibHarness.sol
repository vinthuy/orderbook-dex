// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "../libraries/MerkleLib.sol";

contract MerkleLibHarness {
    function balanceLeaf(address account, uint32 assetId, uint256 balance)
        external
        pure
        returns (bytes32)
    {
        return MerkleLib.balanceLeaf(account, assetId, balance);
    }

    function computeRoot(bytes32[] memory leaves) external pure returns (bytes32) {
        return MerkleLib.computeRoot(leaves);
    }

    function verify(bytes32[] memory proof, bytes32 root, bytes32 leaf)
        external
        pure
        returns (bool)
    {
        return MerkleLib.verify(proof, root, leaf);
    }
}

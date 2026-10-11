// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/// @notice Merkle helpers shared by the settlement contract and the off-chain ledger.
///         Leaf = keccak256(bytes20(account) || bytes32(uint256(assetId)) || bytes32(balance)).
library MerkleLib {
    function balanceLeaf(address account, uint32 assetId, uint256 balance)
        internal
        pure
        returns (bytes32)
    {
        return keccak256(
            abi.encodePacked(bytes20(account), bytes32(uint256(assetId)), bytes32(balance))
        );
    }

    function hashPair(bytes32 a, bytes32 b) internal pure returns (bytes32) {
        return a <= b
            ? keccak256(abi.encodePacked(a, b))
            : keccak256(abi.encodePacked(b, a));
    }

    function computeRoot(bytes32[] memory leaves) internal pure returns (bytes32) {
        if (leaves.length == 0) return bytes32(0);
        bytes32[] memory level = leaves;
        while (level.length > 1) {
            uint256 nextLen = (level.length + 1) / 2;
            bytes32[] memory next = new bytes32[](nextLen);
            for (uint256 i = 0; i < level.length; i += 2) {
                if (i + 1 == level.length) {
                    next[i / 2] = level[i]; // promote odd node unchanged
                } else {
                    next[i / 2] = hashPair(level[i], level[i + 1]);
                }
            }
            level = next;
        }
        return level[0];
    }

    function verify(bytes32[] memory proof, bytes32 root, bytes32 leaf)
        internal
        pure
        returns (bool)
    {
        bytes32 computed = leaf;
        for (uint256 i = 0; i < proof.length; i++) {
            computed = hashPair(computed, proof[i]);
        }
        return computed == root;
    }
}

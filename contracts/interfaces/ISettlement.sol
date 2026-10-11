// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

interface ISettlement {
    event AssetRegistered(uint32 indexed assetId, address indexed token, uint8 decimals);
    event Deposit(address indexed account, uint32 indexed assetId, uint256 amount, uint256 depositSeq);
    event StateRootCommitted(uint256 indexed rootIndex, bytes32 root, uint64 timestamp);
    event OperatorChanged(address indexed oldOperator, address indexed newOperator);
    event Paused(address indexed by);
    event Unpaused(address indexed by);

    function registerAsset(address token) external returns (uint32 assetId);
    function setOperator(address newOperator) external;
    function setExitDelay(uint64 newDelay) external;
    function pause() external;
    function unpause() external;
    function deposit(uint32 assetId, uint256 amount) external;
    function commitStateRoot(bytes32 root) external returns (uint256 rootIndex);

    function latestRootIndex() external view returns (uint256);
    function rootAt(uint256 rootIndex) external view returns (bytes32 root, uint64 timestamp);
    function balanceLeaf(address account, uint32 assetId, uint256 balance)
        external
        pure
        returns (bytes32);
}

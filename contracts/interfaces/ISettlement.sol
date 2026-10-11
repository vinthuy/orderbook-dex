// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

interface ISettlement {
    event AssetRegistered(uint32 indexed assetId, address indexed token, uint8 decimals);
    event Deposit(address indexed account, uint32 indexed assetId, uint256 amount, uint256 depositSeq);
    event StateRootCommitted(uint256 indexed rootIndex, bytes32 root, uint64 timestamp);
    event Withdrawal(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex);
    event OperatorChanged(address indexed oldOperator, address indexed newOperator);
    event Paused(address indexed by);
    event Unpaused(address indexed by);
    event ExitRequested(address indexed account, uint32 indexed assetId, uint256 rootIndex, uint64 timestamp);
    event ExitCancelled(address indexed account, uint32 indexed assetId);
    event ExitFinalized(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex);

    function registerAsset(address token) external returns (uint32 assetId);
    function setOperator(address newOperator) external;
    function setExitDelay(uint64 newDelay) external;
    function requestExit(uint32 assetId) external;
    function cancelExit(uint32 assetId) external;
    function finalizeExit(uint32 assetId, uint256 rootIndex, uint256 balance, bytes32[] calldata proof) external;
    function pause() external;
    function unpause() external;
    function deposit(uint32 assetId, uint256 amount) external;
    function commitStateRoot(bytes32 root) external returns (uint256 rootIndex);
    function withdraw(
        address account,
        uint32 assetId,
        uint256 balance,
        uint256 amount,
        uint256 rootIndex,
        bytes32[] calldata proof
    ) external;

    function latestRootIndex() external view returns (uint256);
    function rootAt(uint256 rootIndex) external view returns (bytes32 root, uint64 timestamp);
    function balanceLeaf(address account, uint32 assetId, uint256 balance)
        external
        pure
        returns (bytes32);
}

// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "./interfaces/ISettlement.sol";
import "./interfaces/IERC20.sol";
import "./libraries/MerkleLib.sol";

contract Settlement is ISettlement {
    struct RootInfo {
        bytes32 root;
        uint64 timestamp;
    }

    struct ExitRequest {
        uint256 rootIndex;
        uint64 timestamp;
    }

    address public owner;
    address public operator;
    bool public paused;
    uint64 public exitDelay;

    uint32 public assetCount;
    mapping(uint32 => address) public assets;
    mapping(address => uint32) public assetIds;

    mapping(uint256 => RootInfo) private _roots;
    uint256 public latestRootIndex;

    mapping(uint32 => uint256) public totalDeposited;
    mapping(uint32 => uint256) public totalWithdrawn;
    mapping(address => uint256) public depositSeq;

    mapping(address => mapping(uint32 => uint256)) public lastWithdrawRoot;
    mapping(address => mapping(uint32 => mapping(uint256 => uint256))) public withdrawnAtRoot;
    mapping(address => mapping(uint32 => ExitRequest)) public exitRequests;
    mapping(address => mapping(uint32 => bool)) public exited;

    modifier onlyOwner() {
        require(msg.sender == owner, "not owner");
        _;
    }

    modifier onlyOperator() {
        require(msg.sender == operator, "not operator");
        _;
    }

    constructor(address _operator, uint64 _exitDelay) {
        require(_operator != address(0), "zero operator");
        owner = msg.sender;
        operator = _operator;
        exitDelay = _exitDelay;
    }

    function registerAsset(address token) external override onlyOwner returns (uint32 assetId) {
        require(token != address(0), "zero token");
        require(assetIds[token] == 0, "already registered");
        assetCount += 1;
        assetId = assetCount;
        assets[assetId] = token;
        assetIds[token] = assetId;
        emit AssetRegistered(assetId, token, IERC20(token).decimals());
    }

    function setOperator(address newOperator) external override onlyOwner {
        require(newOperator != address(0), "zero operator");
        emit OperatorChanged(operator, newOperator);
        operator = newOperator;
    }

    function setExitDelay(uint64 newDelay) external override onlyOwner {
        exitDelay = newDelay;
    }

    function pause() external override onlyOwner {
        paused = true;
        emit Paused(msg.sender);
    }

    function unpause() external override onlyOwner {
        paused = false;
        emit Unpaused(msg.sender);
    }

    function deposit(uint32 assetId, uint256 amount) external override {
        require(!paused, "paused");
        address token = assets[assetId];
        require(token != address(0), "unknown asset");
        require(amount > 0, "zero amount");
        require(exitRequests[msg.sender][assetId].rootIndex == 0, "exit pending");
        if (exited[msg.sender][assetId]) {
            exited[msg.sender][assetId] = false;
        }
        require(IERC20(token).transferFrom(msg.sender, address(this), amount), "transferFrom failed");
        totalDeposited[assetId] += amount;
        uint256 seq = ++depositSeq[msg.sender];
        emit Deposit(msg.sender, assetId, amount, seq);
    }

    function commitStateRoot(bytes32 root) external override onlyOperator returns (uint256 rootIndex) {
        for (uint32 i = 1; i <= assetCount; i++) {
            require(
                IERC20(assets[i]).balanceOf(address(this)) >= totalDeposited[i] - totalWithdrawn[i],
                "insolvent"
            );
        }
        latestRootIndex += 1;
        rootIndex = latestRootIndex;
        _roots[rootIndex] = RootInfo(root, uint64(block.timestamp));
        emit StateRootCommitted(rootIndex, root, uint64(block.timestamp));
    }

    function withdraw(
        address account,
        uint32 assetId,
        uint256 balance,
        uint256 amount,
        uint256 rootIndex,
        bytes32[] calldata proof
    ) external override onlyOperator {
        require(!paused, "paused");
        require(!exited[account][assetId], "exited");
        require(exitRequests[account][assetId].rootIndex == 0, "exit pending");
        require(rootIndex >= 1 && rootIndex <= latestRootIndex, "bad root");
        require(rootIndex >= lastWithdrawRoot[account][assetId], "stale root");
        require(amount > 0 && amount <= balance, "bad amount");

        bytes32 leaf = MerkleLib.balanceLeaf(account, assetId, balance);
        require(MerkleLib.verify(proof, _roots[rootIndex].root, leaf), "bad proof");

        uint256 already = withdrawnAtRoot[account][assetId][rootIndex];
        require(already + amount <= balance, "exceeds balance");
        withdrawnAtRoot[account][assetId][rootIndex] = already + amount;
        lastWithdrawRoot[account][assetId] = rootIndex;

        totalWithdrawn[assetId] += amount;
        require(IERC20(assets[assetId]).transfer(account, amount), "transfer failed");
        emit Withdrawal(account, assetId, amount, rootIndex);
    }

    function requestExit(uint32 assetId) external override {
        require(assets[assetId] != address(0), "unknown asset");
        require(!exited[msg.sender][assetId], "exited");
        require(latestRootIndex >= 1, "no root");
        if (exitRequests[msg.sender][assetId].rootIndex == 0) {
            exitRequests[msg.sender][assetId] = ExitRequest(latestRootIndex, uint64(block.timestamp));
            emit ExitRequested(msg.sender, assetId, latestRootIndex, uint64(block.timestamp));
        }
    }

    function cancelExit(uint32 assetId) external override {
        require(exitRequests[msg.sender][assetId].rootIndex != 0, "no exit");
        delete exitRequests[msg.sender][assetId];
        emit ExitCancelled(msg.sender, assetId);
    }

    function finalizeExit(uint32 assetId, uint256 rootIndex, uint256 balance, bytes32[] calldata proof)
        external
        override
    {
        ExitRequest memory req = exitRequests[msg.sender][assetId];
        require(req.rootIndex != 0, "no exit");
        require(rootIndex == req.rootIndex, "wrong root");
        require(block.timestamp >= uint256(req.timestamp) + uint256(exitDelay), "too early");
        require(balance > 0, "zero balance");

        bytes32 leaf = MerkleLib.balanceLeaf(msg.sender, assetId, balance);
        require(MerkleLib.verify(proof, _roots[rootIndex].root, leaf), "bad proof");

        delete exitRequests[msg.sender][assetId];
        exited[msg.sender][assetId] = true;
        totalWithdrawn[assetId] += balance;
        require(IERC20(assets[assetId]).transfer(msg.sender, balance), "transfer failed");
        // Close off the frozen root so it cannot be replayed after `deposit` clears `exited`.
        lastWithdrawRoot[msg.sender][assetId] = req.rootIndex;
        withdrawnAtRoot[msg.sender][assetId][req.rootIndex] += balance;
        emit ExitFinalized(msg.sender, assetId, balance, rootIndex);
    }

    function rootAt(uint256 rootIndex) external view override returns (bytes32 root, uint64 timestamp) {
        RootInfo memory r = _roots[rootIndex];
        return (r.root, r.timestamp);
    }

    function balanceLeaf(address account, uint32 assetId, uint256 balance)
        external
        pure
        override
        returns (bytes32)
    {
        return MerkleLib.balanceLeaf(account, assetId, balance);
    }
}

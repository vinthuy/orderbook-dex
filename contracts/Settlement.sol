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
    mapping(address => mapping(uint32 => uint256)) public withdrawnAtRoot;
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

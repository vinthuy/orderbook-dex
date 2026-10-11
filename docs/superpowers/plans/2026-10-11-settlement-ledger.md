# 子项目 1：结算合约 + 链下账本 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现可验证的链上托管金库（充值/状态根锚定/快速提现/强制退出）与链下余额账本（确定性 Merkle 根 + WAL 持久化 + 幂等），构成去中心化订单簿 DEX 的地基。

**Architecture:** 用户 ERC20 充值进 `Settlement.sol` 金库；Go 端 `ledger` 包维护内部余额并导出 Merkle 状态根；`StateAnchorService` 周期锚定根；`DepositIndexer` 幂等入账；用户可经 Merkle 证明快速提现，或在延迟后用 `requestExit/finalizeExit` 无需许可地强制退出。链上链下共享同一套"叶子编码 + 排序对建树"规则。

**Tech Stack:** Solidity 0.8.19（Hardhat + Chai）、Go 1.25（module `web3-amm-dex-backend`）、`github.com/ethereum/go-ethereum`、`github.com/holiman/uint256`。

**Spec:** `docs/superpowers/specs/2026-10-11-orderbook-dex-settlement-ledger-design.md`

## Global Constraints

- Solidity 版本固定 `0.8.19`，沿用现有 `hardhat.config.js`（optimizer enabled, runs 200）。不引入新 Solidity 依赖（不装 OpenZeppelin），合约手写以贴合现有仓库风格。
- Go module 名固定 `web3-amm-dex-backend`（见 `backend/go.mod`）。Go 代码使用 `github.com/holiman/uint256` 表示金额（整数、无浮点）；JSON 中金额一律用十进制字符串。
- `AssetID` = `uint32`；`AccountID` = `[20]byte`（v1 即地址）。
- 叶子编码（链上链下必须一字不差）：
  `leaf = keccak256(bytes20(account) ‖ bytes32(uint256(assetId)) ‖ bytes32(balance))`，共 84 字节。
- 建树规则：叶子按 `(account, assetId)` 升序排列；内部节点 `= keccak256(min(a,b) ‖ max(a,b))`；层级为奇数时末尾节点**原样提升**（不复制）；空树根 = `bytes32(0)`。
- 测试位置：Go 用同包 `_test.go`；Solidity 用 `test/*.test.js`（Hardhat + Chai，`@nomicfoundation/hardhat-toolbox`）。
- 命令在 Windows PowerShell 下执行：`&&` 不可用，用 `;` 分隔或分多条执行。
- 提交信息前缀：`feat:` / `test:` / `docs:` / `chore:`。

## File Structure

**新建 / 修改：**

```
contracts/
  libraries/MerkleLib.sol          # 叶子编码 + 建树 + 验证（internal 库）
  interfaces/IERC20.sol            # 最小 ERC20 接口
  interfaces/ISettlement.sol       # 事件 + 对外函数签名（分任务增量扩）
  Settlement.sol                   # 金库合约
  test/MerkleLibHarness.sol        # 暴露 MerkleLib internal 函数的测试壳
  test/MockToken.sol               # 可制造"充值不入账"的 mock 代币（测偿付校验）
test/
  MerkleLib.test.js                # Task 1
  Settlement.core.test.js          # Task 2
  Settlement.withdraw.test.js      # Task 3
  Settlement.exit.test.js          # Task 4
  MerkleVectors.test.js            # Task 12（读 Go fixture，跨语言一致性）
backend/
  ledger/ledger.go                 # 账本状态、Apply、快照序列化
  ledger/op.go                     # Op 接口 + Deposit/Withdraw/Transfer + OpRecord 转换
  ledger/merkle.go                 # 叶子/建树/证明 + Ledger.Root/Proof
  ledger/store.go                  # Store 接口 + FileStore + Recover
  ledger/ledger_test.go
  ledger/merkle_test.go
  ledger/store_test.go
  ledger/testdata/merkle_vectors.json   # Task 6 生成，Task 12 消费
  services/blockchain_adapter.go   # 加 GetLogs；mock 加 Logs/Sent
  services/blockchain_adapter_test.go
  services/evm_adapter.go          # 真实 ethclient 实现
  services/deposit_indexer.go
  services/deposit_indexer_test.go
  services/settlement.go
  services/settlement_test.go
  services/state_anchor.go
  services/state_anchor_test.go
  config/config.go                 # 加 SettlementConfig + 加载函数
  config/defaults.go
  config/config_test.go
```

---

## 共享接口契约（后续任务引用，先在此固定）

```go
// package ledger (backend/ledger)

type AssetID uint32
type AccountID [20]byte

func AccountFromHex(s string) (AccountID, error)
func (a AccountID) Hex() string

type Op interface {
    Key() string        // 幂等键（充值: "txHash:logIndex"；成交: 确定性成交 ID）
    walRecord() OpRecord
    apply(t *tx) error
}

type DepositOp  struct{ Account AccountID; Asset AssetID; Amount *uint256.Int; Ref string }
type WithdrawOp struct{ Account AccountID; Asset AssetID; Amount *uint256.Int; Ref string }
type TransferOp struct{ From, To AccountID; Asset AssetID; Amount *uint256.Int; Ref string }

type Ledger struct { /* unexported */ }
func New() *Ledger
func (l *Ledger) WithStore(s Store) *Ledger
func (l *Ledger) Apply(ops ...Op) error
func (l *Ledger) Balance(a AccountID, asset AssetID) *uint256.Int
func (l *Ledger) TotalBalance(asset AssetID) *uint256.Int
func (l *Ledger) Version() uint64
func (l *Ledger) Root() [32]byte
func (l *Ledger) Proof(a AccountID, asset AssetID) (proof [][32]byte, balance *uint256.Int, err error)
func (l *Ledger) SnapshotBytes() ([]byte, error)
func (l *Ledger) Snap() error

type WALEntry struct{ Seq uint64 `json:"seq"`; Op OpRecord `json:"op"` }
type OpRecord struct {
    Type    string `json:"type"`               // "deposit" | "withdraw" | "transfer"
    Account string `json:"account,omitempty"`
    From    string `json:"from,omitempty"`
    To      string `json:"to,omitempty"`
    Asset   uint32 `json:"asset"`
    Amount  string `json:"amount"`             // decimal string
    Ref     string `json:"ref"`
}
func (r OpRecord) ToOp() (Op, error)

type Store interface {
    AppendBatch(entries []WALEntry) error
    SaveSnapshot(state []byte) error
    LoadSnapshot() ([]byte, error)          // 无快照返回 nil, nil
    Replay(fromSeq uint64) ([]WALEntry, error)
    Close() error
}
func NewFileStore(dir string) (*FileStore, error)
func Recover(store Store) (*Ledger, error)
```

```go
// package services (backend/services)

// BlockchainAdapter 新增：
GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error)

// 新增 Config 字段（services.Config）：
//   SettlementAddress   string
//   DataDir             string
//   RequiredConfirmations uint64
//   AnchorInterval      int64 // 秒
//   ChainID             int64
```

---

### Task 1: Solidity Merkle 库

**Files:**
- Create: `contracts/libraries/MerkleLib.sol`
- Create: `contracts/test/MerkleLibHarness.sol`
- Test: `test/MerkleLib.test.js`

**Interfaces:**
- Produces:
  - `MerkleLib.balanceLeaf(address account, uint32 assetId, uint256 balance) → bytes32`
  - `MerkleLib.hashPair(bytes32 a, bytes32 b) → bytes32`
  - `MerkleLib.computeRoot(bytes32[] leaves) → bytes32`
  - `MerkleLib.verify(bytes32[] proof, bytes32 root, bytes32 leaf) → bool`

- [ ] **Step 1: Write the failing test**

`test/MerkleLib.test.js`:

```javascript
const { expect } = require("chai");
const { ethers } = require("hardhat");

const hashPair = (a, b) => {
  const [x, y] = BigInt(a) <= BigInt(b) ? [a, b] : [b, a];
  return ethers.solidityPackedKeccak256(["bytes32", "bytes32"], [x, y]);
};

const buildRoot = (leaves) => {
  if (leaves.length === 0) return ethers.ZeroHash;
  let level = leaves.slice();
  while (level.length > 1) {
    const next = [];
    for (let i = 0; i < level.length; i += 2) {
      if (i + 1 === level.length) next.push(level[i]);
      else next.push(hashPair(level[i], level[i + 1]));
    }
    level = next;
  }
  return level[0];
};

const buildProof = (leaves, index) => {
  const proof = [];
  let level = leaves.slice();
  let idx = index;
  while (level.length > 1) {
    if (idx % 2 === 0) {
      if (idx + 1 < level.length) proof.push(level[idx + 1]);
    } else {
      proof.push(level[idx - 1]);
    }
    const next = [];
    for (let i = 0; i < level.length; i += 2) {
      if (i + 1 === level.length) next.push(level[i]);
      else next.push(hashPair(level[i], level[i + 1]));
    }
    level = next;
    idx = Math.floor(idx / 2);
  }
  return proof;
};

describe("MerkleLib", function () {
  let harness;
  const account = "0x1111111111111111111111111111111111111111";

  before(async function () {
    harness = await (await ethers.getContractFactory("MerkleLibHarness")).deploy();
  });

  it("balanceLeaf matches packed encoding", async function () {
    const expected = ethers.solidityPackedKeccak256(
      ["address", "uint256", "uint256"],
      [account, 1, 1000n]
    );
    expect(await harness.balanceLeaf(account, 1, 1000n)).to.equal(expected);
  });

  it("single-leaf root equals the leaf", async function () {
    const leaf = await harness.balanceLeaf(account, 1, 100);
    expect(await harness.computeRoot([leaf])).to.equal(leaf);
  });

  it("empty set root is zero", async function () {
    expect(await harness.computeRoot([])).to.equal(ethers.ZeroHash);
  });

  it("3-leaf root matches the JS reference (odd node promoted)", async function () {
    const leaves = [
      await harness.balanceLeaf(account, 1, 100),
      await harness.balanceLeaf(account, 2, 200),
      await harness.balanceLeaf(account, 3, 300),
    ];
    expect(await harness.computeRoot(leaves)).to.equal(buildRoot(leaves));
  });

  it("verifies each valid proof and rejects a wrong leaf", async function () {
    const leaves = [
      await harness.balanceLeaf(account, 1, 100),
      await harness.balanceLeaf(account, 2, 200),
      await harness.balanceLeaf(account, 3, 300),
    ];
    const root = buildRoot(leaves);
    for (let i = 0; i < leaves.length; i++) {
      expect(await harness.verify(buildProof(leaves, i), root, leaves[i])).to.equal(true);
    }
    const wrong = await harness.balanceLeaf(account, 9, 999);
    expect(await harness.verify(buildProof(leaves, 0), root, wrong)).to.equal(false);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npx hardhat test test/MerkleLib.test.js`
Expected: FAIL — `HardhatError: HH700: Artifact for contract "MerkleLibHarness" not found`.

- [ ] **Step 3: Write the library and harness**

`contracts/libraries/MerkleLib.sol`:

```solidity
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
```

`contracts/test/MerkleLibHarness.sol`:

```solidity
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `npx hardhat test test/MerkleLib.test.js`
Expected: PASS (5 passing).

- [ ] **Step 5: Commit**

```bash
git add contracts/libraries/MerkleLib.sol contracts/test/MerkleLibHarness.sol test/MerkleLib.test.js
git commit -m "feat(contracts): add MerkleLib with leaf encoding and sorted-pair tree"
```

---

### Task 2: Settlement 合约核心（资产、充值、锚定、暂停）

**Files:**
- Create: `contracts/interfaces/IERC20.sol`
- Create: `contracts/interfaces/ISettlement.sol`
- Create: `contracts/Settlement.sol`
- Create: `contracts/test/MockToken.sol`
- Test: `test/Settlement.core.test.js`

**Interfaces:**
- Consumes: `MerkleLib.balanceLeaf`（Task 1）。
- Produces（`Settlement` 公开 API，后续任务扩展）：
  - `constructor(address operator, uint64 exitDelay)`
  - `registerAsset(address token) → uint32` / `setOperator(address)` / `setExitDelay(uint64)` / `pause()` / `unpause()`
  - `deposit(uint32 assetId, uint256 amount)`
  - `commitStateRoot(bytes32 root) → uint256 rootIndex`
  - views：`latestRootIndex() → uint256`、`rootAt(uint256) → (bytes32, uint64)`、`balanceLeaf(address, uint32, uint256) → bytes32`
  - public 状态：`owner`、`operator`、`paused`、`exitDelay`、`assetCount`、`assets(uint32)`、`assetIds(address)`、`totalDeposited(uint32)`、`totalWithdrawn(uint32)`、`depositSeq(address)`、`lastWithdrawRoot(address,uint32)`、`exited(address,uint32)`
  - 同时声明（本任务只落存储，函数在 Task 3/4 实现）：
    `exitRequests(address,uint32) → (uint256 rootIndex, uint64 timestamp)`、`withdrawnAtRoot(address,uint32,uint256) → uint256`

- [ ] **Step 1: Write the failing test**

`test/Settlement.core.test.js`:

```javascript
const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Settlement core", function () {
  let owner, operator, alice;
  let token, settlement;
  const EXIT_DELAY = 300;

  beforeEach(async function () {
    [owner, operator, alice] = await ethers.getSigners();
    const Token = await ethers.getContractFactory("Token");
    token = await Token.deploy("Token A", "TKA", 18, 1000000);
    const Settlement = await ethers.getContractFactory("Settlement");
    settlement = await Settlement.deploy(operator.address, EXIT_DELAY);
    await settlement.registerAsset(await token.getAddress());
  });

  it("registers an asset with id 1", async function () {
    expect(await settlement.assetCount()).to.equal(1n);
    expect(await settlement.assets(1)).to.equal(await token.getAddress());
    expect(await settlement.assetIds(await token.getAddress())).to.equal(1n);
  });

  it("rejects registering the same asset twice", async function () {
    await expect(settlement.registerAsset(await token.getAddress()))
      .to.be.revertedWith("already registered");
  });

  it("only owner can register assets", async function () {
    await expect(settlement.connect(alice).registerAsset(await token.getAddress()))
      .to.be.revertedWith("not owner");
  });

  it("deposits and emits Deposit with an incrementing seq", async function () {
    const amt = ethers.parseUnits("100", 18);
    await token.transfer(alice.address, amt);
    await token.connect(alice).approve(await settlement.getAddress(), amt);

    await expect(settlement.connect(alice).deposit(1, amt))
      .to.emit(settlement, "Deposit")
      .withArgs(alice.address, 1, amt, 1n);

    await expect(settlement.connect(alice).deposit(1, amt))
      .to.emit(settlement, "Deposit")
      .withArgs(alice.address, 1, amt, 2n);

    expect(await token.balanceOf(await settlement.getAddress())).to.equal(amt * 2n);
    expect(await settlement.totalDeposited(1)).to.equal(amt * 2n);
  });

  it("rejects deposit when paused or for an unknown asset", async function () {
    await settlement.pause();
    await expect(settlement.connect(alice).deposit(1, 1n)).to.be.revertedWith("paused");
    await settlement.unpause();
    await expect(settlement.connect(alice).deposit(2, 1n)).to.be.revertedWith("unknown asset");
  });

  it("operator commits a state root and rootIndex increments", async function () {
    const root = ethers.keccak256("0x1234");
    await expect(settlement.connect(operator).commitStateRoot(root))
      .to.emit(settlement, "StateRootCommitted");
    const [stored, ts] = await settlement.rootAt(1);
    expect(stored).to.equal(root);
    expect(ts).to.be.greaterThan(0n);
    expect(await settlement.latestRootIndex()).to.equal(1n);
  });

  it("rejects commit from a non-operator", async function () {
    await expect(settlement.connect(alice).commitStateRoot(ethers.ZeroHash))
      .to.be.revertedWith("not operator");
  });

  it("rejects commit when the vault is insolvent", async function () {
    const Mock = await ethers.getContractFactory("MockToken");
    const mock = await Mock.deploy(ethers.parseUnits("1000", 18));
    await settlement.registerAsset(await mock.getAddress()); // assetId 2

    await mock.setNoOp(true); // transferFrom succeeds but moves nothing
    await mock.approve(await settlement.getAddress(), ethers.parseUnits("100", 18));
    await settlement.deposit(2, ethers.parseUnits("100", 18));

    expect(await settlement.totalDeposited(2)).to.equal(ethers.parseUnits("100", 18));
    await expect(settlement.connect(operator).commitStateRoot(ethers.ZeroHash))
      .to.be.revertedWith("insolvent");
  });

  it("owner can change the operator", async function () {
    await expect(settlement.setOperator(alice.address))
      .to.emit(settlement, "OperatorChanged")
      .withArgs(operator.address, alice.address);
    expect(await settlement.operator()).to.equal(alice.address);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npx hardhat test test/Settlement.core.test.js`
Expected: FAIL — `HH700: Artifact for contract "Settlement" not found`.

- [ ] **Step 3: Write the interfaces, mock token, and contract**

`contracts/interfaces/IERC20.sol`:

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
    function decimals() external view returns (uint8);
}
```

`contracts/interfaces/ISettlement.sol`:

```solidity
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
```

`contracts/test/MockToken.sol`:

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/// @notice Minimal ERC20 whose transferFrom can be made a no-op to simulate a
///         deposit that credits the ledger without funding the vault.
contract MockToken {
    string public name = "Mock";
    string public symbol = "MCK";
    uint8 public decimals = 18;
    uint256 public totalSupply;
    bool public noOp;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    constructor(uint256 supply) {
        totalSupply = supply;
        balanceOf[msg.sender] = supply;
    }

    function setNoOp(bool v) external {
        noOp = v;
    }

    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        return true;
    }

    function transfer(address to, uint256 amount) external returns (bool) {
        require(balanceOf[msg.sender] >= amount, "balance");
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
        return true;
    }

    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        if (noOp) return true;
        require(balanceOf[from] >= amount, "balance");
        require(allowance[from][msg.sender] >= amount, "allowance");
        allowance[from][msg.sender] -= amount;
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
        return true;
    }
}
```

`contracts/Settlement.sol`:

```solidity
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
```

> 注：`uint256 public latestRootIndex;` 自动生成的 getter 即满足接口中的 `latestRootIndex()`，无需再写显式函数。

- [ ] **Step 4: Run test to verify it passes**

Run: `npx hardhat test test/Settlement.core.test.js`
Expected: PASS (9 passing)。

- [ ] **Step 5: Commit**

```bash
git add contracts/interfaces/IERC20.sol contracts/interfaces/ISettlement.sol contracts/Settlement.sol contracts/test/MockToken.sol test/Settlement.core.test.js
git commit -m "feat(contracts): Settlement core (assets, deposit, state root commit, pause)"
```

---

### Task 3: Settlement 快速提现（Merkle 证明 + 防重放）

**Files:**
- Modify: `contracts/interfaces/ISettlement.sol`
- Modify: `contracts/Settlement.sol`
- Test: `test/Settlement.withdraw.test.js`

**Interfaces:**
- Consumes: `Ledger.Proof` 的链上对应物——调用方传入 `(account, assetId, balance, amount, rootIndex, proof)`。
- Produces:
  - `event Withdrawal(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex)`
  - `withdraw(address account, uint32 assetId, uint256 balance, uint256 amount, uint256 rootIndex, bytes32[] proof)`

- [ ] **Step 1: Write the failing test**

`test/Settlement.withdraw.test.js`:

```javascript
const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Settlement withdraw", function () {
  let owner, operator, alice;
  let token, settlement;
  const EXIT_DELAY = 300;
  const B = ethers.parseUnits("50", 18);

  beforeEach(async function () {
    [owner, operator, alice] = await ethers.getSigners();
    const Token = await ethers.getContractFactory("Token");
    token = await Token.deploy("Token A", "TKA", 18, 1000000);
    const Settlement = await ethers.getContractFactory("Settlement");
    settlement = await Settlement.deploy(operator.address, EXIT_DELAY);
    await settlement.registerAsset(await token.getAddress());

    await token.transfer(alice.address, B);
    await token.connect(alice).approve(await settlement.getAddress(), B);
    await settlement.connect(alice).deposit(1, B);

    // anchor a root whose only leaf is alice/asset1/B
    const leaf = await settlement.balanceLeaf(alice.address, 1, B);
    await settlement.connect(operator).commitStateRoot(leaf);
  });

  it("withdraws against the anchored root (single-leaf proof = [])", async function () {
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.emit(settlement, "Withdrawal")
      .withArgs(alice.address, 1, B, 1n);
    expect(await token.balanceOf(alice.address)).to.equal(B);
    expect(await settlement.totalWithdrawn(1)).to.equal(B);
  });

  it("rejects a withdrawal that exceeds the proven balance", async function () {
    const tooMuch = B + 1n;
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, tooMuch, 1, []))
      .to.be.revertedWith("bad amount");
  });

  it("rejects an oversized cumulative payout from the same root", async function () {
    await settlement.connect(operator).withdraw(alice.address, 1, B, B - 1n, 1, []);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, 2n, 1, []))
      .to.be.revertedWith("exceeds balance");
  });

  it("rejects a bad proof", async function () {
    const wrongLeaf = await settlement.balanceLeaf(alice.address, 1, B + 1n);
    // commit a root built from the wrong leaf, then try to withdraw the real balance
    await settlement.connect(operator).commitStateRoot(wrongLeaf);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 2, []))
      .to.be.revertedWith("bad proof");
  });

  it("rejects a stale root", async function () {
    await settlement.connect(operator).withdraw(alice.address, 1, B, 1n, 1, []);
    // moving to root 1 then attempting root 1 again is allowed (>=), but root 0 is not
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, 1n, 0, []))
      .to.be.revertedWith("bad root");
  });

  it("rejects withdraw while paused and by a non-operator", async function () {
    await expect(settlement.connect(alice).withdraw(alice.address, 1, B, B, 1, []))
      .to.be.revertedWith("not operator");
    await settlement.pause();
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.be.revertedWith("paused");
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npx hardhat test test/Settlement.withdraw.test.js`
Expected: FAIL — `withdraw` not defined (`TypeError: settlement.connect(...).withdraw is not a function`).

- [ ] **Step 3: Add the event + function to the interface and contract**

Add to `contracts/interfaces/ISettlement.sol`（在 `StateRootCommitted` 事件后）:

```solidity
    event Withdrawal(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex);
```

并在函数区加入：

```solidity
    function withdraw(
        address account,
        uint32 assetId,
        uint256 balance,
        uint256 amount,
        uint256 rootIndex,
        bytes32[] calldata proof
    ) external;
```

Add to `contracts/Settlement.sol`（在 `commitStateRoot` 之后）:

```solidity
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `npx hardhat test test/Settlement.withdraw.test.js`
Expected: PASS (6 passing)。

- [ ] **Step 5: Commit**

```bash
git add contracts/interfaces/ISettlement.sol contracts/Settlement.sol test/Settlement.withdraw.test.js
git commit -m "feat(contracts): permissionless-provable fast withdrawal with replay guards"
```

---

### Task 4: Settlement 强制退出（request / cancel / finalize）

**Files:**
- Modify: `contracts/interfaces/ISettlement.sol`
- Modify: `contracts/Settlement.sol`
- Test: `test/Settlement.exit.test.js`

**Interfaces:**
- Produces:
  - `event ExitRequested(address indexed account, uint32 indexed assetId, uint256 rootIndex, uint64 timestamp)`
  - `event ExitCancelled(address indexed account, uint32 indexed assetId)`
  - `event ExitFinalized(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex)`
  - `requestExit(uint32 assetId)` / `cancelExit(uint32 assetId)` / `finalizeExit(uint32 assetId, uint256 rootIndex, uint256 balance, bytes32[] proof)`

- [ ] **Step 1: Write the failing test**

`test/Settlement.exit.test.js`:

```javascript
const { expect } = require("chai");
const { ethers } = require("hardhat");
const { time } = require("@nomicfoundation/hardhat-network-helpers");

describe("Settlement forced exit", function () {
  let owner, operator, alice;
  let token, settlement;
  const EXIT_DELAY = 300;
  const B = ethers.parseUnits("75", 18);

  beforeEach(async function () {
    [owner, operator, alice] = await ethers.getSigners();
    const Token = await ethers.getContractFactory("Token");
    token = await Token.deploy("Token A", "TKA", 18, 1000000);
    const Settlement = await ethers.getContractFactory("Settlement");
    settlement = await Settlement.deploy(operator.address, EXIT_DELAY);
    await settlement.registerAsset(await token.getAddress());

    await token.transfer(alice.address, B);
    await token.connect(alice).approve(await settlement.getAddress(), B);
    await settlement.connect(alice).deposit(1, B);

    const leaf = await settlement.balanceLeaf(alice.address, 1, B);
    await settlement.connect(operator).commitStateRoot(leaf);
  });

  it("freezes the latest root on requestExit", async function () {
    await expect(settlement.connect(alice).requestExit(1))
      .to.emit(settlement, "ExitRequested");
    const req = await settlement.exitRequests(alice.address, 1);
    expect(req.rootIndex).to.equal(1n);
    expect(req.timestamp).to.be.greaterThan(0n);
  });

  it("finalizeExit reverts before the delay and succeeds after", async function () {
    await settlement.connect(alice).requestExit(1);
    await expect(settlement.connect(alice).finalizeExit(1, 1, B, []))
      .to.be.revertedWith("too early");

    await time.increase(EXIT_DELAY + 1);
    await expect(settlement.connect(alice).finalizeExit(1, 1, B, []))
      .to.emit(settlement, "ExitFinalized")
      .withArgs(alice.address, 1, B, 1n);

    expect(await token.balanceOf(alice.address)).to.equal(B);
    expect(await settlement.exited(alice.address, 1)).to.equal(true);
  });

  it("cannot exit twice", async function () {
    await settlement.connect(alice).requestExit(1);
    await time.increase(EXIT_DELAY + 1);
    await settlement.connect(alice).finalizeExit(1, 1, B, []);
    await expect(settlement.connect(alice).requestExit(1)).to.be.revertedWith("exited");
  });

  it("exit is allowed while paused", async function () {
    await settlement.pause();
    await settlement.connect(alice).requestExit(1);
    await time.increase(EXIT_DELAY + 1);
    await expect(settlement.connect(alice).finalizeExit(1, 1, B, []))
      .to.emit(settlement, "ExitFinalized");
  });

  it("a pending exit blocks fast withdrawal and deposit; cancel re-enables withdraw", async function () {
    await settlement.connect(alice).requestExit(1);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.be.revertedWith("exit pending");

    await settlement.connect(alice).cancelExit(1);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.emit(settlement, "Withdrawal");
  });

  it("rejects finalizeExit with a wrong root or bad proof", async function () {
    await settlement.connect(alice).requestExit(1);
    await time.increase(EXIT_DELAY + 1);
    await expect(settlement.connect(alice).finalizeExit(1, 2, B, [])).to.be.revertedWith("wrong root");
    await expect(settlement.connect(alice).finalizeExit(1, 1, B + 1n, [])).to.be.revertedWith("bad proof");
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npx hardhat test test/Settlement.exit.test.js`
Expected: FAIL — `requestExit` not defined (`TypeError: ... .requestExit is not a function`).

- [ ] **Step 3: Add events + functions**

Add to `contracts/interfaces/ISettlement.sol`（事件区）:

```solidity
    event ExitRequested(address indexed account, uint32 indexed assetId, uint256 rootIndex, uint64 timestamp);
    event ExitCancelled(address indexed account, uint32 indexed assetId);
    event ExitFinalized(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex);
```

Add to `contracts/interfaces/ISettlement.sol`（函数区）:

```solidity
    function requestExit(uint32 assetId) external;
    function cancelExit(uint32 assetId) external;
    function finalizeExit(uint32 assetId, uint256 rootIndex, uint256 balance, bytes32[] calldata proof) external;
```

Add to `contracts/Settlement.sol`（在 `withdraw` 之后）:

```solidity
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
        emit ExitFinalized(msg.sender, assetId, balance, rootIndex);
    }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `npx hardhat test test/Settlement.exit.test.js`
Expected: PASS (6 passing)。

- [ ] **Step 5: Run the full Solidity suite and commit**

Run: `npx hardhat test`
Expected: all Solidity tests pass.

```bash
git add contracts/interfaces/ISettlement.sol contracts/Settlement.sol test/Settlement.exit.test.js
git commit -m "feat(contracts): permissionless forced exit with delay and Merkle proof"
```

---

### Task 5: Go 账本核心（类型、操作、原子 Apply、不变式）

**Files:**
- Create: `backend/ledger/ledger.go`
- Create: `backend/ledger/op.go`
- Test: `backend/ledger/ledger_test.go`

**Interfaces:**
- Produces: `ledger` 包的 `AssetID`、`AccountID`、`AccountFromHex`、`Op`、`DepositOp`、`WithdrawOp`、`TransferOp`、`Ledger`（`New`、`WithStore`、`Apply`、`Balance`、`TotalBalance`、`Version`、`SnapshotBytes`）、`ErrInsufficientBalance`、`ErrEmptyOpKey`。

- [ ] **Step 1: Write the failing test**

`backend/ledger/ledger_test.go`:

```go
package ledger

import (
	"testing"

	"github.com/holiman/uint256"
)

func acct(b byte) AccountID {
	var a AccountID
	for i := range a {
		a[i] = b
	}
	return a
}

func amt(n uint64) *uint256.Int { return uint256.NewInt(n) }

func TestApplyDepositAndWithdraw(t *testing.T) {
	l := New()
	a := acct(1)

	if err := l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
	if err := l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(40), Ref: "w1"}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 60 {
		t.Fatalf("balance = %d, want 60", got)
	}
	if l.Version() != 2 {
		t.Fatalf("version = %d, want 2", l.Version())
	}
}

func TestWithdrawInsufficientIsAtomic(t *testing.T) {
	l := New()
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(10), Ref: "d1"})
	before := l.Version()

	err := l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(11), Ref: "w1"})
	if err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 10 {
		t.Fatalf("balance = %d, want unchanged 10", got)
	}
	if l.Version() != before {
		t.Fatalf("version changed on failed apply")
	}
}

func TestApplyIsAtomicAcrossOps(t *testing.T) {
	l := New()
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(10), Ref: "d1"})

	// second op fails -> first op must roll back
	err := l.Apply(
		WithdrawOp{Account: a, Asset: 1, Amount: amt(5), Ref: "w1"},
		WithdrawOp{Account: b, Asset: 1, Amount: amt(1), Ref: "w2"},
	)
	if err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 10 {
		t.Fatalf("balance = %d, want 10 (rolled back)", got)
	}
}

func TestTransferIsZeroSum(t *testing.T) {
	l := New()
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(30), Ref: "d1"})
	if err := l.Apply(TransferOp{From: a, To: b, Asset: 1, Amount: amt(30), Ref: "t1"}); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if l.Balance(a, 1).Sign() != 0 || l.Balance(b, 1).Uint64() != 30 {
		t.Fatalf("unexpected balances a=%s b=%s", l.Balance(a, 1), l.Balance(b, 1))
	}
	if l.TotalBalance(1).Uint64() != 30 {
		t.Fatalf("total = %s, want 30", l.TotalBalance(1))
	}
}

func TestIdempotentReplay(t *testing.T) {
	l := New()
	a := acct(1)
	op := DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "txhash:0"}
	if err := l.Apply(op); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := l.Apply(op); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 (no double credit)", got)
	}
	if l.Version() != 1 {
		t.Fatalf("version = %d, want 1", l.Version())
	}
}

func TestEmptyOpKeyRejected(t *testing.T) {
	l := New()
	err := l.Apply(DepositOp{Account: acct(1), Asset: 1, Amount: amt(1)})
	if err != ErrEmptyOpKey {
		t.Fatalf("err = %v, want ErrEmptyOpKey", err)
	}
}

func TestConservation(t *testing.T) {
	l := New()
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 7, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: b, Asset: 7, Amount: amt(50), Ref: "d2"})
	_ = l.Apply(TransferOp{From: a, To: b, Asset: 7, Amount: amt(20), Ref: "t1"})
	_ = l.Apply(WithdrawOp{Account: b, Asset: 7, Amount: amt(30), Ref: "w1"})

	want := uint256.NewInt(120) // 100 + 50 - 30
	if l.TotalBalance(7).Cmp(want) != 0 {
		t.Fatalf("total = %s, want %s", l.TotalBalance(7), want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./ledger/...`
Expected: FAIL — `no Go files in ...ledger` / `undefined: New`.

- [ ] **Step 3: Write the implementation**

`backend/ledger/ledger.go`:

```go
package ledger

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/holiman/uint256"
)

// AssetID identifies a registered ERC20 asset on-chain.
type AssetID uint32

// AccountID is a 20-byte account identifier (v1: an EOA address).
type AccountID [20]byte

func (a AccountID) Hex() string { return "0x" + hex.EncodeToString(a[:]) }

func AccountFromHex(s string) (AccountID, error) {
	var a AccountID
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return a, fmt.Errorf("ledger: bad account hex %q: %w", s, err)
	}
	if len(b) != 20 {
		return a, fmt.Errorf("ledger: account must be 20 bytes, got %d", len(b))
	}
	copy(a[:], b)
	return a, nil
}

var (
	ErrInsufficientBalance = errors.New("ledger: insufficient balance")
	ErrEmptyOpKey          = errors.New("ledger: empty op key")
	ErrNoLeaf              = errors.New("ledger: leaf not found")
)

// Ledger is an in-memory balance ledger with an optional write-ahead log.
type Ledger struct {
	mu       sync.Mutex
	balances map[AccountID]map[AssetID]*uint256.Int
	applied  map[string]struct{}
	version  uint64
	store    Store
}

func New() *Ledger {
	return &Ledger{
		balances: make(map[AccountID]map[AssetID]*uint256.Int),
		applied:  make(map[string]struct{}),
	}
}

// WithStore attaches a persistence store; Apply then writes a WAL batch per call.
func (l *Ledger) WithStore(s Store) *Ledger {
	l.store = s
	return l
}

func (l *Ledger) Version() uint64 { return l.version }

func (l *Ledger) Balance(a AccountID, asset AssetID) *uint256.Int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balanceLocked(a, asset)
}

func (l *Ledger) balanceLocked(a AccountID, asset AssetID) *uint256.Int {
	m := l.balances[a]
	if m == nil {
		return uint256.NewInt(0)
	}
	b := m[asset]
	if b == nil {
		return uint256.NewInt(0)
	}
	return new(uint256.Int).Set(b)
}

func (l *Ledger) TotalBalance(asset AssetID) *uint256.Int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := uint256.NewInt(0)
	for _, m := range l.balances {
		if b, ok := m[asset]; ok {
			total.Add(total, b)
		}
	}
	return total
}

// Apply validates and applies ops atomically. Ops whose key was already applied
// (or repeated within the batch) are skipped. On any error nothing changes.
func (l *Ledger) Apply(ops ...Op) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	fresh, err := l.freshOps(ops)
	if err != nil {
		return err
	}
	if len(fresh) == 0 {
		return nil
	}

	t, err := l.mutateLocked(fresh)
	if err != nil {
		return err
	}

	if l.store != nil {
		entries := make([]WALEntry, 0, len(fresh))
		seq := l.version
		for _, op := range fresh {
			seq++
			entries = append(entries, WALEntry{Seq: seq, Op: op.walRecord()})
		}
		if err := l.store.AppendBatch(entries); err != nil {
			t.rollback()
			return err
		}
		l.version = seq
	} else {
		l.version += uint64(len(fresh))
	}

	for _, op := range fresh {
		l.applied[op.Key()] = struct{}{}
	}
	return nil
}

func (l *Ledger) freshOps(ops []Op) ([]Op, error) {
	fresh := make([]Op, 0, len(ops))
	seen := make(map[string]struct{})
	for _, op := range ops {
		k := op.Key()
		if k == "" {
			return nil, ErrEmptyOpKey
		}
		if _, ok := l.applied[k]; ok {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		fresh = append(fresh, op)
	}
	return fresh, nil
}

func (l *Ledger) mutateLocked(fresh []Op) (*tx, error) {
	t := &tx{l: l}
	for _, op := range fresh {
		if err := op.apply(t); err != nil {
			t.rollback()
			return nil, err
		}
	}
	return t, nil
}

type journalEntry struct {
	a       AccountID
	asset   AssetID
	prev    *uint256.Int
	existed bool
}

// tx is a mutation scope over the ledger with a rollback journal.
type tx struct {
	l       *Ledger
	journal []journalEntry
}

func (t *tx) get(a AccountID, asset AssetID) *uint256.Int {
	m := t.l.balances[a]
	if m == nil {
		return uint256.NewInt(0)
	}
	b := m[asset]
	if b == nil {
		return uint256.NewInt(0)
	}
	return b
}

func (t *tx) record(a AccountID, asset AssetID) {
	e := journalEntry{a: a, asset: asset}
	if m := t.l.balances[a]; m != nil {
		if b, ok := m[asset]; ok {
			e.prev = new(uint256.Int).Set(b)
			e.existed = true
		}
	}
	t.journal = append(t.journal, e)
}

func (t *tx) set(a AccountID, asset AssetID, v *uint256.Int) {
	m := t.l.balances[a]
	if m == nil {
		m = make(map[AssetID]*uint256.Int)
		t.l.balances[a] = m
	}
	m[asset] = new(uint256.Int).Set(v)
}

func (t *tx) rollback() {
	for i := len(t.journal) - 1; i >= 0; i-- {
		e := t.journal[i]
		m := t.l.balances[e.a]
		if m == nil {
			continue
		}
		if e.existed {
			m[e.asset] = e.prev
		} else {
			delete(m, e.asset)
		}
	}
}

// --- snapshot serialization ---

type snapshotBalance struct {
	Account string `json:"account"`
	Asset   uint32 `json:"asset"`
	Amount  string `json:"amount"`
}

type snapshotState struct {
	Version  uint64            `json:"version"`
	Balances []snapshotBalance `json:"balances"`
}

func (l *Ledger) SnapshotBytes() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotBytesLocked()
}

func (l *Ledger) snapshotBytesLocked() ([]byte, error) {
	st := snapshotState{Version: l.version}
	for a, m := range l.balances {
		for asset, b := range m {
			if b.IsZero() {
				continue
			}
			st.Balances = append(st.Balances, snapshotBalance{a.Hex(), uint32(asset), b.Dec()})
		}
	}
	return json.Marshal(st)
}

func (l *Ledger) restoreSnapshot(data []byte) error {
	var st snapshotState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	l.balances = make(map[AccountID]map[AssetID]*uint256.Int)
	for _, sb := range st.Balances {
		a, err := AccountFromHex(sb.Account)
		if err != nil {
			return err
		}
		amount, err := uint256.FromDec(sb.Amount)
		if err != nil {
			return err
		}
		m := l.balances[a]
		if m == nil {
			m = make(map[AssetID]*uint256.Int)
			l.balances[a] = m
		}
		m[AssetID(sb.Asset)] = amount
	}
	l.version = st.Version
	return nil
}

// Snap persists a snapshot of the current state (no-op without a store).
func (l *Ledger) Snap() error {
	if l.store == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := l.snapshotBytesLocked()
	if err != nil {
		return err
	}
	return l.store.SaveSnapshot(data)
}
```

`backend/ledger/op.go`:

```go
package ledger

import (
	"fmt"

	"github.com/holiman/uint256"
)

// Op is a single ledger mutation with an idempotency key.
type Op interface {
	Key() string
	walRecord() OpRecord
	apply(t *tx) error
}

type DepositOp struct {
	Account AccountID
	Asset   AssetID
	Amount  *uint256.Int
	Ref     string
}

func (o DepositOp) Key() string { return o.Ref }
func (o DepositOp) walRecord() OpRecord {
	return OpRecord{Type: "deposit", Account: o.Account.Hex(), Asset: uint32(o.Asset), Amount: o.Amount.Dec(), Ref: o.Ref}
}
func (o DepositOp) apply(t *tx) error {
	cur := t.get(o.Account, o.Asset)
	t.record(o.Account, o.Asset)
	t.set(o.Account, o.Asset, new(uint256.Int).Add(cur, o.Amount))
	return nil
}

type WithdrawOp struct {
	Account AccountID
	Asset   AssetID
	Amount  *uint256.Int
	Ref     string
}

func (o WithdrawOp) Key() string { return o.Ref }
func (o WithdrawOp) walRecord() OpRecord {
	return OpRecord{Type: "withdraw", Account: o.Account.Hex(), Asset: uint32(o.Asset), Amount: o.Amount.Dec(), Ref: o.Ref}
}
func (o WithdrawOp) apply(t *tx) error {
	cur := t.get(o.Account, o.Asset)
	if cur.Cmp(o.Amount) < 0 {
		return ErrInsufficientBalance
	}
	t.record(o.Account, o.Asset)
	t.set(o.Account, o.Asset, new(uint256.Int).Sub(cur, o.Amount))
	return nil
}

type TransferOp struct {
	From, To AccountID
	Asset    AssetID
	Amount   *uint256.Int
	Ref      string
}

func (o TransferOp) Key() string { return o.Ref }
func (o TransferOp) walRecord() OpRecord {
	return OpRecord{Type: "transfer", From: o.From.Hex(), To: o.To.Hex(), Asset: uint32(o.Asset), Amount: o.Amount.Dec(), Ref: o.Ref}
}
func (o TransferOp) apply(t *tx) error {
	from := t.get(o.From, o.Asset)
	if from.Cmp(o.Amount) < 0 {
		return ErrInsufficientBalance
	}
	to := t.get(o.To, o.Asset)
	t.record(o.From, o.Asset)
	t.record(o.To, o.Asset)
	t.set(o.From, o.Asset, new(uint256.Int).Sub(from, o.Amount))
	t.set(o.To, o.Asset, new(uint256.Int).Add(to, o.Amount))
	return nil
}

// OpRecord is the serializable form of an Op used in the WAL.
type OpRecord struct {
	Type    string `json:"type"`
	Account string `json:"account,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Asset   uint32 `json:"asset"`
	Amount  string `json:"amount"`
	Ref     string `json:"ref"`
}

func (r OpRecord) ToOp() (Op, error) {
	amount, err := uint256.FromDec(r.Amount)
	if err != nil {
		return nil, fmt.Errorf("ledger: bad amount %q: %w", r.Amount, err)
	}
	switch r.Type {
	case "deposit":
		a, err := AccountFromHex(r.Account)
		if err != nil {
			return nil, err
		}
		return DepositOp{Account: a, Asset: AssetID(r.Asset), Amount: amount, Ref: r.Ref}, nil
	case "withdraw":
		a, err := AccountFromHex(r.Account)
		if err != nil {
			return nil, err
		}
		return WithdrawOp{Account: a, Asset: AssetID(r.Asset), Amount: amount, Ref: r.Ref}, nil
	case "transfer":
		from, err := AccountFromHex(r.From)
		if err != nil {
			return nil, err
		}
		to, err := AccountFromHex(r.To)
		if err != nil {
			return nil, err
		}
		return TransferOp{From: from, To: to, Asset: AssetID(r.Asset), Amount: amount, Ref: r.Ref}, nil
	}
	return nil, fmt.Errorf("ledger: unknown op type %q", r.Type)
}
```

> 注意：`ledger.go` 引用了 `Store`、`WALEntry`（Task 7 定义）。为了让 Task 5 能独立编译，**在 Task 5 先在本文件底部加一个最小 `Store` 接口与 `WALEntry` 声明**（Task 7 会用完整实现替换 `store.go`，届时从 `ledger.go` 删除这两段）：

```go
// 临时占位：Task 7 会在 store.go 中以完整版本替换。
type WALEntry struct {
	Seq uint64    `json:"seq"`
	Op  OpRecord  `json:"op"`
}

type Store interface {
	AppendBatch(entries []WALEntry) error
	SaveSnapshot(state []byte) error
	LoadSnapshot() ([]byte, error)
	Replay(fromSeq uint64) ([]WALEntry, error)
	Close() error
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./ledger/...`
Expected: PASS (7 tests)。

- [ ] **Step 5: Commit**

```bash
git add backend/ledger/ledger.go backend/ledger/op.go backend/ledger/ledger_test.go
git commit -m "feat(ledger): in-memory balance ledger with atomic apply and idempotency"
```

---

### Task 6: Go Merkle（叶子、建树、证明）+ 跨语言向量 fixture

**Files:**
- Create: `backend/ledger/merkle.go`
- Test: `backend/ledger/merkle_test.go`
- Generated: `backend/ledger/testdata/merkle_vectors.json`

**Interfaces:**
- Consumes: `Ledger`（Task 5）。
- Produces:
  - `Ledger.Root() [32]byte`
  - `Ledger.Proof(a AccountID, asset AssetID) (proof [][32]byte, balance *uint256.Int, err error)`
  - 内部：`leafHash`、`hashPair`、`merkleRoot`、`merkleProof`、`verifyProof`
  - fixture 文件格式（`testdata/merkle_vectors.json`）：
    ```json
    {
      "root": "0x…",
      "leaves": [{ "account": "0x…", "assetId": 1, "balance": "100", "leaf": "0x…" }],
      "proofs": [{ "index": 0, "proof": ["0x…"] }]
    }
    ```

- [ ] **Step 1: Write the failing test**

`backend/ledger/merkle_test.go`:

```go
package ledger

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSingleLeafRootEqualsLeaf(t *testing.T) {
	l := New()
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})

	root := l.Root()
	leaf := leafHash(a, 1, amt(100))
	if root != leaf {
		t.Fatalf("root %x != leaf %x", root, leaf)
	}
}

func TestRootIsDeterministic(t *testing.T) {
	build := func() *Ledger {
		l := New()
		_ = l.Apply(DepositOp{Account: acct(2), Asset: 3, Amount: amt(5), Ref: "d1"})
		_ = l.Apply(DepositOp{Account: acct(1), Asset: 1, Amount: amt(7), Ref: "d2"})
		return l
	}
	if build().Root() != build().Root() {
		t.Fatal("root is not deterministic")
	}
}

func TestProofRoundTrip(t *testing.T) {
	l := New()
	a, b, c := acct(1), acct(2), acct(3)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: b, Asset: 1, Amount: amt(200), Ref: "d2"})
	_ = l.Apply(DepositOp{Account: c, Asset: 1, Amount: amt(300), Ref: "d3"})

	root := l.Root()
	for _, x := range []AccountID{a, b, c} {
		proof, balance, err := l.Proof(x, 1)
		if err != nil {
			t.Fatalf("proof: %v", err)
		}
		leaf := leafHash(x, 1, balance)
		if !verifyProof(proof, root, leaf) {
			t.Fatalf("proof does not verify for %x", x)
		}
	}
}

func TestGoldenVectors(t *testing.T) {
	path := filepath.Join("testdata", "merkle_vectors.json")
	l := New()
	accounts := []AccountID{acct(0x11), acct(0x22), acct(0x33)}
	assets := []AssetID{1, 1, 2}
	amounts := []uint64{100, 200, 300}
	for i := range accounts {
		_ = l.Apply(DepositOp{Account: accounts[i], Asset: assets[i], Amount: amt(amounts[i]), Ref: "d" + string(rune('1'+i))})
	}

	type leafJSON struct {
		Account string `json:"account"`
		AssetID uint32 `json:"assetId"`
		Balance string `json:"balance"`
		Leaf    string `json:"leaf"`
	}
	type proofJSON struct {
		Index int      `json:"index"`
		Proof []string `json:"proof"`
	}
	type fixture struct {
		Root   string     `json:"root"`
		Leaves []leafJSON `json:"leaves"`
		Proofs []proofJSON `json:"proofs"`
	}

	entries := l.leafEntries()
	fx := fixture{Root: "0x" + hex.EncodeToString(hexBytes(l.Root()))}
	for i, e := range entries {
		fx.Leaves = append(fx.Leaves, leafJSON{
			Account: e.account.Hex(),
			AssetID: uint32(e.asset),
			Balance: e.balance.Dec(),
			Leaf:    "0x" + hex.EncodeToString(hexBytes(leafHash(e.account, e.asset, e.balance))),
		})
		proof, _, err := l.Proof(e.account, e.asset)
		if err != nil {
			t.Fatalf("proof %d: %v", i, err)
		}
		pj := proofJSON{Index: i}
		for _, p := range proof {
			pj.Proof = append(pj.Proof, "0x"+hex.EncodeToString(hexBytes(p)))
		}
		fx.Proofs = append(fx.Proofs, pj)
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		data, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("golden vectors written to %s", path)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (run with UPDATE_GOLDEN=1 first): %v", err)
	}
	var want fixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fx.Root != want.Root {
		t.Fatalf("root drifted: got %s want %s", fx.Root, want.Root)
	}
	if len(fx.Leaves) != len(want.Leaves) {
		t.Fatalf("leaf count drifted: got %d want %d", len(fx.Leaves), len(want.Leaves))
	}
	for i := range want.Leaves {
		if fx.Leaves[i].Leaf != want.Leaves[i].Leaf {
			t.Fatalf("leaf %d drifted: got %s want %s", i, fx.Leaves[i].Leaf, want.Leaves[i].Leaf)
		}
	}
}

func hexBytes(b [32]byte) []byte { return b[:] }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./ledger/... -run TestSingleLeafRootEqualsLeaf`
Expected: FAIL — `undefined: leafHash` / `undefined: l.Root`.

- [ ] **Step 3: Write the implementation**

`backend/ledger/merkle.go`:

```go
package ledger

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

type leafEntry struct {
	account AccountID
	asset   AssetID
	balance *uint256.Int
}

// leafHash must match MerkleLib.balanceLeaf on-chain:
// keccak256(bytes20(account) || bytes32(uint256(assetId)) || bytes32(balance))
func leafHash(a AccountID, asset AssetID, balance *uint256.Int) [32]byte {
	var buf [84]byte
	copy(buf[0:20], a[:])
	var assetBytes [32]byte
	binary.BigEndian.PutUint32(assetBytes[28:], uint32(asset))
	copy(buf[20:52], assetBytes[:])
	b := balance.Bytes32()
	copy(buf[52:84], b[:])
	h := crypto.Keccak256(buf[:])
	var out [32]byte
	copy(out[:], h)
	return out
}

func hashPair(a, b [32]byte) [32]byte {
	var buf [64]byte
	if bytes.Compare(a[:], b[:]) <= 0 {
		copy(buf[0:32], a[:])
		copy(buf[32:64], b[:])
	} else {
		copy(buf[0:32], b[:])
		copy(buf[32:64], a[:])
	}
	h := crypto.Keccak256(buf[:])
	var out [32]byte
	copy(out[:], h)
	return out
}

func merkleRoot(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return [32]byte{}
	}
	level := leaves
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i]) // promote odd node unchanged
			} else {
				next = append(next, hashPair(level[i], level[i+1]))
			}
		}
		level = next
	}
	return level[0]
}

func merkleProof(leaves [][32]byte, index int) [][32]byte {
	var proof [][32]byte
	level := leaves
	idx := index
	for len(level) > 1 {
		if idx%2 == 0 {
			if idx+1 < len(level) {
				proof = append(proof, level[idx+1])
			}
		} else {
			proof = append(proof, level[idx-1])
		}
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
			} else {
				next = append(next, hashPair(level[i], level[i+1]))
			}
		}
		level = next
		idx /= 2
	}
	return proof
}

func verifyProof(proof [][32]byte, root, leaf [32]byte) bool {
	computed := leaf
	for _, p := range proof {
		computed = hashPair(computed, p)
	}
	return computed == root
}

// leafEntries returns all non-zero balances sorted by (account, asset).
func (l *Ledger) leafEntries() []leafEntry {
	var out []leafEntry
	for a, m := range l.balances {
		for asset, b := range m {
			if b.IsZero() {
				continue
			}
			out = append(out, leafEntry{a, asset, b})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if c := bytes.Compare(out[i].account[:], out[j].account[:]); c != 0 {
			return c < 0
		}
		return out[i].asset < out[j].asset
	})
	return out
}

func (l *Ledger) leavesLocked() [][32]byte {
	entries := l.leafEntries()
	leaves := make([][32]byte, len(entries))
	for i, e := range entries {
		leaves[i] = leafHash(e.account, e.asset, e.balance)
	}
	return leaves
}

// Root returns the deterministic Merkle root of all balances.
func (l *Ledger) Root() [32]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return merkleRoot(l.leavesLocked())
}

// Proof returns the Merkle proof and balance for one (account, asset).
func (l *Ledger) Proof(a AccountID, asset AssetID) ([][32]byte, *uint256.Int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.leafEntries()
	leaves := make([][32]byte, len(entries))
	idx := -1
	for i, e := range entries {
		leaves[i] = leafHash(e.account, e.asset, e.balance)
		if e.account == a && e.asset == asset {
			idx = i
		}
	}
	if idx < 0 {
		return nil, nil, ErrNoLeaf
	}
	return merkleProof(leaves, idx), new(uint256.Int).Set(entries[idx].balance), nil
}
```

- [ ] **Step 4: Generate the fixture and verify the test passes**

Run: `cd backend; $env:UPDATE_GOLDEN=1; go test ./ledger/... -run TestGoldenVectors; Remove-Item Env:UPDATE_GOLDEN; go test ./ledger/...`
Expected: first run logs `golden vectors written...`; then all ledger tests PASS. `backend/ledger/testdata/merkle_vectors.json` now exists.

- [ ] **Step 5: Commit**

```bash
git add backend/ledger/merkle.go backend/ledger/merkle_test.go backend/ledger/testdata/merkle_vectors.json
git commit -m "feat(ledger): deterministic Merkle root + proofs matching the on-chain leaf encoding"
```

---

### Task 7: Go 账本持久化（FileStore + WAL 回放 + 快照恢复）

**Files:**
- Create: `backend/ledger/store.go`
- Modify: `backend/ledger/ledger.go`（删除 Task 5 的临时 `Store`/`WALEntry` 声明）
- Test: `backend/ledger/store_test.go`

**Interfaces:**
- Consumes: `Ledger`、`WALEntry`、`OpRecord`（Task 5）。
- Produces:
  - `Store` 接口、`NewFileStore(dir) (*FileStore, error)`、`Recover(store) (*Ledger, error)`。

- [ ] **Step 1: Write the failing test**

`backend/ledger/store_test.go`:

```go
package ledger

import "testing"

func TestRecoverFromWAL(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(30), Ref: "w1"})
	wantRoot := l.Root()
	wantVersion := l.Version()
	_ = store.Close()

	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store2: %v", err)
	}
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 70 {
		t.Fatalf("balance = %d, want 70", got)
	}
	if rec.Version() != wantVersion {
		t.Fatalf("version = %d, want %d", rec.Version(), wantVersion)
	}
	if rec.Root() != wantRoot {
		t.Fatal("recovered root != original root")
	}
}

func TestSnapshotThenReplay(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	if err := l.Snap(); err != nil {
		t.Fatalf("snap: %v", err)
	}
	_ = l.Apply(DepositOp{Account: b, Asset: 1, Amount: amt(50), Ref: "d2"})
	wantRoot := l.Root()
	_ = store.Close()

	store2, _ := NewFileStore(dir)
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rec.Balance(a, 1).Uint64() != 100 || rec.Balance(b, 1).Uint64() != 50 {
		t.Fatalf("unexpected balances a=%s b=%s", rec.Balance(a, 1), rec.Balance(b, 1))
	}
	if rec.Root() != wantRoot {
		t.Fatal("recovered root != original root")
	}
}

func TestRecoveredLedgerStillIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = store.Close()

	store2, _ := NewFileStore(dir)
	rec, _ := Recover(store2)
	if err := rec.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"}); err != nil {
		t.Fatalf("replay after recover: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./ledger/... -run TestRecoverFromWAL`
Expected: FAIL — `undefined: NewFileStore`.

- [ ] **Step 3: Write the implementation**

`backend/ledger/store.go`:

```go
package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// WALEntry is one append-only operation record.
type WALEntry struct {
	Seq uint64   `json:"seq"`
	Op  OpRecord `json:"op"`
}

// Store persists the ledger: an append-only WAL and periodic snapshots.
type Store interface {
	AppendBatch(entries []WALEntry) error
	SaveSnapshot(state []byte) error
	LoadSnapshot() ([]byte, error) // nil, nil when no snapshot exists
	Replay(fromSeq uint64) ([]WALEntry, error)
	Close() error
}

// FileStore is a directory-backed Store: <dir>/wal.log and <dir>/snapshot.json.
type FileStore struct {
	dir string
	mu  sync.Mutex
	wal *os.File
}

func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileStore{dir: dir, wal: f}, nil
}

func (s *FileStore) AppendBatch(entries []WALEntry) error {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf bytes.Buffer
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if _, err := s.wal.Write(buf.Bytes()); err != nil {
		return err
	}
	return s.wal.Sync()
}

func (s *FileStore) SaveSnapshot(state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := filepath.Join(s.dir, "snapshot.json.tmp")
	if err := os.WriteFile(tmp, state, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "snapshot.json"))
}

func (s *FileStore) LoadSnapshot() ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "snapshot.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *FileStore) Replay(fromSeq uint64) ([]WALEntry, error) {
	f, err := os.Open(filepath.Join(s.dir, "wal.log"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []WALEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e WALEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		if e.Seq > fromSeq {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wal.Close()
}

// Recover rebuilds a ledger from a snapshot plus subsequent WAL entries.
func Recover(store Store) (*Ledger, error) {
	l := New().WithStore(store)

	state, err := store.LoadSnapshot()
	if err != nil {
		return nil, err
	}
	if state != nil {
		if err := l.restoreSnapshot(state); err != nil {
			return nil, err
		}
	}

	entries, err := store.Replay(l.version)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		op, err := e.Op.ToOp()
		if err != nil {
			return nil, err
		}
		if _, ok := l.applied[op.Key()]; ok {
			l.version = e.Seq
			continue
		}
		if _, err := l.mutateLocked([]Op{op}); err != nil {
			return nil, err
		}
		l.applied[op.Key()] = struct{}{}
		l.version = e.Seq
	}
	return l, nil
}
```

在 `backend/ledger/ledger.go` 中**删除** Task 5 末尾的临时 `WALEntry` 与 `Store` 声明（它们现在由 `store.go` 提供）。

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./ledger/...`
Expected: PASS (10 tests，含 Task 5/6)。

- [ ] **Step 5: Commit**

```bash
git add backend/ledger/store.go backend/ledger/store_test.go backend/ledger/ledger.go
git commit -m "feat(ledger): file-backed WAL + snapshot persistence with recovery"
```

---

### Task 8: BlockchainAdapter 扩展 + EVMAdapter

**Files:**
- Modify: `backend/services/blockchain_adapter.go`
- Modify: `backend/services/blockchain_adapter_test.go`
- Create: `backend/services/evm_adapter.go`

**Interfaces:**
- Produces:
  - `BlockchainAdapter` 新增方法：`GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error)`
  - `MockBlockchainAdapter` 新增字段 `Logs []Log`、`Sent []*Transaction`
  - `NewEVMAdapter(rpcURL, privateKeyHex string, chainID int64) (*EVMAdapter, error)`，实现 `BlockchainAdapter`。

- [ ] **Step 1: Write the failing test**

Append to `backend/services/blockchain_adapter_test.go`:

```go
func TestBlockchainAdapter_GetLogs(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.Logs = []Log{
		{Address: "0xvault", BlockNumber: 5, TxHash: "0xaaa", LogIndex: 0},
		{Address: "0xvault", BlockNumber: 10, TxHash: "0xbbb", LogIndex: 1},
	}
	logs, err := adapter.GetLogs(1, 7, "0xvault", nil)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].BlockNumber != 5 {
		t.Fatalf("got %d logs, want exactly the block-5 one", len(logs))
	}
}

func TestBlockchainAdapter_RecordsSent(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	tx := &Transaction{To: "0xvault", Data: []byte{0x01}, GasLimit: 100000}
	if _, err := adapter.SendTransaction(tx); err != nil {
		t.Fatalf("SendTransaction: %v", err)
	}
	if len(adapter.Sent) != 1 || adapter.Sent[0] != tx {
		t.Fatalf("Sent not recorded")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./services/... -run TestBlockchainAdapter`
Expected: FAIL — `adapter.GetLogs undefined` / `adapter.Sent undefined`.

- [ ] **Step 3: Extend the interface/mock and add the EVM adapter**

In `backend/services/blockchain_adapter.go`:
1. 在 `BlockchainAdapter` 接口中加入：

```go
	// GetLogs returns logs matching an address/topics filter in [fromBlock, toBlock].
	GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error)
```

2. 在 `MockBlockchainAdapter` 结构体中加入字段：

```go
	Logs []Log
	Sent []*Transaction
```

3. 在 mock 的方法区加入：

```go
// GetLogs returns mock logs whose block number is inside [fromBlock, toBlock].
func (m *MockBlockchainAdapter) GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	var out []Log
	for _, l := range m.Logs {
		if l.BlockNumber < fromBlock || l.BlockNumber > toBlock {
			continue
		}
		if address != "" && l.Address != address {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}
```

4. 修改 `SendTransaction`，追加记录：

```go
func (m *MockBlockchainAdapter) SendTransaction(tx *Transaction) (string, error) {
	if m.Error != nil {
		return "", m.Error
	}
	m.Sent = append(m.Sent, tx)
	return m.SendTxHash, nil
}
```

`backend/services/evm_adapter.go`:

```go
package services

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// EVMAdapter is a real BlockchainAdapter backed by an Ethereum JSON-RPC node.
type EVMAdapter struct {
	client  *ethclient.Client
	key     *ecdsa.PrivateKey
	address common.Address
	chainID *big.Int
}

func NewEVMAdapter(rpcURL, privateKeyHex string, chainID int64) (*EVMAdapter, error) {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	key, err := crypto.HexToECDSA(trim0x(privateKeyHex))
	if err != nil {
		return nil, fmt.Errorf("parse operator key: %w", err)
	}
	return &EVMAdapter{
		client:  client,
		key:     key,
		address: crypto.PubkeyToAddress(key.PublicKey),
		chainID: big.NewInt(chainID),
	}, nil
}

func trim0x(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

func (a *EVMAdapter) GetBlockNumber() (uint64, error) {
	return a.client.BlockNumber(context.Background())
}

func (a *EVMAdapter) GetTransactionReceipt(txHash string) (*Receipt, error) {
	r, err := a.client.TransactionReceipt(context.Background(), common.HexToHash(txHash))
	if err != nil {
		return nil, err
	}
	out := &Receipt{
		Status:          r.Status,
		BlockNumber:     r.BlockNumber.Uint64(),
		BlockHash:       r.BlockHash.Hex(),
		TxHash:          r.TxHash.Hex(),
		GasUsed:         r.GasUsed,
		ContractAddress: r.ContractAddress.Hex(),
		CumulativeGasUsed: r.CumulativeGasUsed,
	}
	for _, l := range r.Logs {
		out.Logs = append(out.Logs, toLog(l))
	}
	return out, nil
}

func (a *EVMAdapter) CallContract(contract string, data []byte, blockNumber uint64) ([]byte, error) {
	msg := ethereum.CallMsg{To: ptrAddr(common.HexToAddress(contract)), Data: data}
	var bn *big.Int
	if blockNumber > 0 {
		bn = new(big.Int).SetUint64(blockNumber)
	}
	return a.client.CallContract(context.Background(), msg, bn)
}

func (a *EVMAdapter) SendTransaction(tx *Transaction) (string, error) {
	ctx := context.Background()
	nonce, err := a.client.PendingNonceAt(ctx, a.address)
	if err != nil {
		return "", err
	}
	gasPrice, err := a.client.SuggestGasPrice(ctx)
	if err != nil {
		return "", err
	}
	gasLimit := tx.GasLimit
	if gasLimit == 0 {
		gasLimit = 1_000_000
	}
	value := tx.Value
	if value == nil {
		value = big.NewInt(0)
	}
	raw := types.NewTransaction(nonce, common.HexToAddress(tx.To), value, gasLimit, gasPrice, tx.Data)
	signed, err := types.SignTx(raw, types.LatestSignerForChainID(a.chainID), a.key)
	if err != nil {
		return "", err
	}
	if err := a.client.SendTransaction(ctx, signed); err != nil {
		return "", err
	}
	return signed.Hash().Hex(), nil
}

func (a *EVMAdapter) GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error) {
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
	}
	if address != "" {
		q.Addresses = []common.Address{common.HexToAddress(address)}
	}
	for _, group := range topics {
		var hs []common.Hash
		for _, h := range group {
			hs = append(hs, common.HexToHash(h))
		}
		q.Topics = append(q.Topics, hs)
	}
	logs, err := a.client.FilterLogs(context.Background(), q)
	if err != nil {
		return nil, err
	}
	out := make([]Log, 0, len(logs))
	for _, l := range logs {
		out = append(out, toLog(l))
	}
	return out, nil
}

func toLog(l types.Log) Log {
	topics := make([]string, len(l.Topics))
	for i, t := range l.Topics {
		topics[i] = t.Hex()
	}
	return Log{
		Address:     l.Address.Hex(),
		Topics:      topics,
		Data:        l.Data,
		BlockNumber: l.BlockNumber,
		TxHash:      l.TxHash.Hex(),
		LogIndex:    l.Index,
	}
}

func ptrAddr(a common.Address) *common.Address { return &a }
```

> `ecdsa` 需要导入 `crypto/ecdsa`；`types.SignTx` 接受 `*ecdsa.PrivateKey`。

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./services/...`
Expected: PASS（含既有 adapter 测试 + 2 个新测试）。

- [ ] **Step 5: Commit**

```bash
git add backend/services/blockchain_adapter.go backend/services/blockchain_adapter_test.go backend/services/evm_adapter.go
git commit -m "feat(services): add GetLogs to adapter and implement EVMAdapter"
```

---

### Task 9: DepositIndexer（幂等入账 + 确认数）

**Files:**
- Create: `backend/services/deposit_indexer.go`
- Modify: `backend/services/registry.go`（在 `Config` 中新增字段）
- Test: `backend/services/deposit_indexer_test.go`

**Interfaces:**
- Consumes: `BlockchainAdapter`（Task 8）、`ledger.Ledger`/`DepositOp`（Task 5）。
- Produces:
  - `NewDepositIndexer(adapter BlockchainAdapter, l *ledger.Ledger, contract string, confirmations uint64) *DepositIndexer`
  - `(*DepositIndexer) PollOnce() error`（测试用：单次拉取）
  - `(*DepositIndexer) Name/Initialize/Start/Stop`（实现 `services.Service`）

- [ ] **Step 1: Write the failing test**

`backend/services/deposit_indexer_test.go`:

```go
package services

import (
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"web3-amm-dex-backend/ledger"
)

func depositLog(t *testing.T, block uint64, account common.Address, assetID uint32, amount string, seq uint64) Log {
	t.Helper()
	topic0 := crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)")).Hex()
	topic1 := common.BytesToHash(account.Bytes()).Hex()
	topic2 := common.BigToHash(common.Big1.SetUint64(uint64(assetID))).Hex()

	amtBytes, err := hex.DecodeString(amount)
	if err != nil {
		t.Fatalf("bad amount hex: %v", err)
	}
	data := make([]byte, 64)
	copy(data[32-len(amtBytes):32], amtBytes)
	seqB := common.BigToHash(common.Big1.SetUint64(seq))
	copy(data[32:64], seqB.Bytes())

	return Log{
		Address:     "0xvault",
		Topics:      []string{topic0, topic1, topic2},
		Data:        data,
		BlockNumber: block,
		TxHash:      "0xtx" + hex.EncodeToString(common.BigToHash(common.Big1.SetUint64(block)).Bytes()),
		LogIndex:    uint(block),
	}
}

func TestDepositIndexerCreditsOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	adapter.Logs = []Log{depositLog(t, 5, account, 1, "64", 1)} // 0x64 == 100

	l := ledger.New()
	idx := NewDepositIndexer(adapter, l, "0xvault", 1)

	if err := idx.PollOnce(); err != nil {
		t.Fatalf("poll: %v", err)
	}
	var a ledger.AccountID
	copy(a[:], account.Bytes())
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}

	// second poll must not double credit
	if err := idx.PollOnce(); err != nil {
		t.Fatalf("poll2: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 after re-poll", got)
	}
}

func TestDepositIndexerRespectsConfirmations(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x2222222222222222222222222222222222222222")
	adapter.Logs = []Log{depositLog(t, 19, account, 1, "a", 1)} // 0x0a == 10

	l := ledger.New()
	idx := NewDepositIndexer(adapter, l, "0xvault", 5) // needs block <= 15

	if err := idx.PollOnce(); err != nil {
		t.Fatalf("poll: %v", err)
	}
	var a ledger.AccountID
	copy(a[:], account.Bytes())
	if got := l.Balance(a, 1).Uint64(); got != 0 {
		t.Fatalf("balance = %d, want 0 (unconfirmed)", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./services/... -run TestDepositIndexer`
Expected: FAIL — `undefined: NewDepositIndexer`.

- [ ] **Step 3: Write the implementation**

`backend/services/deposit_indexer.go`:

```go
package services

import (
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

var depositTopic = crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)"))

// DepositIndexer credits on-chain Deposit events into the off-chain ledger.
type DepositIndexer struct {
	adapter       BlockchainAdapter
	ledger        *ledger.Ledger
	contract      string
	confirmations uint64

	mu        sync.Mutex
	nextBlock uint64
	interval  time.Duration
	stop      chan struct{}
	started   bool
}

func NewDepositIndexer(adapter BlockchainAdapter, l *ledger.Ledger, contract string, confirmations uint64) *DepositIndexer {
	if confirmations == 0 {
		confirmations = 1
	}
	return &DepositIndexer{
		adapter:       adapter,
		ledger:        l,
		contract:      contract,
		confirmations: confirmations,
		nextBlock:     1,
		interval:      5 * time.Second,
		stop:          make(chan struct{}),
	}
}

func (d *DepositIndexer) Name() string { return "DepositIndexer" }

func (d *DepositIndexer) Initialize(cfg Config) error {
	if cfg.RequiredConfirmations > 0 {
		d.confirmations = cfg.RequiredConfirmations
	}
	if cfg.SettlementAddress != "" {
		d.contract = cfg.SettlementAddress
	}
	if cfg.AnchorInterval > 0 {
		d.interval = time.Duration(cfg.AnchorInterval) * time.Second
	}
	return nil
}

func (d *DepositIndexer) Start() error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = true
	d.mu.Unlock()

	go func() {
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-ticker.C:
				_ = d.PollOnce()
			}
		}
	}()
	return nil
}

func (d *DepositIndexer) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		close(d.stop)
		d.started = false
	}
	return nil
}

// PollOnce fetches and applies all confirmed Deposit logs once.
func (d *DepositIndexer) PollOnce() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	latest, err := d.adapter.GetBlockNumber()
	if err != nil {
		return err
	}
	if latest < d.confirmations {
		return nil
	}
	to := latest - d.confirmations
	if to < d.nextBlock {
		return nil
	}

	logs, err := d.adapter.GetLogs(d.nextBlock, to, d.contract, [][]string{{depositTopic.Hex()}})
	if err != nil {
		return err
	}
	for _, l := range logs {
		op, err := decodeDeposit(l)
		if err != nil {
			return err
		}
		if err := d.ledger.Apply(op); err != nil {
			return err
		}
	}
	d.nextBlock = to + 1
	return nil
}

func decodeDeposit(l Log) (ledger.DepositOp, error) {
	if len(l.Topics) < 3 {
		return ledger.DepositOp{}, errors.New("deposit log: missing topics")
	}
	if len(l.Data) < 64 {
		return ledger.DepositOp{}, errors.New("deposit log: short data")
	}
	var account ledger.AccountID
	copy(account[:], common.HexToHash(l.Topics[1]).Bytes()[12:20])

	assetID := uint32(new(big.Int).SetBytes(common.HexToHash(l.Topics[2]).Bytes()).Uint64())
	amount, overflow := uint256.FromBig(new(big.Int).SetBytes(l.Data[0:32]))
	if overflow {
		return ledger.DepositOp{}, errors.New("deposit log: amount overflow")
	}

	ref := l.TxHash + ":" + big.NewInt(int64(l.LogIndex)).String()
	return ledger.DepositOp{Account: account, Asset: ledger.AssetID(assetID), Amount: amount, Ref: ref}, nil
}
```

> 依赖：本任务用到 `services.Config` 的 `SettlementAddress` / `RequiredConfirmations` / `AnchorInterval` 字段。**执行本任务前，先在 `backend/services/registry.go` 的 `Config` 结构体中加这些字段**（与 Task 13 的 `config` 包无关，二者独立）：

```go
	SettlementAddress     string
	DataDir               string
	RequiredConfirmations uint64
	AnchorInterval        int64 // seconds
	ChainID               int64
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./services/... -run TestDepositIndexer`
Expected: PASS (2 tests)。

- [ ] **Step 5: Commit**

```bash
git add backend/services/registry.go backend/services/deposit_indexer.go backend/services/deposit_indexer_test.go
git commit -m "feat(services): idempotent DepositIndexer with confirmation gating"
```

---

### Task 10: SettlementService（ABI 编码 + commitRoot + withdraw）

**Files:**
- Create: `backend/services/settlement.go`
- Test: `backend/services/settlement_test.go`

**Interfaces:**
- Consumes: `BlockchainAdapter`（Task 8）、`ledger.Ledger`（Task 5/6）。
- Produces:
  - `NewSettlementService(adapter BlockchainAdapter, l *ledger.Ledger, contract string) (*SettlementService, error)`
  - `(*SettlementService) CommitRoot(root [32]byte) (string, error)` → 返回 txHash
  - `(*SettlementService) LatestRootIndex() (uint64, error)`
  - `(*SettlementService) Withdraw(account ledger.AccountID, asset ledger.AssetID, balance, amount *uint256.Int, rootIndex uint64) (string, error)`
  - `Name/Initialize/Start/Stop`（`Service`）

- [ ] **Step 1: Write the failing test**

`backend/services/settlement_test.go`:

```go
package services

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestSettlementServiceCommitRootEncodesCalldata(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	svc, err := NewSettlementService(adapter, ledger.New(), "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var root [32]byte
	root[31] = 7
	if _, err := svc.CommitRoot(root); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx, got %d", len(adapter.Sent))
	}
	data := adapter.Sent[0].Data
	if len(data) != 4+32 {
		t.Fatalf("calldata len = %d, want 36", len(data))
	}
	if data[35] != 7 {
		t.Fatalf("root not encoded into calldata")
	}
}

func TestSettlementServiceLatestRootIndex(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// latestRootIndex() returns uint256 3
	resp := make([]byte, 32)
	resp[31] = 3
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	svc, _ := NewSettlementService(adapter, ledger.New(), contract)
	got, err := svc.LatestRootIndex()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got != 3 {
		t.Fatalf("rootIndex = %d, want 3", got)
	}
}

func TestSettlementServiceWithdrawBuildsProofCalldata(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, _ := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(100), 1); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx")
	}
	// selector for withdraw(address,uint32,uint256,uint256,uint256,bytes32[])
	data := adapter.Sent[0].Data
	if len(data) < 4 {
		t.Fatalf("no selector")
	}
	// account address must appear in the ABI-encoded args
	if !bytes.Contains(data, acc[:]) {
		t.Fatalf("account not encoded in calldata")
	}
	if new(big.Int).SetBytes(data[4:36]).Sign() == 0 {
		t.Fatalf("account word should be non-zero")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./services/... -run TestSettlementService`
Expected: FAIL — `undefined: NewSettlementService`.

- [ ] **Step 3: Write the implementation**

`backend/services/settlement.go`:

```go
package services

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

const settlementABIJSON = `[
  {"type":"function","name":"commitStateRoot","stateMutability":"nonpayable","inputs":[{"name":"root","type":"bytes32"}],"outputs":[{"name":"rootIndex","type":"uint256"}]},
  {"type":"function","name":"withdraw","stateMutability":"nonpayable","inputs":[
     {"name":"account","type":"address"},
     {"name":"assetId","type":"uint32"},
     {"name":"balance","type":"uint256"},
     {"name":"amount","type":"uint256"},
     {"name":"rootIndex","type":"uint256"},
     {"name":"proof","type":"bytes32[]"}],"outputs":[]},
  {"type":"function","name":"latestRootIndex","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]}
]`

// SettlementService encodes and submits Settlement contract calls.
type SettlementService struct {
	adapter  BlockchainAdapter
	abi      abi.ABI
	contract common.Address
	ledger   *ledger.Ledger
}

func NewSettlementService(adapter BlockchainAdapter, l *ledger.Ledger, contract string) (*SettlementService, error) {
	parsed, err := abi.JSON(strings.NewReader(settlementABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse settlement abi: %w", err)
	}
	return &SettlementService{
		adapter:  adapter,
		abi:      parsed,
		contract: common.HexToAddress(contract),
		ledger:   l,
	}, nil
}

func (s *SettlementService) Name() string                    { return "SettlementService" }
func (s *SettlementService) Initialize(cfg Config) error     { return nil }
func (s *SettlementService) Start() error                    { return nil }
func (s *SettlementService) Stop() error                     { return nil }

// CommitRoot submits commitStateRoot(root) and returns the tx hash.
func (s *SettlementService) CommitRoot(root [32]byte) (string, error) {
	data, err := s.abi.Pack("commitStateRoot", root)
	if err != nil {
		return "", err
	}
	return s.adapter.SendTransaction(&Transaction{
		To:       s.contract.Hex(),
		Data:     data,
		GasLimit: 500_000,
	})
}

// LatestRootIndex reads latestRootIndex() from the contract.
func (s *SettlementService) LatestRootIndex() (uint64, error) {
	data, err := s.abi.Pack("latestRootIndex")
	if err != nil {
		return 0, err
	}
	out, err := s.adapter.CallContract(s.contract.Hex(), data, 0)
	if err != nil {
		return 0, err
	}
	vals, err := s.abi.Unpack("latestRootIndex", out)
	if err != nil {
		return 0, err
	}
	if len(vals) != 1 {
		return 0, errors.New("latestRootIndex: unexpected output")
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return 0, errors.New("latestRootIndex: bad type")
	}
	return n.Uint64(), nil
}

// Withdraw submits a fast withdrawal using the ledger's Merkle proof.
func (s *SettlementService) Withdraw(account ledger.AccountID, asset ledger.AssetID, balance, amount *uint256.Int, rootIndex uint64) (string, error) {
	proof, proven, err := s.ledger.Proof(account, asset)
	if err != nil {
		return "", err
	}
	// Guard: never sign a proof for a balance we did not intend to publish.
	if proven.Cmp(balance) != 0 {
		return "", fmt.Errorf("withdraw: balance mismatch (ledger=%s arg=%s)", proven, balance)
	}
	proofHashes := make([][32]byte, len(proof))
	copy(proofHashes, proof)

	data, err := s.abi.Pack(
		"withdraw",
		common.BytesToAddress(account[:]),
		uint32(asset),
		balance.ToBig(),
		amount.ToBig(),
		new(big.Int).SetUint64(rootIndex),
		proofHashes,
	)
	if err != nil {
		return "", err
	}
	return s.adapter.SendTransaction(&Transaction{
		To:       s.contract.Hex(),
		Data:     data,
		GasLimit: 500_000,
	})
}
```

> 注：`settlement.go` 的 import 已包含 `"strings"`，用于解析内嵌的 ABI JSON。

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./services/... -run TestSettlementService`
Expected: PASS (3 tests)。

- [ ] **Step 5: Commit**

```bash
git add backend/services/settlement.go backend/services/settlement_test.go
git commit -m "feat(services): SettlementService for root anchoring and provable withdrawals"
```

---

### Task 11: StateAnchorService（定时锚定 + 根/版本映射）

**Files:**
- Create: `backend/services/state_anchor.go`
- Test: `backend/services/state_anchor_test.go`

**Interfaces:**
- Consumes: `SettlementService`（Task 10）、`ledger.Ledger`（Task 5/6）。
- Produces:
  - `type RootRecord struct { Root [32]byte; Version uint64; RootIndex uint64 }`
  - `NewStateAnchorService(settlement *SettlementService, l *ledger.Ledger) *StateAnchorService`
  - `(*StateAnchorService) AnchorOnce() (*RootRecord, error)`
  - `(*StateAnchorService) RootIndexForVersion(version uint64) (uint64, bool)`
  - `Name/Initialize/Start/Stop`（`Service`）

- [ ] **Step 1: Write the failing test**

`backend/services/state_anchor_test.go`:

```go
package services

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestStateAnchorOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// latestRootIndex() -> 1
	resp := make([]byte, 32)
	resp[31] = 1
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 9
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(42), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)
	rec, err := anchor.AnchorOnce()
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if rec.RootIndex != 1 || rec.Version != l.Version() {
		t.Fatalf("record = %+v, want rootIndex 1 version %d", rec, l.Version())
	}
	if rec.Root != l.Root() {
		t.Fatal("anchored root != ledger root")
	}
	got, ok := anchor.RootIndexForVersion(rec.Version)
	if !ok || got != 1 {
		t.Fatalf("RootIndexForVersion = %d, %v", got, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./services/... -run TestStateAnchor`
Expected: FAIL — `undefined: NewStateAnchorService`.

- [ ] **Step 3: Write the implementation**

`backend/services/state_anchor.go`:

```go
package services

import (
	"sync"
	"time"

	"web3-amm-dex-backend/ledger"
)

// RootRecord links a committed on-chain root to the ledger version it represents.
type RootRecord struct {
	Root      [32]byte
	Version   uint64
	RootIndex uint64
}

// StateAnchorService periodically commits the ledger root on-chain.
type StateAnchorService struct {
	settlement *SettlementService
	ledger     *ledger.Ledger

	mu      sync.Mutex
	records []RootRecord
	byVer   map[uint64]uint64 // version -> rootIndex

	interval time.Duration
	stop     chan struct{}
	started  bool
}

func NewStateAnchorService(settlement *SettlementService, l *ledger.Ledger) *StateAnchorService {
	return &StateAnchorService{
		settlement: settlement,
		ledger:     l,
		byVer:      make(map[uint64]uint64),
		interval:   60 * time.Second,
		stop:       make(chan struct{}),
	}
}

func (s *StateAnchorService) Name() string { return "StateAnchorService" }

func (s *StateAnchorService) Initialize(cfg Config) error {
	if cfg.AnchorInterval > 0 {
		s.interval = time.Duration(cfg.AnchorInterval) * time.Second
	}
	return nil
}

func (s *StateAnchorService) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				_, _ = s.AnchorOnce()
			}
		}
	}()
	return nil
}

func (s *StateAnchorService) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		close(s.stop)
		s.started = false
	}
	return nil
}

// AnchorOnce commits the current ledger root and records the mapping.
func (s *StateAnchorService) AnchorOnce() (*RootRecord, error) {
	root := s.ledger.Root()
	version := s.ledger.Version()

	if _, err := s.settlement.CommitRoot(root); err != nil {
		return nil, err
	}
	rootIndex, err := s.settlement.LatestRootIndex()
	if err != nil {
		return nil, err
	}

	rec := RootRecord{Root: root, Version: version, RootIndex: rootIndex}
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.byVer[version] = rootIndex
	s.mu.Unlock()
	return &rec, nil
}

func (s *StateAnchorService) RootIndexForVersion(version uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byVer[version]
	return idx, ok
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./services/... -run TestStateAnchor`
Expected: PASS。

- [ ] **Step 5: Run the whole Go suite and commit**

Run: `cd backend; go test ./...`
Expected: PASS（`cache`、`tmp` 目录不参与）。

```bash
git add backend/services/state_anchor.go backend/services/state_anchor_test.go
git commit -m "feat(services): StateAnchorService committing ledger roots on-chain"
```

---

### Task 12: 跨语言 Merkle 一致性测试

**Files:**
- Test: `test/MerkleVectors.test.js`

**Interfaces:**
- Consumes: `backend/ledger/testdata/merkle_vectors.json`（Task 6）、`MerkleLibHarness`（Task 1）。

- [ ] **Step 1: Write the test**

`test/MerkleVectors.test.js`:

```javascript
const { expect } = require("chai");
const { ethers } = require("hardhat");
const path = require("path");
const fs = require("fs");

describe("Merkle cross-language parity", function () {
  let harness;
  let fixture;

  before(async function () {
    const file = path.join(__dirname, "..", "backend", "ledger", "testdata", "merkle_vectors.json");
    if (!fs.existsSync(file)) {
      throw new Error(`missing fixture ${file}; run UPDATE_GOLDEN=1 go test ./ledger/...`);
    }
    fixture = JSON.parse(fs.readFileSync(file, "utf8"));
    harness = await (await ethers.getContractFactory("MerkleLibHarness")).deploy();
  });

  it("on-chain balanceLeaf matches every Go leaf", async function () {
    for (const leaf of fixture.leaves) {
      const onChain = await harness.balanceLeaf(leaf.account, leaf.assetId, leaf.balance);
      expect(onChain).to.equal(leaf.leaf);
    }
  });

  it("on-chain verify accepts every Go proof against the Go root", async function () {
    for (let i = 0; i < fixture.proofs.length; i++) {
      const p = fixture.proofs[i];
      const leaf = fixture.leaves[p.index].leaf;
      expect(await harness.verify(p.proof, fixture.root, leaf)).to.equal(true);
    }
  });

  it("on-chain computeRoot of all Go leaves equals the Go root", async function () {
    const leaves = fixture.leaves.map((l) => l.leaf);
    expect(await harness.computeRoot(leaves)).to.equal(fixture.root);
  });
});
```

- [ ] **Step 2: Run the test**

Run: `npx hardhat test test/MerkleVectors.test.js`
Expected: PASS (3 passing)。若失败，说明链上叶子编码/建树与 Go 实现不一致——回到 Task 1/6 修正，**不要**修改 fixture 迁就测试。

- [ ] **Step 3: Commit**

```bash
git add test/MerkleVectors.test.js
git commit -m "test(contracts): cross-language Merkle parity against Go-generated vectors"
```

---

### Task 13: 配置与文档

**Files:**
- Modify: `backend/config/config.go`
- Modify: `backend/config/defaults.go`
- Modify: `backend/config/config_test.go`
- Modify: `README.md`

**Interfaces:**
- Produces:
  - `config.SettlementConfig` 结构体与 `config.LoadSettlementConfig() SettlementConfig`。

- [ ] **Step 1: Write the failing test**

Append to `backend/config/config_test.go`:

```go
func TestLoadSettlementConfigDefaults(t *testing.T) {
	t.Setenv("SETTLEMENT_ADDRESS", "")
	t.Setenv("DATA_DIR", "")
	cfg := LoadSettlementConfig()
	if cfg.ChainID != 13700 {
		t.Fatalf("ChainID = %d, want 13700", cfg.ChainID)
	}
	if cfg.Confirmations != 1 {
		t.Fatalf("Confirmations = %d, want 1", cfg.Confirmations)
	}
	if cfg.ExitDelay != 300 {
		t.Fatalf("ExitDelay = %d, want 300", cfg.ExitDelay)
	}
}

func TestLoadSettlementConfigFromEnv(t *testing.T) {
	t.Setenv("SETTLEMENT_ADDRESS", "0xabc")
	t.Setenv("OPERATOR_KEY", "0xdead")
	t.Setenv("REQUIRED_CONFIRMATIONS", "12")
	t.Setenv("ANCHOR_INTERVAL", "90")
	t.Setenv("CHAIN_ID", "31337")
	cfg := LoadSettlementConfig()
	if cfg.ContractAddress != "0xabc" || cfg.OperatorKey != "0xdead" {
		t.Fatalf("address/key not loaded: %+v", cfg)
	}
	if cfg.Confirmations != 12 || cfg.AnchorIntervalSeconds != 90 || cfg.ChainID != 31337 {
		t.Fatalf("numeric fields not loaded: %+v", cfg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./config/... -run TestLoadSettlementConfig`
Expected: FAIL — `undefined: LoadSettlementConfig`.

- [ ] **Step 3: Write the implementation**

Add to `backend/config/config.go`:

```go
import (
	"os"
	"strconv"
	"time"
)

// SettlementConfig configures the settlement/ledger subsystem (subproject 1).
type SettlementConfig struct {
	ContractAddress       string
	OperatorKey           string
	ChainID               int64
	DataDir               string
	Confirmations         uint64
	AnchorIntervalSeconds int64
	ExitDelay             uint64
}

// LoadSettlementConfig reads settlement settings from the environment.
func LoadSettlementConfig() SettlementConfig {
	cfg := SettlementConfig{
		ContractAddress:       os.Getenv("SETTLEMENT_ADDRESS"),
		OperatorKey:           os.Getenv("OPERATOR_KEY"),
		ChainID:               13700,
		DataDir:               defaultDataDir(),
		Confirmations:         1,
		AnchorIntervalSeconds: 30,
		ExitDelay:             300,
	}
	if v := os.Getenv("CHAIN_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ChainID = n
		}
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("REQUIRED_CONFIRMATIONS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Confirmations = n
		}
	}
	if v := os.Getenv("ANCHOR_INTERVAL"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.AnchorIntervalSeconds = n
		}
	}
	if v := os.Getenv("EXIT_DELAY"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.ExitDelay = n
		}
	}
	return cfg
}

// AnchorInterval returns the anchor period as a Duration.
func (c SettlementConfig) AnchorInterval() time.Duration {
	return time.Duration(c.AnchorIntervalSeconds) * time.Second
}
```

Add to `backend/config/defaults.go`:

```go
// defaultDataDir returns the default on-disk location for the ledger WAL/snapshots.
func defaultDataDir() string {
	if v := os.Getenv("DATA_DIR"); v != "" {
		return v
	}
	return "./data/settlement"
}
```

> `defaults.go` 需导入 `os`。

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./config/...`
Expected: PASS。

- [ ] **Step 5: Update the README**

在 `README.md` 的"Project Structure"之前插入一节：

```markdown
## Settlement + Ledger (orderbook DEX, subproject 1)

The `contracts/Settlement.sol` vault + `backend/ledger` package form the custody
and accounting foundation for the off-chain order book DEX:

- Users deposit ERC20 assets into the vault (`deposit`).
- The off-chain ledger tracks internal balances and exports a deterministic Merkle root.
- `StateAnchorService` commits that root on-chain (`commitStateRoot`).
- Users withdraw via Merkle proof (`withdraw`), or permissionlessly exit after a
  delay (`requestExit` / `finalizeExit`) even if the operator is offline.

Design: `docs/superpowers/specs/2026-10-11-orderbook-dex-settlement-ledger-design.md`

### Settlement environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `SETTLEMENT_ADDRESS` | — | deployed vault address |
| `OPERATOR_KEY` | — | operator signing key (hex) |
| `CHAIN_ID` | `13700` | chain id (matches `hardhat.config.js`) |
| `DATA_DIR` | `./data/settlement` | ledger WAL/snapshot directory |
| `REQUIRED_CONFIRMATIONS` | `1` | deposit confirmations before crediting |
| `ANCHOR_INTERVAL` | `30` | state-root anchor period (seconds) |
| `EXIT_DELAY` | `300` | forced-exit delay (seconds) |

### Running the tests

```bash
npx hardhat test          # contracts
cd backend; go test ./... # ledger + services + config
```
```

- [ ] **Step 6: Run everything and commit**

Run: `npx hardhat test`
Expected: all pass.
Run: `cd backend; go test ./...`
Expected: all pass.

```bash
git add backend/config/config.go backend/config/defaults.go backend/config/config_test.go README.md
git commit -m "chore(config,docs): settlement config + README for subproject 1"
```

---

## Self-Review

**Spec coverage：**
- §4 合约（资产/充值/锚定/偿付/暂停/提现/退出/防重放/叶子/树/暂停不挡退出）→ Task 2/3/4。
- §5 账本（类型/操作/不变式/状态根/WAL+快照/幂等）→ Task 5/6/7。
- §6 服务（Settlement/Anchor/DepositIndexer + adapter 扩展）→ Task 8/9/10/11。
- §7 资金流（充值/快速提现/强制退出）→ Task 2/3/4/9/10/11 覆盖。
- §8 配置参数（EXIT_DELAY/CONFIRMATIONS/ANCHOR_INTERVAL/CHAIN_ID/DATA_DIR/OPERATOR_KEY）→ Task 13。
- §9 错误处理（重组靠确认数；偿付校验；幂等；暂停语义）→ Task 2/9 覆盖（重组回滚以确认数 + `nextBlock` 前进实现，撤销已入账版本列为后续增强）。
- §10 测试（Solidity/Go/golden/跨语言）→ Task 1–12。
- §11 文件布局 → 见 File Structure（新增 `contracts/test/*.sol`、`backend/services/evm_adapter.go`、`backend/config/*`，均为测试/实现所需）。

**已知取舍（写入计划的显式决定）：**
- 重组回滚：v1 用确认数 + 单调 `nextBlock` 防止重复入账；显式"撤销已入账版本"留作后续增强（spec §9 已述）。
- `services.Config` 新增字段在 Task 9 Step 3 前置加（消除任务顺序歧义）。
- `latestRootIndex` 用 public 变量实现接口函数（Task 2 已注明）。
- 跨语言一致性用 Go 生成 fixture + Hardhat 读取断言（Task 6 + Task 12），替代本地链端到端对拍。

**类型一致性：** `ledger.AccountID`/`AssetID`/`Op`/`Store`/`WALEntry`、`services.BlockchainAdapter.GetLogs`、`services.SettlementService`/`StateAnchorService`/`DepositIndexer` 的签名在"共享接口契约"与各任务中保持一致。

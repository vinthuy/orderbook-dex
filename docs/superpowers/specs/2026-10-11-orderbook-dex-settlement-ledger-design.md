# 子项目 1 设计：结算合约 + 链下账本

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：仅子项目 1（结算合约 + 链下账本），即整套系统的地基。
> **日期**：2026-10-11
> **状态**：已评审通过，待实现

---

## 1. 背景与目标

### 1.1 项目背景

目标系统是一个开源的去中心化订单簿交易所：订单簿与撮合在链下（高性能），资产托管与结
算在链上（可验证）。完整形态包含现货与永续合约、保证金、杠杆、资金费率与强平。

现有仓库 `D:\code\web3` 是一个 AMM DEX 学习项目（Solidity 0.8.19 手写 ERC20、Go + gin
后端、Hardhat 开发链），已具备 `Service` / `ServiceRegistry` / `EventBus` /
`BlockchainAdapter` 分层脚手架。本项目复用其技术栈与代码风格，但交易模型改为订单簿。

### 1.2 子项目路线图（依赖驱动）

| # | 子项目 | 内容 | 依赖 |
|---|--------|------|------|
| **1** | **结算合约 + 链下账本**（本文档） | 链上托管金库、状态根锚定、强制退出；链下内部余额账本 | — |
| 2 | 撮合引擎 | 内存订单簿、限价/市价、部分成交 | — |
| 3 | 排序器 + 网关 + 状态机 | 收单/鉴权/限流、WebSocket 推送；下单→撮合→更新账本 | 1,2 |
| 4 | 链上批量结算 + 索引器 | 批量提交成交结算上链、状态重建、历史查询 | 1,3 |
| 5 | 永续合约 + 风险引擎 | 仓位、保证金、杠杆、标记价格、资金费率、强平 | 1,2,3,4 |
| 6 | 前端 + 行情 | 交易界面、深度图、K 线 | 3,4 |

### 1.3 本子项目目标

1. 用户可把 ERC20 资产充值进链上结算合约（金库），交易所以链下账本记账并支持交易扣减。
2. 运营方周期性把账本余额的 Merkle 根锚定上链。
3. 用户始终能取回其"已锚定"的余额：运营方配合走快速提现，不配合可走强制退出。
4. 账本具备确定性状态根、守恒不变式、WAL 持久化与幂等重放能力。

### 1.4 非目标（明确不做，留给后续子项目）

撮合、订单簿、永续/风险引擎、HTTP/WS API、前端、ZK 有效性证明、多资产保证金、子账户
（仅预留类型接口）、快速提现的 LP 垫资、多签治理/合约可升级代理。

---

## 2. 信任模型与安全保证

### 2.1 角色

- **用户**：充值与提现，可自行发起链上退出。
- **运营方（operator）**：运行链下订单簿/撮合/账本、锚定状态根、批量发起快速提现。可被
  owner 轮换。
- **owner**：合约管理（注册资产、暂停、轮换 operator）。生产环境应为多签。

### 2.2 v1 提供的保证

1. **可退出性**：只要某余额出现在某个已锚定的状态根中，用户最终一定能取出它。
   - 运营方配合：`withdraw`（需 Merkle 证明）。
   - 运营方不配合：`requestExit` 冻结发起时的最新根 → 等待 `exitDelay` → `finalizeExit`
     用 Merkle 证明自行提取，**不依赖运营方在线**。
2. **偿付性**：`commitStateRoot` 时合约校验
   `token.balanceOf(vault) >= totalDeposited[asset] - totalWithdrawn[asset]`，不足则拒绝
   锚定，保证金库始终足以兑付全部负债。
3. **防重放**：快速提现按 `(account, asset)` 的 `lastRoot` 单调约束；强制退出按
   `(account, asset)` 一次性标记，冻结根与请求一一对应，不可重复领取。
4. **抗审查**：`paused` 只拦截充值与快速提现，**不拦截** `requestExit` / `finalizeExit`。

### 2.3 残余信任假设（无 ZK 的诚实边界）

v1 **无法**在密码学上阻止运营方锚定一个"少算/错误"的根（例如选择性低估某用户余额，使其
可退出金额变小）。彻底解决需要有效性证明（ZK），属后续子项目。v1 的缓解手段是：状态根
锚定即公开、退出有延迟窗口、账本与根可被任何人审计比对。

此外，快速提现的正确性依赖运营方诚实（其本身即由运营方发起）；强制退出则是无需许可的兜底。

---

## 3. 架构概览

```
┌───────────────────────────────────────────────────────────┐
│                        用户 / 客户端                        │
└───────┬───────────────────────────────────────┬───────────┘
        │ approve/deposit                       │ requestExit/finalizeExit
        ▼                                       ▼
┌───────────────────────────────┐   ┌───────────────────────────────┐
│      Settlement.sol (链上金库)  │   │        Merkle 状态根            │
│  deposit / withdraw / exit     │◄──│  commitStateRoot(root)         │
└───────────────┬───────────────┘   └───────────────────────────────┘
                │ Deposit 事件                    ▲
                ▼                                 │ root
┌───────────────────────────────┐   ┌───────────────────────────────┐
│  DepositIndexer (幂等入账)      │──►│   Ledger (链下账本, 内存+WAL)   │
└───────────────────────────────┘   └───────────────┬───────────────┘
                                                     │ 供撮合扣减(子项目2,3)
                                     ┌───────────────▼───────────────┐
                                     │   StateAnchorService (定时锚定) │
                                     └───────────────────────────────┘
```

---

## 4. 链上合约 `Settlement.sol`

### 4.1 职责

持有全部用户资产；注册资产；接收充值；锚定状态根；执行快速提现；提供无需许可的退出兜底。

### 4.2 状态变量

| 变量 | 类型 | 说明 |
|------|------|------|
| `owner` | `address` | 管理账户（生产为多签） |
| `operator` | `address` | 可锚定根、发起快速提现 |
| `paused` | `bool` | 暂停标志（只挡充值/快速提现） |
| `exitDelay` | `uint64` | 强制退出延迟（秒），owner 可配 |
| `assets` | `mapping(uint32 => address)` | `assetId => token` |
| `assetIds` | `mapping(address => uint32)` | 反查；0 表示未注册 |
| `assetCount` | `uint32` | 已注册资产数 |
| `roots` | `mapping(uint256 => RootInfo)` | `rootIndex => {bytes32 root; uint64 timestamp}` |
| `latestRootIndex` | `uint256` | 最新根索引（从 1 开始，0 表示无根） |
| `lastWithdrawRoot` | `mapping(address => mapping(uint32 => uint256))` | 快速提现防重放 |
| `exitRequests` | `mapping(address => mapping(uint32 => ExitRequest))` | `{uint256 rootIndex; uint64 timestamp}` |
| `exited` | `mapping(address => mapping(uint32 => bool))` | 已退出标记 |
| `totalDeposited` | `mapping(uint32 => uint256)` | 累计充值 |
| `totalWithdrawn` | `mapping(uint32 => uint256)` | 累计出金（含快速提现与退出） |

### 4.3 事件

```solidity
event AssetRegistered(uint32 indexed assetId, address indexed token, uint8 decimals);
event Deposit(address indexed account, uint32 indexed assetId, uint256 amount, uint256 depositSeq);
event StateRootCommitted(uint256 indexed rootIndex, bytes32 root, uint64 timestamp);
event Withdrawal(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex);
event ExitRequested(address indexed account, uint32 indexed assetId, uint256 rootIndex, uint64 timestamp);
event ExitCancelled(address indexed account, uint32 indexed assetId);
event ExitFinalized(address indexed account, uint32 indexed assetId, uint256 amount, uint256 rootIndex);
event OperatorChanged(address indexed oldOperator, address indexed newOperator);
event Paused(address indexed by);
event Unpaused(address indexed by);
```

### 4.4 接口

```solidity
interface ISettlement {
    // --- owner ---
    function registerAsset(address token) external returns (uint32 assetId);
    function setOperator(address newOperator) external;
    function setExitDelay(uint64 newDelay) external;
    function pause() external;
    function unpause() external;

    // --- user ---
    function deposit(uint32 assetId, uint256 amount) external;
    function requestExit(uint32 assetId) external;
    function cancelExit(uint32 assetId) external;
    function finalizeExit(
        uint32 assetId,
        uint256 rootIndex,
        uint256 balance,
        bytes32[] calldata proof
    ) external;

    // --- operator ---
    function commitStateRoot(bytes32 root) external returns (uint256 rootIndex);
    function withdraw(
        address account,
        uint32 assetId,
        uint256 balance,
        uint256 amount,
        uint256 rootIndex,
        bytes32[] calldata proof
    ) external;

    // --- views ---
    function latestRootIndex() external view returns (uint256);
    function rootAt(uint256 rootIndex) external view returns (bytes32 root, uint64 timestamp);
    function balanceLeaf(address account, uint32 assetId, uint256 balance)
        external pure returns (bytes32);
    function isExited(address account, uint32 assetId) external view returns (bool);
}
```

### 4.5 叶子与 Merkle 树

- 叶子定义（链上链下必须一致）：
  `leaf = keccak256(abi.encodePacked(bytes20(account), bytes32(uint256(assetId)), bytes32(balance)))`
  （固定 20 + 32 + 32 = 84 字节紧凑编码，避免 `abi.encode` 的填充歧义）
- 建树规则（与 OpenZeppelin `MerkleProof` 兼容）：
  - 叶子按 `(account, assetId)` 升序排列后逐层两两哈希；
  - 内部节点 `= keccak256(min(a,b) || max(a,b))`（排序对哈希，防第二原像）；
  - 层级节点数为奇数时，末尾节点**原样提升**到上一层（不复制）；
  - 空树根定义为 `bytes32(0)`。

### 4.6 关键函数语义与不变量

- `deposit(assetId, amount)`：`require(!paused)`；`require(exitRequests[account][assetId].rootIndex == 0)`
  （未决退出期间不可充值）；`transferFrom(msg.sender, vault, amount)`；若该 `(account, assetId)`
  曾退出则重置 `exited[account][assetId] = false`（消除"退出后重新充值被卡死"的坑）；
  `totalDeposited[assetId] += amount`；`depositSeq` 为每账户递增计数，emit `Deposit`。
- `commitStateRoot(root)`：仅 operator；`require(balanceOf(vault) >= totalDeposited - totalWithdrawn)`；
  `latestRootIndex++`；记录 `roots[latestRootIndex] = {root, now}`；emit `StateRootCommitted`。
- `withdraw(account, assetId, balance, amount, rootIndex, proof)`：仅 operator；`require(!paused)`；
  `require(!exited[account][assetId])`；
  `require(exitRequests[account][assetId].rootIndex == 0)`（有未决退出时禁止快速提现，避免双付）；
  以 `leaf = balanceLeaf(account, assetId, balance)` 校验 proof 属于 `roots[rootIndex].root`；
  `require(amount > 0 && amount <= balance)`；
  `require(rootIndex >= lastWithdrawRoot[account][assetId])` 且更新之；
  `totalWithdrawn += amount`；`token.transfer(account, amount)`；emit `Withdrawal`。
  （`balance` 由 operator 传入，由 Merkle 证明绑定到已锚定的根；合约无法自行得知链下余额。）
- `requestExit(assetId)`：`require(exited == false)`；若已有未决请求则幂等返回；冻结
  `rootIndex = latestRootIndex`、`timestamp = now`；emit `ExitRequested`。**不受 `paused` 影响。**
- `cancelExit(assetId)`：仅在该 `(account, asset)` 尚未 finalize 时允许，清除请求；
  emit `ExitCancelled`。
- `finalizeExit(assetId, rootIndex, balance, proof)`：`require(请求存在 && rootIndex == 请求.rootIndex)`；
  `require(now >= 请求.timestamp + exitDelay)`；`require(balance > 0)`；以
  `leaf = balanceLeaf(account, assetId, balance)` 校验 proof 属于 `roots[rootIndex].root`；
  `exited[account][assetId] = true`；清除请求；`totalWithdrawn += balance`；
  `token.transfer(account, balance)`；emit `ExitFinalized`。**不受 `paused` 影响。**
  v1 为**全额退出**（提取该冻结根中该资产的完整余额），不支持部分退出；部分退出留待后续子项目。
- `pause/unpause`：仅 owner；emit 事件。只影响 `deposit` 与 `withdraw`。

### 4.7 安全要点

- Solidity 0.8.19 自带溢出检查；无外部回调，无重入面。
- 快速提现与退出互斥（未决退出期间禁止快速提现），退出一次性标记，根索引 + 请求绑定防重放。
- 所有者与 operator 均不可直接转移用户资金，除按上述受约束路径。
- 无 `delegatecall`、无自毁、无可升级代理（v1 有意保持不可变）。

---

## 5. 链下账本 `backend/ledger`（Go）

### 5.1 类型

```go
type AssetID   uint32
type AccountID [20]byte            // v1 = 地址；独立类型以便将来扩展子账户
type Amount    = *uint256.Int      // 依赖中已有 github.com/holiman/uint256
```

资产注册表由 config/链上事件驱动：`AssetID -> {token address, decimals, symbol}`。

### 5.2 操作与 API

```go
type Ledger struct { /* 内部状态 */ }

func New() *Ledger
func (l *Ledger) Balance(a AccountID, asset AssetID) *uint256.Int
func (l *Ledger) Apply(ops ...Op) error           // 原子：全部成功或全部回滚
func (l *Ledger) Root() (root [32]byte, version uint64)
func (l *Ledger) Snapshot(w io.Writer) error
func (l *Ledger) Restore(r io.Reader) error
func (l *Ledger) TotalBalance(asset AssetID) *uint256.Int

// Op 接口：带幂等键，重复应用不产生副作用
type Op interface {
    Key() string
    apply(st *state) error
}

type DepositOp  struct{ Account AccountID; Asset AssetID; Amount *uint256.Int; Ref string }
type WithdrawOp struct{ Account AccountID; Asset AssetID; Amount *uint256.Int; Ref string }
type TransferOp struct{ From, To AccountID; Asset AssetID; Amount *uint256.Int; Ref string }
```

- `Apply` 先在副本上执行，全部成功才提交（原子性）。
- 每个 `Op` 的 `Key()` 为幂等键（充值用 `txHash:logIndex`；撮合成交用确定性成交 ID）。
  已应用过的 key 记入去重集，重放直接跳过（返回 nil）。

### 5.3 不变式（必须由测试守护）

1. 任意 `(account, asset)` 余额非负。
2. 每个资产 `Σ余额 == Σ累计充值 − Σ累计出金`（守恒）。
3. `TransferOp` 零和：转账前后该资产总余额不变。
4. 相同状态 → 相同 `Root()`（确定性）。

### 5.4 状态根

- 汇总所有非零 `(account, asset, balance)`，按 `(account, assetId)` 升序排列为叶子，
  按第 4.5 节规则建树，返回 32 字节根与账本版本号（每 `Apply` 成功递增）。
- 叶子编码与合约 `balanceLeaf` 完全一致（紧凑 84 字节）。
- 提供 golden 测试：固定输入集的根哈希硬编码断言，防止编码规则漂移。

### 5.5 持久化 `LedgerStore`

- **WAL**：追加型操作日志，每条含 `seq`、幂等键、操作内容（JSON 行）。
- **快照**：周期性全量状态快照（含 `version`、`lastAppliedSeq`、去重集）。
- **恢复**：加载最新快照 → 从 `lastAppliedSeq` 之后重放 WAL。
- 抽象为接口，v1 实现为本地文件；后续可替换为 bbolt / Postgres 而不改账本核心。

```go
type Store interface {
    Append(entry WALEntry) error
    Snapshot(state []byte, version uint64, lastSeq uint64) error
    LoadLatest() (snapshot []byte, version uint64, lastSeq uint64, err error)
    Replay(fromSeq uint64) ([]WALEntry, error)
}
```

---

## 6. 服务层（沿用现有 `Service` 模式）

三者均实现 `services.Service`（`Name/Initialize/Start/Stop`），注册进 `ServiceRegistry`，
通过 `EventBus` 解耦。

### 6.1 `SettlementService`

- 封装合约交互：解析 ABI（来自 Hardhat artifacts）、构造/发送交易、
  `CommitRoot(root)`、`Withdraw(...)`、读取 `latestRootIndex` 等视图。
- 依赖现有 `BlockchainAdapter`。

### 6.2 `DepositIndexer`

- 订阅链上 `Deposit` 事件 → 调用 `ledger.Apply(DepositOp{...})`（幂等键 `txHash:logIndex`）。
- 启动时按 `fromBlock` 回填；按 `requiredConfirmations` 确认后入账；支持重组时回滚重扫
  （记录已入账的 `(blockNumber, txHash, logIndex)`，遇重组撤销对应版本）。
- **需要扩展 `BlockchainAdapter`**：新增按区间/地址/话题拉取日志的方法
  （`GetLogs(ctx, from, to, address, topics) ([]Log, error)`），并同步更新 `MockBlockchainAdapter`。

### 6.3 `StateAnchorService`

- 按 `anchorInterval` 定时（或按累计成交数触发）读取 `ledger.Root()`，调用
  `SettlementService.CommitRoot(root)` 上链。
- 维护 `rootIndex ↔ 账本 version` 的映射，供退出流程与索引器对齐。
- 提交失败重试；不做本地状态假设，重启后以链上 `latestRootIndex` 为准。

---

## 7. 资金流

### 7.1 充值

1. 用户 `token.approve(vault, amount)`。
2. 用户调用 `Settlement.deposit(assetId, amount)`。
3. 合约 `transferFrom` 入金库，emit `Deposit`。
4. `DepositIndexer` 确认 N 个区块后幂等入账。
5. 下一次 `commitStateRoot` 把该余额纳入状态根。

### 7.2 快速提现

1. 用户链下签名提现请求（EIP-712，含 nonce 防重放）。
2. 运营方校验请求并组批，构造 Merkle 证明。
3. 调用 `Settlement.withdraw(account, asset, amount, rootIndex, proof)`。
4. 出金 → 账本扣减（`WithdrawOp`）→ 生成新根。

### 7.3 强制退出（兜底，无需运营方配合）

1. 用户调用 `requestExit(assetId)`，冻结发起时的最新根。
2. 等待 `exitDelay`。
3. 用户调用 `finalizeExit(assetId, rootIndex, proof)`，凭 Merkle 证明提取该根中的全部余额。
4. 标记退出，账户该资产不再参与后续交易。

---

## 8. 配置参数

| 参数 | dev 默认 | 生产建议 | 说明 |
|------|----------|----------|------|
| `EXIT_DELAY` | 300 秒 | 7 天 | 强制退出延迟 |
| `REQUIRED_CONFIRMATIONS` | 1 | 12 | 充值入账确认数 |
| `ANCHOR_INTERVAL` | 30 秒 | 60 秒 | 状态根锚定周期 |
| `ETH_RPC_URL` | `http://localhost:8545` | — | 复用现有配置 |
| `OPERATOR_KEY` | 本地测试私钥 | KMS/多签 | 锚定与快速提现签名 |
| `CHAIN_ID` | 13700（沿用 hardhat.config.js） | — | 复用现有配置 |

配置通过既有 `config` 包 + 环境变量注入，沿用现有 `LoadConfig` 风格。

---

## 9. 错误处理与边界

- **链重组**：索引器以确认数为界，支持回滚重扫；账本记录可撤销的入账版本。
- **金库存量不足**：`commitStateRoot` 的偿付校验直接拒绝锚定。
- **提现失败/超时**：运营方侧重试；用户侧不受影响，可用退出兜底。
- **重复入账 / 重复提交**：幂等键去重。
- **暂停语义**：`paused` 只挡 `deposit`/`withdraw`，`requestExit`/`finalizeExit` 永远可用。
- **运营方下线**：退出流程与运营方在线与否无关。
- **非法输入**：资产未注册、金额为 0、proof 不匹配、延迟未到均 revert，并有对应测试。

---

## 10. 测试策略

### 10.1 Solidity（Hardhat + Chai）

- `deposit`：转入成功、余额入金库、事件、暂停时 revert。
- `commitStateRoot`：operator 权限、偿付校验、根索引递增、事件。
- `withdraw`：有效证明成功；无效证明 / 金额超余额 / rootIndex 过期 / 存在未决退出 / 重复 rootIndex 均 revert。
- `requestExit` / `finalizeExit`：延迟未到 revert；到期成功；重复 finalize revert；
  `paused` 下仍可退出；`cancelExit` 后可快速提现。
- 权限：非 owner/operator 调用管理函数 revert。
- 叶子与根：`balanceLeaf` 与链下实现对齐（见集成测试）。

### 10.2 Go（表驱动单测）

- 账本不变式：守恒、非负、零和。
- 幂等：重复 `Apply` 同一 key 无副作用。
- WAL：写→重启→恢复，状态与根一致。
- Merkle：golden 向量（固定输入 → 固定根）；空树、单叶、奇数节点。
- 索引器：mock 适配器下的幂等入账、确认数、重组回滚。
- 锚定服务：根与版本映射、重试。

### 10.3 集成

- 起本地 Hardhat 链，部署 `Settlement`；用同一批 `(account, asset, balance)`：
  - Go 账本算出的 root == 合约 `commitStateRoot` 接受并存储的 root；
  - 用 Go 生成的 proof 在链上成功 `withdraw` / `finalizeExit`。

---

## 11. 文件布局

```
contracts/
  Settlement.sol
  interfaces/ISettlement.sol
  libraries/MerkleLib.sol
test/
  Settlement.test.js
backend/
  ledger/{ledger.go, merkle.go, store.go, wal.go, ledger_test.go, merkle_test.go, wal_test.go}
  services/{settlement.go, state_anchor.go, deposit_indexer.go,
            settlement_test.go, state_anchor_test.go, deposit_indexer_test.go}
  services/blockchain_adapter.go        # 扩展 GetLogs
  models/settlement.go
```

---

## 12. 已定决策记录

| 决策 | 结论 |
|------|------|
| 交易所形态 | 去中心化订单簿 DEX |
| 撮合/结算模型 | 混合式：链下订单簿 + 链下撮合 + 链上结算 |
| v1 范围 | 目标为现货 + 永续（永续在子项目 5）；子项目 1 只做托管/账本地基 |
| 信任模型 | 状态根锚定 + 强制退出（无 ZK） |
| 技术栈 | Go（后端/账本）+ Solidity 0.8.19（合约）+ Hardhat + TypeScript（前端，后续） |
| 状态根机制 | OpenZeppelin 兼容 Merkle，叶子 = `keccak256(bytes20(account)‖bytes32(assetId)‖bytes32(balance))` |
| 账本持久化 | 内存账本 + WAL + 快照，抽象 `Store` 接口（v1 本地文件） |
| 冻结点 | 快速提现与强制退出互斥；退出一次性；`paused` 不阻断退出 |
```

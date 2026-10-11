# 子项目 2：服务装配 + 运行入口 + 资金安全修复 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让子项目 1 的结算子系统真正**可运行且资金安全**：把 `SettlementService` / `DepositIndexer` / `StateAnchorService` 装配进 `ServiceRegistry`、打通 `SettlementConfig`、扩展 `backend/main.go` 为可启动入口；并修掉 5 项会丢钱/重复付钱的缺陷（其中 3 项来自 PR #2 的 Bugbot 评审，此前未记录）。

**Architecture:** 不变更子项目 1 的分层。新增一个装配层 `services.BuildSettlementStack(cfg, adapter)`，把「EVMAdapter + 账本（FileStore + Recover）+ 三个服务」构造成一个可 `StartAll`/`StopAll` 的栈；`main.go` 保持现有 AMM API 不变，额外加载 `SettlementConfig` 并启动该栈。缺陷修复分别落在合约（`finalizeExit` 补防重放）、账本持久化（WAL 尾部容忍）、服务生命周期（可重启）、锚定正确性（等回执 + 事件解码）、提现记账（先扣账再上链）。

**Tech Stack:** Solidity 0.8.19（Hardhat + Chai）、Go 1.25（module `web3-amm-dex-backend`）、`github.com/ethereum/go-ethereum`、`github.com/holiman/uint256`。

**Spec:** `docs/superpowers/specs/2026-10-11-orderbook-dex-settlement-ledger-design.md`（§4 合约、§8 服务、§10 验证）
**前置分支:** `feat/stack-assembly`，基于 `master` = `6658e69`（PR #1 + PR #2 已合入）

## 基线（已实测于 `6658e69`，改动前必须仍然成立）

```
go build ./...            # exit 0
go test ./... -count=1    # ok api / config / ledger / services / tests/integration
npx hardhat test          # 31 passing
```

## Global Constraints

- Solidity 固定 `0.8.19`，沿用现有 `hardhat.config.js`；**不引入新依赖**（不装 OpenZeppelin）。
- Go module 名固定 `web3-amm-dex-backend`；金额一律 `github.com/holiman/uint256`，JSON 中为十进制字符串。
- 叶子编码与建树规则**不得改动**：`leaf = keccak256(bytes20(account) ‖ bytes32(uint256(assetId)) ‖ bytes32(balance))`；叶子按 `(account, assetId)` 升序；内部节点 `keccak256(min ‖ max)`；奇数层末尾原样提升；空根 `bytes32(0)`。
- 测试位置：Go 用同包 `_test.go`；Solidity 用 `test/*.test.js`。
- 命令在 Windows PowerShell 下执行：`&&` 不可用，用 `;` 或分条执行。
- 提交信息前缀：`feat:` / `test:` / `fix:` / `docs:` / `chore:`。
- **本轮不做**（继续延后，勿顺手实现）：`ProofAt`/历史根树、§10.3 本地链端到端、重组回滚。

## File Structure

```
contracts/
  Settlement.sol                      # T1 修改 finalizeExit
test/
  Settlement.exit.test.js             # T1 追加用例
backend/
  ledger/store.go                     # T2 修改 Replay
  ledger/store_test.go                # T2 追加用例
  ledger/ledger.go                    # T4 加 RootAndVersion()
  ledger/ledger_test.go               # T4 追加用例
  services/blockchain_adapter.go      # T4 mock 升级（按 selector 的 CallHandler、pending→mined 回执）
  services/settlement.go              # T4 CommitRoot 等回执 + 事件解码；T5 Withdraw 先扣账
  services/state_anchor.go            # T3 生命周期；T4 用新 CommitRoot
  services/deposit_indexer.go         # T3 生命周期
  services/stack.go                   # T6 新建：BuildSettlementStack
  services/stack_test.go              # T6
  services/settlement_test.go         # T4/T5 追加用例
  services/state_anchor_test.go       # T3/T4 追加用例
  services/deposit_indexer_test.go    # T3 追加用例
  config/config.go                    # T6 加结算栈所需字段（如有）
  main.go                             # T7 启动结算栈
  main_test.go                        # T7（可测的 run 辅助）
  README.md                           # T7 补充运行方式与环境变量
```

---

## 共享接口契约（本计划内固定，实现时不得擅自改名）

```go
// T4：账本需原子取得根与版本，避免 Root()/Version() 两次加锁之间的漂移
func (l *Ledger) RootAndVersion() ([32]byte, uint64)

// T4：CommitRoot 改为等回执并解码事件，返回链上确认的 rootIndex
//     （不再由调用方事后读 latestRootIndex()）
func (s *SettlementService) CommitRoot(root [32]byte) (txHash string, rootIndex uint64, err error)

// T5：提现在提交上链前先落一笔幂等 WithdrawOp；ref 由调用方提供，重试必须复用同一 ref
func (s *SettlementService) Withdraw(
    account ledger.AccountID, asset ledger.AssetID,
    balance, amount *uint256.Int, rootIndex uint64, ref string,
) (txHash string, err error)

// T6：装配入口
type SettlementStack struct {
    Adapter    BlockchainAdapter
    Store      *ledger.FileStore
    Ledger     *ledger.Ledger
    Settlement *SettlementService
    Indexer    *DepositIndexer
    Anchor     *StateAnchorService
    Registry   *ServiceRegistry
}
func BuildSettlementStack(cfg Config, adapter BlockchainAdapter) (*SettlementStack, error)
func (st *SettlementStack) Close() error

// T4：mock 需能按「合约+方法 selector」区分响应，并能模拟 pending → mined
type MockBlockchainAdapter struct {
    // 新增：
    CallHandler func(contract string, data []byte) ([]byte, error)
}
```

---

## Task 1：合约 — `finalizeExit` 补打防重放标记

**问题（资金安全，已验证）：** `withdraw` 只受 `!exited[account][assetId]` 与 `rootIndex >= lastWithdrawRoot[account][assetId]` 约束（`contracts/Settlement.sol:124,127`）。`finalizeExit` 付清冻结根上的余额后只置 `exited = true`（`Settlement.sol:173`），**不打 `lastWithdrawRoot` / `withdrawnAtRoot`**；而 `deposit` 又会把 `exited` 清回 false（`Settlement.sol:93-94`）。于是「finalizeExit → 重新充值 → 以同一冻结 rootIndex 调 withdraw」可对同一片叶子**二次领取**。

**Files:** `contracts/Settlement.sol`、`test/Settlement.exit.test.js`

- [ ] 在 `finalizeExit` 中，付清之后补打：
  `lastWithdrawRoot[msg.sender][assetId] = req.rootIndex;`
  `withdrawnAtRoot[msg.sender][assetId][req.rootIndex] += balance;`（与 `withdraw` 的记账口径一致，作纵深防御）
- [ ] 保持 `exited` / `deposit` 现有语义**不变**（重新充值后，比冻结根更新的根仍应可正常提现）。
- [ ] 先写失败测试（TDD），再改合约：
  - `finalizeExit 后重新充值、再以冻结 rootIndex 提现必须 revert`（用冻结根的合法 proof，断言 revert 原因为 `stale root`）。
  - `finalizeExit 后重新充值、以「新根」提现新入账金额应当成功`（证明修复没有把用户的新资金也锁死）。
  - 保留并复核既有 `cannot exit twice`、`exit is allowed while paused` 等用例仍通过。

**验收：** `npx hardhat test` 全绿，且新增两条用例覆盖「同根二次领取被拒」与「新资金仍可取」。

---

## Task 2：账本 — WAL 尾部半条记录必须可恢复

**问题（资金安全/可用性，已验证）：** `FileStore.Replay` 对第一行 `json.Unmarshal` 失败即整体返回错误（`backend/ledger/store.go:106-108`），而 `AppendBatch` 用单次 `Write` 写整批（`store.go:60-62`）。崩溃/短写会留下半条尾部记录 → `Recover` 永久失败；WAL 从不截断且每次从 seq 0 重放，一次中断写入即**永久卡死账本**。

**Files:** `backend/ledger/store.go`、`backend/ledger/store_test.go`

- [ ] 修改 `Replay`：**尾部不完整记录**（最后一条非空行解析失败，或文件末字节不是 `\n`）→ 记日志并忽略，正常返回已解析条目；**中间记录损坏**（其后仍有数据）→ 仍然返回错误（不能静默吞掉数据丢失）。
- [ ] 不得改变 `Replay` 的返回条目语义（仍只返回 `Seq > fromSeq` 的条目）；不得改变 `WALEntry` 的 JSON 结构（这些已写盘）。
- [ ] 测试：
  - 写入 3 条 → 把文件在第 3 条中途截断 → `Recover` **成功**且状态等价于前 2 条。
  - 中间行注入非法 JSON（其后仍有合法行）→ `Replay`/`Recover` 返回错误。
  - 末字节非 `\n`（半条）→ 成功且忽略该条。
  - 既有 `store_test.go` 用例全部保持通过。

**验收：** `go test ./ledger/ -count=1` 全绿，新增 3 条用例。

---

## Task 3：服务生命周期 — 可重启且不 panic

**问题（可用性，已验证）：** `stop` 通道只在构造函数里建一次（`deposit_indexer.go:69-...`、`state_anchor.go:40`），而 `Stop` 只用 `started` 护住 `close`（`deposit_indexer.go:88-95`、`state_anchor.go:79-86`）。`Start → Stop → Start` 后：新 goroutine 立刻从**已关闭**通道返回（服务看似在跑，实则永不轮询）；再次 `Stop` 时 `started` 又为 true → 对已关闭通道 `close` → **panic**，进程崩溃。

**Files:** `backend/services/deposit_indexer.go`、`backend/services/state_anchor.go`、各自 `_test.go`

- [ ] 两个服务统一改为：`Start` 在锁内**重建** `stop` 通道并要求处于 stopped 状态才启动（运行中的重复 `Start` 仍是幂等 no-op，沿用 Task 11 的既定裁定）；`Stop` 幂等，未启动或已停止时不得 `close`。
- [ ] 建议抽一个包内私有小类型（如 `runner`）承载「通道 + 状态 + goroutine 生命周期」，两个服务共用，避免两处各修一遍再各错一遍；若实现者认为抽公共类型反而增大改动面，可在两文件各自修，但**语义与测试必须一致**。
- [ ] 测试（两个服务都要）：
  - `Start` → `Stop` → `Start` → `Stop`：**全程不 panic**。
  - 重启后确实恢复工作：indexer 重启后能再次 `PollOnce` 到新事件；anchor 重启后能再产生一条 `RootRecord`。
  - 运行中重复 `Start` 为 no-op（不产生第二个 goroutine —— 以「一次 tick 只触发一次轮询」断言）。
  - 未启动即 `Stop`、连续两次 `Stop` 均安全。

**验收：** `go test ./services/ -count=1 -race` 全绿（生命周期用例用 `-race` 跑，捕获双 goroutine / 数据竞争）。

---

## Task 4：锚定 — 等回执、以事件解码取真实 `rootIndex`，并原子取根/版本

**问题（正确性，已验证，HIGH）：** `AnchorOnce` 调 `CommitRoot`（内部只 `SendTransaction`，拿到 mempool 返回的 hash，**不等回执、不检查是否 revert**）后立刻读 `LatestRootIndex()`（`state_anchor.go:96-102`）。真链上这会读到**提交前**的索引 → `version → rootIndex` 映射**差一**；被 `insolvent` 拒绝的提交也被记为成功。mock 完全掩盖此问题。

**关键依据：** `commitStateRoot` 会 emit `StateRootCommitted(uint256 indexed rootIndex, bytes32 root, uint64 timestamp)`（`contracts/interfaces/ISettlement.sol:7`，发射于 `contracts/Settlement.sol:111`）。因此**解码回执中的该事件**是唯一无歧义的取索引方式，且能同时证明提交真的成功。

**Files:** `backend/services/settlement.go`、`backend/services/state_anchor.go`、`backend/services/blockchain_adapter.go`（mock 升级）、`backend/ledger/ledger.go`、相关 `_test.go`

- [ ] `CommitRoot` 改为 `(txHash string, rootIndex uint64, err error)`：
  1. 打包并 `SendTransaction` 得 txHash；
  2. 轮询 `GetTransactionReceipt(txHash)` 直到非 nil 或超时（`GetTransactionReceipt` 对未知 tx 返回 `(nil, nil)`，必须视为 **pending** 而非错误）；
  3. 要求 `receipt.Status == 1`，否则报错（reverted）；
  4. 从 `receipt.Logs` 中按合约地址 + `StateRootCommitted` 的 topic0 解码 `rootIndex` 与 `root`；**校验解出的 `root` 与传入 `root` 相等**，不相等即报错；
  5. 若回执里没有该事件，报错（不退回「事后读 latestRootIndex」的旧做法）。
- [ ] 超时与轮询间隔：加 `Config` 字段（如 `ReceiptTimeoutSeconds` 默认 60、`ReceiptPollMillis` 默认 1000），`Initialize` 读取；测试里注入极小值以免拖慢。
- [ ] `AnchorOnce` 去掉事后 `LatestRootIndex()` 读取，改用 `CommitRoot` 返回的 `rootIndex`；并原子取根/版本。
- [ ] `ledger.Ledger` 新增 `RootAndVersion() ([32]byte, uint64)`（同一把锁内取两者），`AnchorOnce` 用它取代分别调用 `Root()` 与 `Version()`（消除两次加锁之间的漂移）。
- [ ] mock 升级（`MockBlockchainAdapter`）：
  - `CallContract` 现按**合约地址**返回同一响应，无法区分方法 → 新增 `CallHandler func(contract string, data []byte) ([]byte, error)`，非 nil 时优先使用；保留 `ContractResponses` 回退（既有用例不受影响）。
  - `GetTransactionReceipt` 支持「先 pending（nil, nil）若干次再返回 mined 回执」，供测试断言「确实等了回执」。
- [ ] 测试：
  - **reverted 提交（status 0）** → `AnchorOnce` 返回错误，且**不新增任何** `RootRecord` / `byVer` 条目。
  - **成功提交** → 记录的 `rootIndex` 等于回执事件里的值；构造「`latestRootIndex()` 滞后于事件」的 mock（即复现原缺陷场景），断言仍取到正确索引。
  - **回执永远 pending** → 超时报错，不记录。
  - **事件 `root` 与提交 `root` 不符** → 报错。
  - `RootIndexForVersion(version)` 在真实提交后返回正确索引（既有 Task 11 用例需按新签名调整）。

**验收：** `go test ./services/ ./ledger/ -count=1` 全绿；新增至少 4 条锚定用例，其中「latestRootIndex 滞后」用例在**旧实现下必须失败**（否则该用例没打到点）。

---

## Task 5：快速提现 — 先扣账、再上链，重试幂等

**问题（资金安全，已验证）：** `SettlementService.Withdraw` 只打包证明并发送交易，**从不写 `WithdrawOp`**（`backend/services/settlement.go:120-149`）。提现后锚定的新根仍含该余额 → 可被再次领取；偿付校验 `balanceOf >= totalDeposited - totalWithdrawn` 也不能替代账本扣账。

**用户裁定：** 扣账**先于**上链提交；提交失败则保留该笔幂等扣账记录，靠同一 `ref` 重试，而不是回滚后再可能重复提交。

**Files:** `backend/services/settlement.go`、`backend/services/settlement_test.go`

- [ ] `Withdraw` 增加 `ref string` 参数（见共享契约），步骤严格为：
  1. **先取证明**（在扣账之前！）：`proof, proven, err := s.ledger.Proof(account, asset)` —— 证明必须对应调用方给的 `rootIndex` 所代表的那一版根；校验 `proven.Cmp(balance) == 0`、`amount > 0 && amount <= balance`；
  2. `ledger.Apply(WithdrawOp{Account, Asset, Amount, Ref: ref})` —— 余额不足返回 `ErrInsufficientBalance`，此时**不得发交易**；
  3. 再 `SendTransaction`（用步骤 1 的 proof 与 `rootIndex`）。
- [ ] **顺序陷阱（必须写进注释与测试）：** 扣账会把账本推进到新版本，因此证明**必须**在扣账前取；否则证明对应的是扣账后的根，与合约校验的 `rootIndex` 根不符。
- [ ] 重复调用同一 `ref`：`WithdrawOp` 命中幂等 → 不二次扣账；仍应重新 `SendTransaction`（前一次可能未上链），返回新的 txHash。
- [ ] 测试：
  - 成功后账本余额减少且**恰好**一笔 `WithdrawOp`；下一版本的根不再包含该金额。
  - 同 `ref` 重试：不再扣账（余额不变），但确实重发了交易（mock `Sent` 长度 +1）。
  - 余额不足 → 返回 `ErrInsufficientBalance`，**未发交易**、**未扣账**。
  - `balance` 参数与账本不一致 → 报错、未扣账、未发交易。
  - `SendTransaction` 失败 → 返回错误但扣账**已持久化**；随后同 `ref` 重试不产生第二次扣账。
  - 既有 `TestSettlementServiceWithdrawBuildsProofCalldata` 等按新签名调整且断言仍非空转（沿用 Task 10 的裁定：不得退化为恒真断言）。

**验收：** `go test ./services/ -count=1` 全绿，且每条新用例都能在「扣账缺失」的旧实现下失败。

---

## Task 6：装配 — `BuildSettlementStack` + 配置桥接

**目的：** 目前没有任何东西把三个服务注册进 `ServiceRegistry`，`SettlementConfig` 也没有消费者 —— 无法启动的 v1。

**Files:** `backend/services/stack.go`（新建）、`backend/services/stack_test.go`（新建）、`backend/config/config.go`（如需补字段）

- [ ] `BuildSettlementStack(cfg Config, adapter BlockchainAdapter) (*SettlementStack, error)`：
  1. `adapter == nil` 时用 `NewEVMAdapter(cfg.RPCURL, operatorKey, cfg.ChainID)` 构造（`evm_adapter.go:24`）；测试注入 mock；
  2. `ledger.NewFileStore(cfg.DataDir)` → `ledger.Recover(store)`（**必须走 Recover**，从快照 + WAL 重建，并恢复幂等键集合）；
  3. `NewSettlementService(adapter, l, cfg.SettlementAddress)`；
  4. `NewDepositIndexer(...)`（确认数取 `cfg.RequiredConfirmations`）、`NewStateAnchorService(...)`（周期取 `cfg.AnchorInterval`）；
  5. 三者注册进新建的 `ServiceRegistry`；
  6. 返回 `SettlementStack`（含 `Registry`、`Store`）。
- [ ] `Close()`：`StopAll` 幂等 + 关闭 `FileStore`（`store.Close()`），保证 WAL 句柄释放。
- [ ] 配置桥接：提供 `SettlementConfig`（`backend/config`）→ `services.Config` 的映射（`ContractAddress→SettlementAddress`、`DataDir`、`Confirmations→RequiredConfirmations`、`AnchorIntervalSeconds→AnchorInterval`、`ChainID`）。放在 `stack.go` 的 `ConfigFromSettlement`（`services` 可 import `config`，同 module），或放 `main` 包；择一，勿两处重复。
- [ ] 注意 `services.Config` 的 `Port int` 与 `config.Config` 的 `Port string` 类型不同 —— 桥接函数不要顺手做字符串→int 的静默转换而吞掉解析错误。
- [ ] 测试（mock adapter + `t.TempDir()`）：
  - 栈构建成功后 `Registry.Get("SettlementService"/"DepositIndexer"/"StateAnchorService")` 三者均存在。
  - `InitializeAll(cfg)` + `StartAll()` + `StopAll()` 全流程不报错、不 panic。
  - 预置一个含 WAL 的 `DataDir` → 构建后账本状态被恢复（复用 `ledger.Recover` 的既有能力，不重复实现）。
  - `Close()` 后 `FileStore` 已关闭（再次写应失败），且重复 `Close()` 安全。
  - `Adapter == nil` 且缺少 operator key 时返回**明确错误**，而不是后续 nil panic。

**验收：** `go test ./services/ -count=1` 全绿；新增 `stack_test.go`。

---

## Task 7：入口 — `backend/main.go` 启动结算栈

**Files:** `backend/main.go`、`backend/main_test.go`（新建）、`backend/README.md`

- [ ] 保持现有 AMM API（gin 路由、`services.InitClient`、`ListenEvents`）**完全不变**。
- [ ] 新增：`cfg := config.LoadSettlementConfig()`；仅当 `cfg.ContractAddress != ""` 时装配并启动结算栈：
  `stack, err := services.BuildSettlementStack(services.ConfigFromSettlement(cfg), nil)` → `InitializeAll` → `StartAll`；启动日志打印合约地址、DataDir、确认数、锚定周期（**绝不打印 `OPERATOR_KEY`**）。
- [ ] 关闭路径：收到 SIGINT/SIGTERM 后 `stack.Close()`，再退出；`Close` 出错只记日志，不 `Fatal`。
- [ ] `ContractAddress` 为空 → 只跑 AMM，明确日志说明结算服务未启用（保持既有行为可用）。
- [ ] 可测性：把「读环境 → 建栈 → 启停」抽成可测函数（如 `startSettlementStack(getenv func(string) string) (io.Closer, error)`），`main()` 只做编排；`main_test.go` 覆盖：未设地址 → 返回「未启用」且不报错；设了地址但 operator key 缺失 → 返回明确错误；mock adapter 注入路径能成功启动并 `Close`。
- [ ] `backend/README.md`：补充结算栈的环境变量（`SETTLEMENT_ADDRESS`/`OPERATOR_KEY`/`CHAIN_ID`/`DATA_DIR`/`REQUIRED_CONFIRMATIONS`/`ANCHOR_INTERVAL`/`EXIT_DELAY`）、启动与关闭方式、`DATA_DIR` 是否被 `.gitignore` 覆盖。

**验收：** `go build ./...` 通过；`go vet ./...` 无新增告警；`go test ./... -count=1` 全绿。

---

## Task 8：整支回顾与收尾

- [ ] 干净检出（`git worktree`）上复跑全部验证：`go build ./...`、`go test ./... -count=1`、`go test ./services/ -race -count=1`、`npx hardhat test`。
- [ ] 逐条回到 PR #2 的 Bugbot 评论与本计划 Task 1–7，确认**每一条都有对应改动与能失败的测试**，无一条被静默跳过。
- [ ] 复核**未引入**本轮范围外的改动（`ProofAt`、§10.3 端到端、重组回滚仍为显式延后）。
- [ ] 更新 `docs/superpowers/specs/2026-10-11-orderbook-dex-settlement-ledger-design.md` 中因本轮改动而失准的段落（`CommitRoot` 返回值、`Withdraw` 的 `ref`、装配入口），保持设计与实现一致。
- [ ] 写整支回顾报告（`final-review.md` 风格），逐条给出「设计覆盖 / 跨任务集成 / 干净检出证据 / 仍未解决项」。
- [ ] 开 PR（base `master`），把 Bugbot 评论里三条的修复逐条对应到 PR 描述。

**验收：** 干净检出 4 项命令全绿；回顾报告列出每条修复及其「旧实现下会失败的测试」。

---

## 风险与注意事项

1. **Task 4 与 Task 5 都动 `settlement.go`，Task 3 与 Task 4 都动 `state_anchor.go`** —— 建议串行执行 T3 → T4 → T5，避免同一文件的并发改动互相覆盖（子项目 1 已发生过一次多智能体同文件冲突）。
2. **Task 5 的证明时序是最易写错的一点**：任何「先扣账、后取证明」的实现都会让证明与合约校验的根不匹配，且有可能被测试漏掉（因为 mock 不回验证明）。必须显式断言「证明是在扣账前对旧根取的」。
3. **Task 4 的 mock 升级会触及既有锚定/索引器用例** —— 保留 `ContractResponses` 回退路径，避免大面积改写既有测试。
4. 本轮不引入任何新外部依赖；若发现必须引入，先停下来问。

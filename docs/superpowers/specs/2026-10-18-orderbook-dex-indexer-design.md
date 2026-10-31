# 子项目 5 设计：链上批量结算 + 索引器

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：子项目 5（索引器 + 状态重建 + 历史查询）。
> **前置**：子项目 1（结算合约 + 链下账本）、2（服务装配）、3（撮合引擎）、4（网关）均已合入 `master`。
> **日期**：2026-10-18
> **状态**：已裁定关键设计，待实现

---

## 1. 背景与目标

### 1.1 承接关系

子项目 1 交付了链上金库 `Settlement.sol`（充值 / `commitStateRoot` 锚定 / 快速提现 / 强制退出）与链下
账本；子项目 3 交付了纯确定性撮合引擎，成交在**链下账本内用零和 `TransferOp` 结算**，从不触碰链上合约；
子项目 4 交付了 HTTP/WebSocket 网关作为对外入口。

但整套系统缺少**可观测的历史**：成交明细、链上资金进出、状态根演进都没有一个可查询、可持久化的记录；
也缺少一个**独立于运行中主账本的审计能力**，来证明链上金库与链下账本的余额总量一致。

本子项目补上这一环：一个索引器，订阅撮合成交事件 + 轮询链上资金事件，持久化到嵌入式 KV 库，提供只读
历史查询；并提供从链上事件独立重建、再与主账本对账的能力。

### 1.2 子项目路线图（依赖驱动，编号已按实际执行顺序重排）

| # | 子项目 | 内容 | 依赖 | 状态 |
|---|--------|------|------|------|
| 1 | 结算合约 + 链下账本 | 链上托管金库、状态根锚定、强制退出；链下账本 | — | ✅ 已合入 |
| 2 | 服务装配 + 资金安全修复 | `BuildSettlementStack` + `main` 入口；WAL 尾损、锚定取索引、提现钉住 | 1 | ✅ 已合入 |
| 3 | 链下订单簿与撮合引擎 | 内存订单簿、限价/市价、部分成交、资金冻结 | 1,2 | ✅ 已合入 |
| 4 | 排序器 + 网关 + 状态机 | 收单/鉴权/限流、WebSocket 推送；下单→撮合→更新账本 | 3 | ✅ 已合入 |
| **5** | **索引器 + 状态重建**（本文档） | 成交/资金/根历史索引、历史查询、从链上事件重建与对账 | 3,4 | 进行中 |
| 6 | 永续合约 + 风险引擎 | 仓位、保证金、杠杆、标记价格、资金费率、强平 | 1..5 | — |
| 7 | 前端 + 行情 | 交易界面、深度图、K 线 | 4,5 | — |

### 1.3 本子项目目标

1. 一个**索引器**，订阅 `EventBus` 的成交事件实时写入成交历史，轮询链上 `Deposit` / `Withdrawal` /
   `ExitFinalized` / `StateRootCommitted` 事件写入资金与根历史。
2. 历史持久化到**嵌入式 KV（bbolt）**，重启后历史不丢，且增量更新。
3. 提供**三类只读历史查询**：成交历史（按市场/账户/时间）、链上资金历史（充值/提现/退出）、状态根历史。
4. 提供**独立状态重建 + 对账**：从链上资金事件重放到一个独立 `ledger.Ledger` 实例，比对每资产总量与主
   账本一致，验证链上金库与链下账本资金守恒。
5. 不改 `Settlement` 合约：结算仍靠状态根锚定，本子项目是纯链下可观测性/审计层。

### 1.4 非目标（明确不做，留给后续子项目）

永续/保证金/杠杆/资金费率/强平（子项目 6）；前端行情/K 线/深度图（子项目 7）；成交历史走 WebSocket
实时推送（v1 只做 HTTP 只读）；逐账户的对账（见 §2.2）；对账的定时自动触发；SQL 级查询语言；
对链上 `Settlement` 合约的任何改动。

---

## 2. 关键裁定与信任边界

### 2.1 本子项目裁定（已与 owner 确认）

| 议题 | 结论 | 理由 |
|------|------|------|
| 结算模型 | 索引器 + 状态重建为主，**不改 Settlement 合约** | 结算已由状态根锚定承载；本子项目是纯观测/审计层 |
| 历史范围 | 三类：成交、链上资金事件、状态根 | 覆盖对账与查询的核心面 |
| 存储 | 嵌入式 KV（bbolt） | 重启持久、增量更新；与现有文件/WAL 风格一致 |
| 成交来源 | 订阅 `EventBus` 的 `orderbook.trade` 实时写入 | 简单、实时、与撮合同生命周期 |
| 状态重建 | 独立重建 + 对账 | 审计能力，不替换运行中的主账本 |
| 对外形态 | HTTP 只读路由（复用 gin） | 与子项目 4 读接口风格一致，不鉴权 |
| 对账触发 | HTTP 手动触发 + 纯函数 `RebuildAndCompare` | v1 不强制定时自动 |

### 2.2 v1 保证

1. **历史可查**：成交、资金、根三类历史持久化，重启不丢，可按市场/账户/时间区间查询。
2. **总量对账**：从链上资金事件重建的每资产 `Σ余额` 与主账本一致 ⇒ 链上金库与链下账本资金守恒。
3. **幂等**：索引写入按唯一键（成交 `trade:<id>`、链上事件 `txHash:logIndex`）去重，重复处理不产生重复历史。
4. **不改合约、不改撮合内核**：本子项目只新增观测/审计层，对既有资金安全路径零侵入（除 §5 的一处
   事件补发，见下）。

### 2.3 残余信任假设（必须在 README 与文档中显式声明）

1. **重建不含撮合腿**：撮合是零和 `TransferOp`，链上事件里没有它的痕迹。因此重建出的账本**每账户余额
   与主账本不同**，只有**每资产总量**可比对。逐账户对账需要每笔成交的 Merkle 证明上链（子项目 5 的
   "成交批次证明上链"备选模型，已裁定不做），留待后续。
2. **对账只检测总量漂移，不定位原因**：一旦总量不一致，说明充值/提现/退出索引有缺口或账本有 bug，但
   本子项目不自动修复，只报告差异。
3. **成交历史的实时性依赖事件投递**：`EventBus.Publish` 是同步内联的（子项目 3），索引器在事件处理里写
   KV。若索引器写失败，当前撮合路径**不回滚成交**（成交本身已安全结算），只记录索引器降级状态；历史
   可由重启 + WAL 回填补齐（见 §5）。

---

## 3. 架构概览

```
            撮合成交（链下，零和）
                │ EventBus orderbook.trade
                ▼
┌────────────────────────────────────────────────────────┐
│  IndexerService（新包 backend/indexer）                  │
│  ├─ onTrade：写成交历史（幂等键 trade:<id>）             │
│  ├─ pollEvents：轮询链上 Deposit/Withdrawal/ExitFinalized│
│  │              /StateRootCommitted → 写资金/根历史       │
│  ├─ Store（bbolt）：三类历史 + 游标                      │
│  └─ Rebuild：从链上事件重放到独立 ledger → 对账总量       │
└──────────────┬─────────────────────────────────────────┘
               │ Query API
               ▼
   HTTP 只读路由（挂到 gateway /v1/...）
```

分层约束：`indexer` 依赖 `services.EventBus`（读事件）、`services.BlockchainAdapter`（`GetLogs` 读链上事件）、
`ledger`（重建）、`matching`（成交类型）。它**不** import `services.OrderBookService` 的内部，**不** import
`gateway`（路由由 gateway 侧装配时调用 `indexer` 的查询 API）。

---

## 4. 存储 `backend/indexer/store.go`

使用 `go.etcd.io/bbolt`（新增依赖）。一个 bbolt 文件，三个 bucket + 游标：

```
bucket "trades"        key = trade:<tradeID>            value = TradeRecord JSON
bucket "fund_events"   key = <txHash>:<logIndex>         value = FundEvent JSON
bucket "root_events"   key = root:<rootIndex>            value = RootEvent JSON
bucket "cursor"        key = "chain_events"              value = 上次轮询到的区块高度
```

```go
type Store struct { db *bbolt.DB }

func OpenStore(path string) (*Store, error)          // 打开/创建，建 bucket
func (s *Store) Close() error

func (s *Store) PutTrade(t TradeRecord) error          // 幂等：同 key 覆盖（trade id 确定性）
func (s *Store) PutFundEvent(e FundEvent) error        // 幂等：同 txHash:logIndex 覆盖
func (s *Store) PutRootEvent(e RootEvent) error        // 幂等：同 rootIndex 覆盖
func (s *Store) LastProcessedBlock() (uint64, error)   // 读游标
func (s *Store) SetLastProcessedBlock(h uint64) error  // 写游标

func (s *Store) QueryTrades(market, account, from, to, limit) ([]TradeRecord, error)
func (s *Store) QueryFundEvents(account, asset, limit) ([]FundEvent, error)
func (s *Store) QueryRoots(from, to, limit) ([]RootEvent, error)
```

- 幂等靠**确定性键**：成交 `trade:<id>`（子项目 3 保证 `TradeID` 重放可复现）、链上事件 `txHash:logIndex`。
- `TradeRecord` 保存：`TradeID, Market, Price, Qty, Notional, Maker, Taker, MakerSide, BlockHeight?`（成交在
  链下，无区块高度，记 `Seq` 或写入时间戳供排序）。资金/根事件保存链上事件的全部可解析字段 + 区块号。
- 游标 `chain_events` 记录链上事件轮询的进度（已处理区块高度），重启后从游标继续，避免重复/遗漏。
- 查询按 key 前缀扫描；时间区间对成交用写入顺序（`TradeID` 单调）过滤，对资金事件用区块号过滤。

### 4.1 记录类型

```go
type TradeRecord struct {
    TradeID   uint64
    Market    uint32
    Price     uint64
    Qty       string // 十进制字符串
    Notional  string
    Maker     string
    Taker     string
    MakerSide string
}

type FundEvent struct {
    Kind      string // deposit | withdrawal | exit
    TxHash    string
    LogIndex  uint
    Block     uint64
    Account   string
    Asset     uint32
    Amount    string
    Seq       uint64 // deposit 专用
    RootIndex uint64 // withdrawal/exit 专用
}

type RootEvent struct {
    RootIndex uint64
    Root      string
    Timestamp uint64
    Block     uint64
}
```

金额一律用十进制字符串（与 `ledger`/`gateway` 一致），`uint256` 不外露为 JSON 数字。

---

## 5. 索引器 `backend/indexer/indexer.go`

实现 `services.Service`（`Name/Initialize/Start/Stop`），沿用子项目 2/3 的 `runner` 约定（可重复
Start/Stop 不 panic）。

```go
type IndexerService struct {
    store   *Store
    bus     *services.EventBus
    adapter services.BlockchainAdapter
    chain   string        // Settlement 合约地址（订阅事件用）
    interval time.Duration
    run     runner
}

func New(store *Store, bus *services.EventBus, adapter services.BlockchainAdapter, chain string) *IndexerService
func (s *IndexerService) Name() string { return "IndexerService" }
func (s *IndexerService) Initialize(cfg Config) error
func (s *IndexerService) Start() error   // 启动成交订阅 + 事件轮询 goroutine
func (s *IndexerService) Stop() error
```

### 5.1 成交订阅

- `Start` 里 `bus.Subscribe(services.TopicOrderBookTrade, s.onTrade)`。
- `onTrade`：`TradeEvent{Trade}` → 构造 `TradeRecord` → `store.PutTrade`。幂等键 `trade:<tradeID>`。
- 写入失败：记录日志并置 `degraded` 标志（不 panic、不回滚成交）。

### 5.2 链上事件轮询

- 复用 `BlockchainAdapter.GetLogs(fromBlock, toBlock, address, topics)`（子项目 2 已实现）。
- 四个事件 topic 由签名哈希计算（`Deposit(address,uint32,uint256,uint256)`、
  `Withdrawal(address,uint32,uint256,uint256)`、`ExitFinalized(address,uint32,uint256,uint256)`、
  `StateRootCommitted(uint256,bytes32,uint64)`）。
- 从 `store.LastProcessedBlock()+1` 起，按确认数（复用 `RequiredConfirmations`）回退到安全高度，拉取日志，
  逐条解析 → `store.PutFundEvent` / `store.PutRootEvent`，最后 `SetLastProcessedBlock`。
- 解析与 `deposit_indexer.go` 的 `decodeDeposit` 同风格：地址从 `topics[1]`，`assetId` 从 `topics[2]`，金额从
  `data[0:32]`。

### 5.3 重启回填（关键：需小改 order_book.go）

成交历史靠事件实时写入，但 `OrderBookService.recover` 重启重放指令 WAL 时重算出确定性成交，**当前不发
事件**，索引器会漏掉这些成交。

**裁定：小改 `services/order_book.go` 的 `recover`，在重放每笔成交后补发 `orderbook.trade` 事件。** 重放
本来已经产生了相同的 `Trade`（相同 `TradeID`），补发只是让订阅者跟上；事件幂等键 `trade:<id>` 保证索引器
即使重复收到也不产生重复历史。这是最小、最干净的改法，不改变撮合语义与资金安全路径。

> 具体：`recover` 里 `res, err := s.engine.Place(e.Place.Request())` 成功后，对 `res.Trades` 逐笔
> `s.bus.Publish(Event{Topic: TopicOrderBookTrade, Data: TradeEvent{Trade: t}})`（与 `publish` 一致）。

---

## 6. 状态重建与对账 `backend/indexer/rebuild.go`

```go
// Rebuild replays the settlement contract's funding events into a fresh ledger,
// yielding per-asset total balances. It is read-only: it never touches the
// running ledger.
func Rebuild(adapter services.BlockchainAdapter, chain string, fromBlock uint64) (*ledger.Ledger, error)

// CompareTotals compares the rebuild's per-asset totals against the running
// ledger's per-asset totals, returning per-asset discrepancies.
func CompareTotals(rebuilt, running *ledger.Ledger) ([]AssetDelta, error)

type AssetDelta struct {
    Asset   ledger.AssetID
    Rebuilt string
    Running string
    Match   bool
}
```

- `Rebuild`：`GetLogs` 拉取 `Deposit`/`Withdrawal`/`ExitFinalized` 事件，`Deposit` → `DepositOp`，
  `Withdrawal`/`ExitFinalized` → `WithdrawOp`，按事件顺序 `Apply` 到 `ledger.New()`（幂等键 `txHash:logIndex`）。
- 撮合腿（零和）不进重建，故只有**每资产总量**有意义。
- `CompareTotals`：对每个资产，比对 `rebuilt.TotalBalance(asset)` 与 `running.TotalBalance(asset)`。
  不一致说明充值/提现/退出索引有缺口或账本 bug，只报告、不修复。
- 触发：HTTP 端点 `/v1/reconcile`（或 `/v1/admin/reconcile`）手动调用；也可在测试/运维脚本直接调纯函数。

### 6.1 对账口径（重要）

`commitStateRoot` 的偿付校验是 `balanceOf(vault) >= totalDeposited - totalWithdrawn`，即**每资产总量**。
重建 + 对账复用的正是这个口径：验证链上金库与链下账本总量守恒。**不做逐账户对账**（需成交 Merkle 证明
上链，已裁定不做）。

---

## 7. 查询 API（HTTP 只读路由）

挂到现有 gateway 的 `/v1` 组（读接口不鉴权，与子项目 4 一致）：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/v1/trades?market=&account=&limit=` | 成交历史，按市场/账户过滤 |
| GET | `/v1/fund-events?account=&asset=&limit=` | 充值/提现/退出历史 |
| GET | `/v1/roots?limit=` | 状态根历史 |
| POST | `/v1/reconcile` | 触发重建 + 对账，返回每资产差异 |

- 查询走 `indexer.Store` 的查询方法，gateway 侧 handler 只做参数解析 + JSON 序列化。
- `reconcile` 返回 `{ assets: [{asset, rebuilt, running, match}], consistent: bool }`。

---

## 8. 服务装配

- `backend/indexer` 暴露 `New(...)`；`services/stack.go` 或 `main.go` 在订单簿存在时装配 `IndexerService`
  并注册进 `ServiceRegistry`。
- `main.go`：把索引器的查询 API 挂到 gateway 路由。
- 配置新增：

| 键 | 含义 | 默认 |
|----|------|------|
| `INDEXER_DB_PATH` | bbolt 文件路径（空则内存/临时，或禁用持久化） | 空 |
| `INDEXER_POLL_INTERVAL` | 链上事件轮询间隔（秒） | 5 |

- 依赖：`go.etcd.io/bbolt`。

---

## 9. 错误处理与边界

- **索引写入失败**：记录日志 + 置降级标志，不 panic、不回滚成交；成交本身已安全结算，历史由重启回填补齐。
- **链上事件轮询失败**：按间隔重试，游标不回退（下次从上次成功处继续），记录日志。
- **重建失败**：`/v1/reconcile` 返回 500 + detail；不影响运行中的主账本。
- **对账不一致**：返回 200 + `consistent:false` + 每资产差异，不自动修复。
- **游标回退**：链重组时游标需回退重扫；v1 沿用子项目 2 的确认数方案，以确认数为界避免重组影响。

---

## 10. 测试策略

### 10.1 单元测试（Go，表驱动）

- **Store**：`PutTrade`/`PutFundEvent`/`PutRootEvent` 幂等（同 key 覆盖）；游标读写；三类查询按 key 过滤。
- **索引器**：`onTrade` 写成交（幂等）；事件轮询按游标推进、按确认数回退；解析四个链上事件。
- **重建**：给定事件序列 → `Rebuild` 产出的总量与手算一致；`CompareTotals` 对一致/不一致分别返回正确结果。

### 10.2 集成测试

- **端到端**：充值 → 撮合成交 → `onTrade` 写历史 → 查询成交历史返回该笔；重启（重放 WAL + 补发事件）后
  历史仍在且不重复。
- **对账**：用 `MockBlockchainAdapter` 注入充值/提现事件，`Rebuild` 后与主账本 `CompareTotals` 一致；人为
  制造缺口后不一致。

### 10.3 回归

- 既有 Go + Solidity 全量测试必须保持绿色（尤其 `order_book.go` 的 `recover` 补发事件不得破坏重放测试）。

---

## 11. 文件布局

```
backend/
  indexer/
    store.go, store_test.go        # bbolt 封装 + 游标 + 查询
    indexer.go, indexer_test.go    # IndexerService（订阅 + 轮询）
    rebuild.go, rebuild_test.go    # 独立重建 + 对账
    types.go                       # TradeRecord/FundEvent/RootEvent/AssetDelta
  services/order_book.go           # 改：recover 补发 trade 事件
  services/stack.go                # 改：装配 IndexerService（可选，或由 main 装配）
  gateway/handlers.go              # 改：新增只读查询 + reconcile 路由
  config/config.go                 # 改：INDEXER_* 配置
  main.go                          # 改：装配索引器 + 挂路由
  go.mod                           # 改：+ go.etcd.io/bbolt
docs/superpowers/specs/2026-10-18-orderbook-dex-indexer-design.md   # 本文档
docs/superpowers/plans/2026-10-18-orderbook-dex-indexer.md          # 实施计划
```

---

## 12. 已定决策记录

| 决策 | 结论 |
|------|------|
| 结算模型 | 索引器 + 状态重建为主，不改 Settlement 合约 |
| 历史范围 | 三类：成交、链上资金事件、状态根 |
| 存储 | bbolt（嵌入式 KV），幂等键 trade:<id> / txHash:logIndex / root:<index> |
| 成交来源 | 订阅 EventBus orderbook.trade 实时写入 |
| 重启回填 | 小改 order_book.go recover 补发 trade 事件 |
| 状态重建 | 独立重建 + 每资产总量对账（不含撮合腿） |
| 对账触发 | HTTP 手动 + 纯函数 RebuildAndCompare |
| 对外形态 | HTTP 只读路由，不鉴权 |
| 依赖 | 新增 go.etcd.io/bbolt |
| 本轮未做（显式延后） | 逐账户对账、成交 Merkle 证明上链、WebSocket 推送、定时自动对账、永续/风险、前端 |

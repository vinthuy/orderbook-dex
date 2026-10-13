# 子项目 3 设计：链下订单簿与撮合引擎

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：子项目 3（链下订单簿 + 撮合引擎 + 资金冻结），即交易所内核。
> **前置**：子项目 1（结算合约 + 链下账本）、子项目 2（服务装配 + 资金安全修复）均已合入 `master`。
> **日期**：2026-10-13
> **状态**：已裁定关键设计，待实现

---

## 1. 背景与目标

### 1.1 承接关系

子项目 1 交付了"地基"：链上金库（充值/锚定/快速提现/强制退出）与链下双重记账账本
（`backend/ledger`：`DepositOp/WithdrawOp/TransferOp`、原子 `Apply`、确定性 Merkle 根、WAL + 快照）。
子项目 2 把它装配成可运行的服务栈，并修掉了两条资金安全缺陷。

本子项目在账本之上构建**订单簿与撮合**：用户下单 → 撮合成交 → 账本内两条零和转移
（base 与 quote）完成结算。这是"交易"第一次真正发生。

### 1.2 子项目路线图（依赖驱动，编号已按实际执行顺序重排）

| # | 子项目 | 内容 | 依赖 | 状态 |
|---|--------|------|------|------|
| 1 | 结算合约 + 链下账本 | 链上托管金库、状态根锚定、强制退出；链下账本 | — | ✅ 已合入 |
| 2 | 服务装配 + 资金安全修复 | `BuildSettlementStack` + `main` 入口；WAL 尾损、锚定取索引、提现钉住 | 1 | ✅ 已合入 |
| **3** | **链下订单簿与撮合引擎**（本文档） | 内存订单簿、限价/市价、部分成交、资金冻结 | 1,2 | 进行中 |
| 4 | 排序器 + 网关 + 状态机 | 收单/鉴权/限流、WebSocket 推送；下单→撮合→更新账本的对外入口 | 3 | — |
| 5 | 链上批量结算 + 索引器 | 批量提交成交结算上链、状态重建、历史查询 | 3,4 | — |
| 6 | 永续合约 + 风险引擎 | 仓位、保证金、杠杆、标记价格、资金费率、强平 | 1..5 | — |
| 7 | 前端 + 行情 | 交易界面、深度图、K 线 | 4,5 | — |

> 原路线图中 2 = 撮合引擎。实际执行时把"服务装配 + 资金安全修复"插到了撮合之前（否则撮合
> 引擎没有一个可运行的宿主），故撮合顺延为 3，其后顺延。

### 1.3 本子项目目标

1. 一个**纯确定性撮合内核**（无 I/O、不读系统时钟、不依赖随机）：给定相同的市场配置与相同的
   指令序列，必然产出相同的订单 ID、成交 ID、成交明细与最终订单簿。
2. 支持**限价 GTC**、**市价 IOC**、**撤单**、**部分成交**；价格-时间优先。
3. **下单即冻结资金**，使撮合结果必然可结算（账本转移不可能因余额不足失败）。
4. 成交结算通过账本**两条零和转移**完成；成交以确定性 ID 作幂等键，崩溃后重放可安全重算。
5. 订单簿具备**自己的 WAL 指令日志**，重启后重建订单簿与冻结额；账本按成交幂等键收敛。
6. 提现只能提取"可用余额"（= 账本余额 − 冻结），冻结资金不可被提走。

### 1.4 非目标（明确不做，留给后续子项目）

HTTP/WebSocket API、鉴权、限流（子项目 4）；链上批量结算与索引器（子项目 5）；止损/止盈、post-only、
FOK、reduce-only、冰山/时间加权（后续）；手续费与返佣（本子项目裁定为 v1 不做）；保证金/杠杆/永续
（子项目 6）；行情推送与深度聚合（子项目 7）；订单簿快照的增量压缩、撮合性能优化（基准后再说）。

---

## 2. 关键裁定与信任边界

### 2.1 本子项目裁定（已与 owner 确认）

| 议题 | 结论 | 理由 |
|------|------|------|
| 资金冻结位置 | **冻结在撮合引擎侧**，账本零改动；Merkle 根仍发布**总额** | 账本与叶子编码跨语言冻结（合约 + Go 一致性测试），不改是最大保护；且根发布总额使**强制退出仍能取走全部**，头部保证不被削弱 |
| 冻结的持久化 | 订单簿自带**指令 WAL**，重启重放重建订单簿与冻结额 | 冻结内存化却仍持久可恢复，二者兼得 |
| 提现口径 | `可用 = 账本余额 − 冻结`；提现只能提可用 | 冻结资金不可被提走，否则成交无法结算 |
| v1 订单类型 | 限价 GTC + 市价 IOC + 撤销 | 最小可用闭环 |
| 手续费 | **v1 零手续费** | 避免舍入与资金泄漏面；手续费是纯增量，后续可加 |
| 自成交 | **禁止**，撤掉较旧（挂单）订单，入单继续撮合 | 避免自成交刷量与无意义资金位移 |
| 舍入 | `notional = floor(qty × price / 10^baseDecimals)`；买入冻结取 `ceil` | 双腿同一公式 ⇒ 精确零和；冻结取 ceil ⇒ 必然覆盖 floor 扣减 |
| 结算顺序 | **先做账本转移，再释放冻结**；绝不反向 | 反向会在窗口内虚增可用余额，提现可与转移竞争导致转移失败（半成交） |
| 日志顺序 | **先落指令 WAL，再改订单簿，再应用账本** | 指令日志是唯一真相源；账本按成交幂等键收敛，崩溃后重放即自愈 |
| 账本应用失败 | 服务进入**致命降级**：拒绝后续下单/撤单，需重启由重放对账 | 避免"订单簿已成交、账本未记"期间可用余额被高估 |

### 2.2 v1 保证

1. **可撮合性**：下单通过冻结校验后，该订单任何成交量都必然能在账本内结算（余额不足不再是撮合期错误）。
2. **守恒**：成交只产生零和转移；任意资产 `Σ余额` 只受充值与提现影响，撮合不改变它。
3. **确定性**：同配置 + 同指令序列 ⇒ 同结果；`Root()` 亦然（沿用子项目 1 的账本根）。
4. **崩溃自洽**：重启重放指令日志后，订单簿、冻结额、账本三者收敛到同一状态。
5. **提现不越权**：冻结资金不可被提走。

### 2.3 残余信任假设（必须在 README 与文档中显式声明）

1. **强制退出与挂单**：用户走强制退出取走的是**总额**（根含冻结资金）。运营方**必须**在观察到该
   用户的 `requestExit` 后撤销其挂单；否则订单簿内存在"资金已离开金库"的订单，成交后账本会
   记出一笔金库无法覆盖的余额。v1 以"运营方响应退出事件撤单"兜底；更彻底的做法（订单 TTL /
   退出即自动撤单）列为后续。
2. **运营方可撤单/可停摆**：撮合在链下，运营方可以拒绝服务、撤单、暂停撮合。这与子项目 1 的
   信任模型一致（运营方不可作恶致用户**资金损失**，但可以拒绝服务）。
3. **指令日志是单点**：订单簿 WAL 若整体损坏，冻结额无法重建（账本不受影响，总额仍可退出）。
   运维上应备份；后续可换 bbolt/Postgres 而不改内核。

---

## 3. 架构概览

```
                 services/order_book.go  (OrderBookService)
                 ┌──────────────────────────────────────────────┐
  下单/撤单 ────►│ 1 校验 + 冻结(ledger.Holds)                   │
                 │ 2 落指令 WAL（唯一真相源）                     │
                 │ 3 engine.Place/Cancel（纯确定性，无可失败路径） │
                 │ 4 ledger.Apply(零和转移 ×2/笔成交)             │
                 │ 5 释放冻结残额                                 │
                 │ 6 EventBus 发布 trade/order 事件               │
                 └───────────┬──────────────────┬───────────────┘
                             │                  │
              matching (纯内核)              ledger
              ┌──────────────────┐      ┌─────────────────────┐
              │ Engine / book    │      │ Ledger（不改）       │
              │ 价格-时间优先     │      │ TransferOp 零和      │
              │ 限价/市价/撤单    │      │ WAL + 快照 + Merkle  │
              │ STP 撤旧单        │      ├─────────────────────┤
              │ 冻结额推导        │      │ Holds（新增）        │
              └──────────────────┘      │ 可用 = 余额 − 冻结   │
                                        └─────────────────────┘
```

分层的核心约束：**`matching` 不 import 任何 I/O**（不 import `services`，不读时钟、不用随机、不碰
账本）。它只做"给定状态 + 指令 → 结果"。所有副作用（冻结、账本、事件、持久化）都在服务层。

---

## 4. 撮合内核 `backend/matching`

### 4.1 类型

```go
type Side   uint8 // Buy, Sell
type Type   uint8 // Limit, Market
type TIF    uint8 // GTC, IOC
type Status uint8 // New, PartiallyFilled, Filled, Cancelled, Rejected

type OrderID uint64 // 引擎分配，从 1 单调递增（重放可复现）
type Seq     uint64 // 价格-时间优先的时间序，从 1 单调递增
type Price   uint64 // quote 最小单位 / 1 个整 base 单位
type TradeID uint64 // 成交序号，从 1 单调递增（重放可复现）
```

- `Qty`/`Amount` 用 `*uint256.Int`（与账本一致）。
- `Price` 用 `uint64`：既是价格级 map 的可比较键，也足够表达任何现实价格。**价格方向约定**：
  市场必须以"1 个整 base 单位至少值 1 个 quote 最小单位"的方向配置（即 `price ≥ 1`）。若某
  token 单价低于一个 quote 最小单位（如 1e-9 USDT），应把该市场反向报价（例如以"每 1e9 枚"
  为 base 单位）。`NewEngine` 对 `price == 0` 的挂单直接拒绝。

### 4.2 市场配置

```go
type MarketID uint32

type Market struct {
    ID                    MarketID
    Symbol                string
    Base, Quote           ledger.AssetID
    BaseDecimals          uint8  // floor 基数：notional = qty*price/10^BaseDecimals
    MinQty                *uint256.Int // 可空；最小下单量（base）
}
```

`Market.Validate()` 校验：Base ≠ Quote、`10^BaseDecimals` 可表达、`MinQty` 非负。
`NewEngine(markets []Market)` 拒绝重复 `ID`/`Symbol` 与非法市场。

### 4.3 订单与成交

```go
type Order struct {
    ID        OrderID
    Seq       Seq       // 入簿序（时间优先依据）
    Market    MarketID
    Account   ledger.AccountID
    Side      Side
    Type      Type
    TIF       TIF
    Price     Price            // 限价单必填；市价单为 0（无价格约束）
    Qty       *uint256.Int     // base 数量（两类订单一致）
    MaxNotional *uint256.Int   // 市价买必填：最多花费的 quote；限价买可空
    Filled    *uint256.Int     // 已成交 base 数量
    Status    Status
    reserved  *uint256.Int     // 内部：当前仍冻结的金额（买=quote，卖=base）
}

type Trade struct {
    ID          TradeID
    Market      MarketID
    Price       Price
    Qty         *uint256.Int // 本次成交 base 数量
    Notional    *uint256.Int // floor(Qty*Price/10^BaseDecimals)
    MakerOrder  OrderID
    TakerOrder  OrderID
    Maker, Taker ledger.AccountID
    MakerSide   Side
}
```

- `Order.remaining() = Qty − Filled`；`IsOpen()` = Status ∈ {New, PartiallyFilled}。
- 成交 ID 是**成交序号**（`TradeID`），重放可复现；账本幂等键由此派生（见 6.3）。

### 4.4 冻结额推导（内核负责，服务层只做搬运）

内核是"每张挂单当前应冻结多少"的唯一真相源：

- **限价买**：入簿时 `reserved = ceil(Qty × Price / 10^BaseDecimals)`；每笔成交扣掉
  `该笔 Notional`；离簿（全成/撤销）时残额即释放。
- **限价卖**：`reserved = Qty`；每笔成交扣掉该笔 `Qty`。
- **市价买**：`reserved = MaxNotional`；每笔成交扣掉该笔 `Notional`。
- **市价卖**：`reserved = Qty`；每笔成交扣掉该笔 `Qty`。

**离簿必释放（子项目 3 实现期发现并修复的泄漏）**：任何订单离开订单簿（全成、撤销、STP 被撤）
时，都必须把 `reserved` 的**残额**一次性释放。买入冻结取 `ceil` 而每笔成交按 `floor` 扣除，两者
之差是整数取整残额；若订单全成离簿时不释放，它会被**永久冻结**（`Available` 永久少算，且
`OpenReserves` 已不再包含它，重放对账也修不回来）。注意释放必须发生在**扣减之后**：先释放再扣减
会把 `reserved` 扣到 0 再减去成交额而**下溢**。该泄漏在随机指令流下表现为每笔全成买单最多 1 个
quote 最小单位，量小但永久、且累积。

**市价买预算不足的边界**：`MaxNotional` 必须 `> 0`。若预算小于"在最优价可成交的最小金额"
（即在当前最优价上裁量后 `floor(剩余预算 × 10^BaseDecimals / Price) == 0`），则**零成交、残部撤销、
释放全部 `MaxNotional`**——不拒绝。理由：是否"必然零成交"取决于订单簿当前状态（是竞态，不是请求
本身的属性），把状态相关的判断塞进请求校验会让结果依赖时序；统一为"IOC 未成交即撤销"更简单，
且不损失任何资金。

`Qty > 0`、`MinQty` 校验在入簿前完成（`RequiredReserve` 阶段即可拒绝，不入簿）。
`RequiredReserve(req)` 计算首笔冻结额（纯函数，不改状态）：
`Limit buy → (Quote, ceil(Qty*Price/10^BaseDecimals))`；`Market buy → (Quote, MaxNotional)`；
`sell → (Base, Qty)`。

### 4.5 价格-时间优先与数据结构

每个市场一本 `book`：

```go
type book struct {
    m      Market
    bids   *side // 价格降序
    asks   *side // 价格升序
    orders map[OrderID]*Order
}

type side struct {
    prices []Price                 // 最优价在前，二分维护
    levels map[Price]*level        // 价格级
}

type level struct {
    price Price
    total *uint256.Int   // 该级剩余挂单量合计（不变量校验用）
    fifo  *list.List     // *Order，「时间优先」= 表内先后
}
```

- 每单记住自己的 `*list.Element` 以便 **O(1) 撤单**；撤单时同步维护 `level.total`，
  级空则删除价格级并从 `prices` 移除。
- 撮合总是取最优价级、级内取表头（最早入簿）。
- **同价同级严格 FIFO**；跨级按价格优先。

### 4.6 撮合语义

`Engine.Place(req)`：

1. 校验：市场存在、`Qty > 0`、`Qty ≥ MinQty`、限价单 `Price > 0`、市价买 `MaxNotional > 0`、
   `Market + GTC` 非法、限价买若给了 `MaxNotional` 则须 `≥ ceil(Qty*Price/10^BaseDecimals)`。
   任何失败返回 `Rejected`（实现上返回 `ErrBadRequest`）且**不改状态、不产生冻结、`Releases` 为空**。
   校验**不得**依赖订单簿状态（例如"预算是否够成交一笔"），否则同一请求的结果会随时序变化。
2. 分配 `ID`、`Seq`；置 `Status = New`。
3. **撮合循环**：反复取对手方最优价级：
   - **价格约束**：限价买要求 `最优卖价 ≤ 本单 Price`；限价卖要求 `最优买价 ≥ 本单 Price`；
     市价单无价格约束（买另有 `MaxNotional` 约束）。
   - **市值约束（市价买）**：若 `本次 Notional > MaxNotional − 已花费`，则**按剩余预算裁量**：
     令可成交 base 量为 `floor(剩余预算 × 10^BaseDecimals / Price)`，只吃这个量（`IOC` 残部将
     被撤销，故不必吃到整级）。若裁量后 base 量为 0，停止撮合。
   - **STP**：若对手方挂单与本单 `Account` 相同 → **撤销该挂单（较旧者）**，释放其冻结残额，
     本单继续对下一张挂单撮合；本单自身不撤销。
   - 否则按 `min(本单剩余, 对手剩余)` 成交：产出 `Trade`，双方 `Filled` 前进，级 `total` 减少，
     对手挂单若全成则离簿。
   - **TIF**：
     - `GTC`：循环结束（无可成交对手）后残部**入簿**，`Status = New` 或 `PartiallyFilled`。
     - `IOC`：残部**不入簿**，直接撤销；`Filled == Qty` 则 `Filled`，否则 `Cancelled`。
   - 市价单在本设计中恒为 `IOC`（`Type = Market` 且 `TIF != GTC`；`NewEngine` 拒绝
     `Market + GTC`）。
4. 返回 `Result{Order, Trades, Releases}`；`Releases` 是本单与对手挂单因成交/撤销而**减少的
   冻结额**（服务层据此调用 `Holds.Release`）。

`Engine.Cancel(market, account, orderID)`：仅允许撤自己的单；挂单不存在或已离簿返回
`ErrOrderNotFound`；他人订单返回 `ErrNotOwner`；成功则离簿、释放残额、`Status = Cancelled`。

`Engine.Book(market, depth)`：返回买/卖各前 `depth` 级的 `(price, total)` 快照，供行情与测试。

### 4.7 内核不变量（必须由测试守护）

1. `level.total == Σ(该级挂单剩余量)`；级空即从 `prices`/`levels` 移除，不残留空级。
2. `side.prices` 严格有序且与 `levels` 键集合一致；买降序、卖升序。
3. 同一价级内 `fifo` 顺序 == `Seq` 升序。
4. 任意**在簿**挂单 `0 ≤ Filled < Qty` 且 `Remaining > 0`（`Filled == 0` 对刚入簿未成交的单是
   合法的，故不能写成 `0 < Filled`）。
5. **在簿**挂单的 `reserved` 覆盖其剩余量应冻结额：卖单 `reserved == Remaining`；买单
   `reserved ≥ ceil(Remaining × Price / 10^BaseDecimals)`。（`uint256` 无负值，故"`reserved ≥ 0`"
   是空话；真正有意义的是这一条与第 8 条。）
6. **成交零和 + 无量泄漏**：每笔成交 `Notional == floor(Qty*Price/10^BaseDecimals)`；一买一卖
   两侧记账金额恒等。
7. **确定性**：随机指令序列下，两台同配置引擎产出逐字段相同的 `Trades` 与最终 `Book`。
8. **冻结全账**：`Σ(所有被接受订单入簿时的冻结额) == Σ(所有 `Releases`) + Σ(`OpenReserves()`)`。
   第 5 条只检查**在簿**订单，无法发现"订单离簿时残额未释放"这类泄漏（残额随订单一起离开视野）；
   第 8 条是全账恒等式，是那类泄漏的唯一守护。**实现期正是靠它发现了全成买单的取整残额泄漏。**

---

## 5. 冻结额 `backend/ledger/holds.go`

```go
type Holds struct {
    mu     sync.Mutex
    l      *Ledger
    held   map[holdKey]*uint256.Int
}

type holdKey struct { Account AccountID; Asset AssetID }

func NewHolds(l *Ledger) *Holds
func (h *Holds) Held(a AccountID, asset AssetID) *uint256.Int        // 冻结额
func (h *Holds) Available(a AccountID, asset AssetID) *uint256.Int   // 余额 − 冻结（不为负）
func (h *Holds) Reserve(a AccountID, asset AssetID, amount *uint256.Int) error
func (h *Holds) Release(a AccountID, asset AssetID, amount *uint256.Int) error
func (h *Holds) Reconcile(open map[holdKey]*uint256.Int)             // 重放后按内核重建
// Spend 在持有冻结锁的前提下校验"可用 ≥ amount"并执行 debit。
// 它保证自校验到 debit 之间没有并发的 Reserve 抢走资金。
func (h *Holds) Spend(a AccountID, asset AssetID, amount *uint256.Int, debit func() error) error
```

- `Reserve` 在 `Held` 上加；`Release` 减（减到 0 删除键；减成负数是内部 bug，返回错误）。
- `Available` 用 `max(0, balance − held)`：账本余额不可能小于冻结额（冻结受可用约束），负值只
  可能是 bug，钳到 0 以免负值外溢，同时由测试守护该情形不出现。
- **`Spend` 是提现的必经之路**：`SettlementService.Withdraw` 的"余额检查 + `WithdrawOp`"整体
  在 `Holds.mu` 内完成，与 `Reserve` 互斥。这正是"提现不能提走冻结资金"的落点。

---

## 6. 服务层 `services/order_book.go`

### 6.1 生命周期

实现 `services.Service`（`Name/Initialize/Start/Stop`），沿用子项目 2 的 `runner` 约定：`Start`
重建内部通道、`Stop` 幂等、可重复 `Start→Stop→Start→Stop` 不 panic。

### 6.2 下单流程（顺序即裁定，不可交换）

```
Place(req) :
  1. svc 互斥锁
  2. required := engine.RequiredReserve(req)        // 纯函数，不改状态
     holds.Reserve(account, asset, required)        // 失败 → 拒绝，订单簿零改动
  3. journal.Append(placeCmd(req))                  // 唯一真相源，durable
  4. res := engine.Place(req)                       // 纯确定性，无 I/O；理论上不可失败
     res.Order.Status == Rejected → 释放 required，回滚第 2 步，返回错误
  5. for _, t := range res.Trades:
        ledger.Apply(baseTransfer(t), quoteTransfer(t))   // 两条零和转移，一次原子 Apply
        失败 → 进入致命降级（见 6.5）
  6. for _, r := range res.Releases: holds.Release(...)
  7. EventBus 发布 trade / order 事件
```

- 第 2 步先冻结、第 4 步才动订单簿：冻结失败时订单簿**零改动**。
- 第 5 步**先于**第 6 步：反向会在窗口内虚增可用余额。
  **注意：这条顺序无法由终态断言验证**——"转移"与"释放"在终态下可交换（终态可用余额相同），
  所以"成交后可用余额正确"的用例对顺序是**盲的**。唯一能观测它的是在第 5、6 步之间取一个
  观测点：`OrderBookService` 因此保留一个非导出的测试钩子 `beforeRelease`，在"账本已结算、冻结
  未释放"处回调；测试在该点断言**可用余额被低估而非高估**（`mid + 释放额 == final` 且
  `mid < final`）。实测：把两步对调后，该用例变红，而端态用例仍然通过。
- 第 4 步若返回 `Rejected`（例如市价买 `MaxNotional` 过低），必须**回滚第 2 步的冻结**并**丢弃
  已落盘的指令**（追加一条补偿记录或让重放跳过被拒指令——见 6.4）。

`Cancel(market, account, orderID)` 同构：落日志 → `engine.Cancel` → 释放残额 → 事件。

### 6.3 成交的账本编码

一笔成交 = 两条 `TransferOp`（base 与 quote），同一个 `Apply` 内原子提交：

```go
// 买方 = 出 quote、入 base；卖方 = 出 base、入 quote
base :=  TransferOp{From: seller, To: buyer,  Asset: market.Base,  Amount: t.Qty,      Ref: fmt.Sprintf("trade:%d:base",  t.ID)}
quote := TransferOp{From: buyer,  To: seller, Asset: market.Quote, Amount: t.Notional, Ref: fmt.Sprintf("trade:%d:quote", t.ID)}
```

- **幂等键由成交序号派生**，重放同一成交即同一键 ⇒ 账本跳过 ⇒ 不重复结算。
- 但幂等键的**语义前提是金额完全一致**（子项目 2 的教训：键相同而金额不同会被静默忽略）。
  成交金额是撮合的确定性输出，故 `TradeID` 相同必然金额相同；本子项目以**确定性测试**守护。
- 无手续费（裁定），故不需要手续费腿。

### 6.4 指令日志（订单簿 WAL）

- 文件 `orders.wal`，JSON 行，每条 `{seq, kind: place|cancel, payload}`。
- 沿用子项目 2 的**尾损容忍**规则：末条非空行解析失败或文件末字节非换行 → 忽略该条并告警；
  **中间记录**损坏必须报错。
- 启动：`Replay` → 逐条重放（重复 6.2 的第 2/4/5/6 步，只是冻结改由 `Holds.Reconcile` 一次
  设定）→ 账本侧对已结算成交是幂等跳过，对"已落盘未结算"的成交则**补结算**，实现自愈。
- 被拒指令：`placeCmd` 重放时同样被拒（确定性），此时其冻结在当次重放中未发生，
  `Holds.Reconcile` 以内核最终状态为准，故无需特例。
- 提供 `Snapshot`（可选，v1 允许只靠 WAL 全量重放；启动慢但正确，先求正确）。

### 6.5 致命降级

`ledger.Apply` 失败（WAL 写入错误等）：置 `svc.fatalErr`，之后所有 `Place/Cancel` 返回
`ErrServiceDegraded`，并发布 `orderbook.degraded` 事件。理由：此时订单簿已推进而账本未记，
继续服务会高估可用余额。**恢复方式 = 重启 + 重放**（指令日志是真相源，账本按幂等键收敛）。

---

## 7. 配置

| 键 | 含义 | 默认 |
|----|------|------|
| `ORDERBOOK_DATA_DIR` | 指令日志目录（空则纯内存，不持久化） | 空 |
| `ORDERBOOK_MARKETS` | 市场定义（见下） | 空（不启用） |

市场定义以紧凑字符串表达，避免为 v1 引入配置语言：
`ID:SYMBOL:BASE_ASSET:QUOTE_ASSET:BASE_DECIMALS[:MIN_QTY]`，多个以 `;` 分隔。
例：`1:ETH/USDT:2:1:18:1000000000000000`（base=asset 2、quote=asset 1、base 18 位小数、
最小 0.001 ETH）。解析失败必须**启动失败**，不得静默跳过市场。

---

## 8. 测试策略

### 8.1 `matching` 单测（表驱动）
- 价格优先 / 时间优先 / 跨级吃单；部分成交；同级 FIFO。
- 限价 GTC 残部入簿；IOC 残部撤销；市价买卖；市价买按 `MaxNotional` 裁量并停止。
- STP：撤旧单后继续撮合；被撤挂单冻结全额释放。
- 撤单：正确性、权限、未知单、重复撤单。
- 舍入与零和：`Notional` 恒等于公式；不可整除价位的多笔部分成交后冻结残额恰好归零。
- 不变量：4.7 的 1/2/3/4/5 逐条断言（含 `level.total` 一致性、价格级不残留空级）。
- **确定性**：随机指令序列 → 两台引擎逐字段比对（`Trades`、`Book`、每单 `reserved`）。

### 8.2 `ledger` 单测
- `Holds`：`Available` 扣除冻结；`Reserve` 不足即失败且不改状态；`Release` 归零删除键；
  `Reconcile` 重建；`Spend` 与 `Reserve` 并发互斥（并发测试断言不超发）。

### 8.3 `services` 单测
- 端到端下单成交：账本余额变化正确、`Σ余额` 不变（守恒）。
- **提现不能动冻结**：有挂单时提现冻结部分被拒。
- **崩溃重放**：构造"指令已落盘、账本未结算"的日志 → 新服务启动 → 账本被补结算、订单簿与
  冻结额与崩溃前一致（逐字段）。
- **重放不重复结算**：完整日志重放两遍，余额与冻结额不变。
- 致命降级：注入 `Apply` 失败 → 后续 `Place/Cancel` 返回 `ErrServiceDegraded`。
- `Start→Stop→Start→Stop` 不 panic（沿用 `runner` 断言）。

### 8.4 跨层一致性
- 同一成交在 Go 侧编码的 `TransferOp` 金额，与账本 `Apply` 后余额变化一致；
  根（`RootAndVersion`）在重放后与崩溃前一致。

---

## 9. 文件布局

```
backend/
  ledger/holds.go, holds_test.go                       # 新增：可用余额/冻结
  matching/market.go, market_test.go                   # 市场配置与校验
  matching/order.go, order_test.go                     # 类型、订单、冻结额规则
  matching/book.go, book_test.go                       # 价格级 + 时间优先
  matching/engine.go, engine_test.go                   # 撮合循环、撤单、市价裁量、STP
  matching/invariants_test.go                          # 4.7 不变量 + 确定性随机测试
  services/order_book.go, order_book_test.go           # 服务装配、指令 WAL、降级
  services/settlement.go                               # 改：Withdraw 经 Holds.Spend
  services/stack.go                                    # 改：装配 OrderBookService
  config/config.go                                     # 改：ORDERBOOK_* 配置
  README.md                                            # 改：冻结语义、残余假设、运行方式
docs/superpowers/specs/2026-10-13-orderbook-dex-matching-engine-design.md   # 本文档
docs/superpowers/plans/2026-10-13-matching-engine.md                        # 实施计划
```

---

## 10. 已定决策记录

| 决策 | 结论 |
|------|------|
| 撮合内核定位 | 纯确定性 `backend/matching`：无 I/O、不读时钟、不用随机、不碰账本 |
| 序（时间优先） | `Seq` 单调递增，来自指令序列，**不用时间戳** |
| 冻结位置 | 引擎侧（账本零改动）；Merkle 根仍发布总额 ⇒ 强制退出仍可全取 |
| 冻结持久化 | 订单簿自带指令 WAL，重启重放重建订单簿与冻结额 |
| 提现口径 | 可用 = 余额 − 冻结；提现只能提可用（`Holds.Spend` 与 `Reserve` 互斥） |
| 价格表示 | `Price uint64` = quote 最小单位 / 1 整 base 单位；市场须按 `price ≥ 1` 方向配置 |
| 金额公式 | `notional = floor(qty × price / 10^baseDecimals)`；买入冻结取 `ceil` |
| 订单类型 | 限价 GTC + 市价 IOC + 撤销；`Market + GTC` 非法 |
| 市价买 | 必须给 `MaxNotional`（最多花费），按剩余预算裁量成交，残部撤销 |
| 手续费 | v1 零手续费 |
| 自成交 | 禁止：撤销较旧的挂单，入单继续撮合 |
| 结算顺序 | 先账本转移、后释放冻结；绝不反向 |
| 日志顺序 | 先落指令 WAL、再改订单簿、再应用账本 |
| 账本应用失败 | 致命降级，拒绝后续下单/撤单，重启重放对账 |
| 幂等键 | 成交：`trade:<TradeID>:base` / `:quote`；由确定性成交序号派生 |
| 离簿必释放冻结残额（子项目 3 实现期修复） | 订单全成/撤销/STP 被撤时释放 `reserved` 残额；**必须在扣减之后**释放（先释放会下溢）。买入 `ceil` 冻结与 `floor` 扣减之差是全成买单的取整残额，不释放即**永久冻结**；守护不变量是「冻结全账」恒等式，而非"在簿订单"检查 |
| 市价买预算不足（子项目 3） | 不拒绝；零成交、残部撤销、释放全部 `MaxNotional`。校验不得依赖订单簿状态 |
| 不变量 4 措辞（子项目 3 修正） | 在簿挂单为 `0 ≤ Filled < Qty`（`Filled == 0` 合法），原 `0 < Filled` 不成立 |
| 本轮未做（显式延后） | 订单 TTL/退出即自动撤单、手续费、订单簿快照、HTTP/WS、链上批量结算 |
```

# 子项目 6a 设计：永续仓位内核 + 逐仓保证金

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：子项目 6 的第一部分（6a）——永续仓位撮合内核 + 逐仓保证金 + 杠杆。标记价格、资金费率（6b）与强平引擎（6c）不在本文档。
> **前置**：子项目 1-5 均已合入 `master`（1 结算合约+账本、2 服务装配、3 现货撮合、4 网关、5 索引器）。
> **日期**：2026-10-18
> **状态**：已裁定关键设计，待实现

---

## 1. 背景与目标

### 1.1 承接关系

现有撮合内核 `backend/matching` 是**现货**模型：成交产生 base/quote 两资产的零和 `TransferOp`，资金冻结在
账户余额上，买卖方以资产交割。子项目 1 的 `Settlement` 合约与链下账本承载了现货资金托管与状态根锚定。

子项目 6 引入**永续合约**：交易标的不是资产余额而是**仓位**。用户以 quote 资产作保证金，做多或做空一个
市场，持有期间产生未实现盈亏，平仓时一次性结算净盈亏到账本余额。

本文档（6a）只做永续的**地基**：仓位模型、独立撮合内核、逐仓保证金与杠杆。标记价格 / 资金费率（6b）与
强平（6c）显式延后。

### 1.2 子项目路线图（依赖驱动）

| # | 子项目 | 内容 | 依赖 | 状态 |
|---|--------|------|------|------|
| 1..5 | 结算+账本、装配、现货撮合、网关、索引器 | — | — | ✅ 已合入 |
| **6a** | **永续仓位内核 + 逐仓保证金**（本文档） | 仓位模型、独立撮合、逐仓保证金、杠杆 | 1,3 | ✅ 已实现（PR #7） |
| 6b | 标记价格 + 资金费率 | oracle 喂价、标记价格、资金费率定期结算 | 6a | ✅ 已实现（PR #8） |
| 6c | 强平引擎 | 维持保证金不足时自动强平 | 6a,6b | ✅ 已实现（PR #9） |
| 7 | 前端 + 行情 | 交易界面、深度图、K 线 | 4,5,6 | ✅ 网关 PR #10；交易界面 PR #11（K 线未做） |

### 1.3 本子项目目标

1. 一个**独立**的永续撮合内核 `backend/perp`：不 import、不改动现货 `matching`；复用其算法模式（价格-时间
   优先、订单簿价格级、STP、限价 GTC / 市价 IOC），但代码独立、冻结语义按永续来。
2. **仓位模型**：做多/做空、加仓/减仓、开仓均价、未实现盈亏。
3. **逐仓隔离保证金**：每个账户 × 每个市场一个独立仓位与独立保证金池，互不影响。
4. **杠杆**：市场设上限（如 10x），用户下单时自选杠杆（≤ 上限）；开仓保证金 = `ceil(notional / leverage)`。
5. **平仓时一次性结算盈亏**：净盈亏写进账本余额，保证金释放；持有期间盈亏不写账本。
6. 纯确定性：与现货一样，无 I/O、不读时钟、指令序列重放可复现；崩溃后由指令 WAL 重放自愈。

### 1.4 非目标（明确不做，留给后续子项目）

标记价格 / oracle 喂价、资金费率（6b）；维持保证金与强平（6c）；全仓交叉保证金、子账户、多资产保证金；
止损/止盈、post-only、FOK、reduce-only、冰山；手续费与返佣；永续的 HTTP/WS 网关（6a 只做内核 + 服务层，
对外接口留待 6c 之后或子项目 7 前单独评估）；对链上 `Settlement` 合约的任何改动。

---

## 2. 关键裁定与信任边界

### 2.1 本子项目裁定（已与 owner 确认）

| 议题 | 结论 | 理由 |
|------|------|------|
| 内核形态 | 独立 `backend/perp`，现货 `matching` 零改动 | 现货冻结「买=quote/卖=base」与永续「多空都冻结 quote」语义冲突，无法在不改现货的前提下复用其冻结逻辑；独立演进更清晰 |
| 保证金模式 | 逐仓隔离（账户 × 市场） | 风险隔离、实现简单、dYdX v3 默认 |
| 保证金资产 | 市场 quote 资产 | 现货已有该资产余额，最自然 |
| 杠杆 | 市场设上限 + 用户自选 | 灵活且封顶风险 |
| 盈亏结算 | 平仓时一次性结算 | 简单；持有期盈亏仅作账本外「未实现」值 |
| 开仓均价 | notional 加权：`Σnotional / Σsize` | 行业标准 |
| 击穿处理 | 亏损 > 保证金时 `WithdrawOp` 失败 → 服务致命降级 | 6a 无强平引擎，击穿是显式已知缺口，交 6c |

### 2.2 v1 保证

1. **确定性**：同市场配置 + 同指令序列 ⇒ 同仓位、同成交、同结算；重放可复现。
2. **保证金覆盖**：开仓冻结的保证金 ≥ 初始所需；任何平仓扣减都不会因保证金不足而「半成交」（除 §2.3 击穿）。
3. **守恒**：平仓净盈亏 = 多空双方盈亏之和为零（成交在仓位层零和，结算在账本层把零和盈亏物化）。
4. **隔离**：一个仓位的盈亏不影响同账户其它市场、也不影响其它账户。
5. **崩溃自洽**：指令 WAL 重放后仓位、保证金冻结、账本三者收敛。

### 2.3 残余信任假设（必须在 README 与文档中显式声明）

1. **击穿缺口（6a 已知）**：6a 无强平引擎，若价格剧烈波动导致未实现亏损超过保证金，平仓时的 `WithdrawOp`
   会因余额不足失败并触发服务致命降级（与现货账本应用失败同策略）。真实系统靠 6c 的强平引擎在击穿前
   强制平仓；6a 阶段此缺口显式存在。
   > **已由 6c 关闭**：强平引擎在击穿前平仓；即便来不及，亏损由保险基金吸收并通过 `perp.baddebt`
   > 事件上报，订单簿不再降级。见 `2026-10-31-orderbook-dex-perp-liquidation-design.md`。
2. **无标记价（6a 已知）**：6a 的盈亏用**成交价**计算，不区分标记价。6b 引入 oracle 后改用标记价。
   > **已由 6b 关闭**：未实现盈亏与资金费使用标记价；已实现盈亏仍按成交价（这是正确的）。
   > 见 `2026-10-31-orderbook-dex-perp-funding-design.md`。
3. **冻结语义不复用现货**：永续多空都冻结 quote 保证金，与现货「卖冻结 base」不同；这是独立内核的直接
   后果，也是本子项目刻意为之。

---

## 3. 架构概览

```
┌────────────────────────────────────────────────────────┐
│  services/perp_order_book.go  (PerpOrderBookService)    │
│  冻结(quote 保证金) → 落指令 WAL → perp 撮合 →          │
│  平仓盈亏结算(账本) → 释放保证金 → 事件                  │
└──────────────┬─────────────────┬───────────────────────┘
               │                 │
       backend/perp (纯内核)    backend/ledger (复用，不改)
       ┌──────────────────┐    ┌────────────────────────┐
       │ Engine / book    │    │ Ledger（Deposit/Withdraw）│
       │ Position 仓位     │    │ Holds（冻结保证金）      │
       │ 价格-时间优先     │    └────────────────────────┘
       │ 限价/市价/STP     │
       └──────────────────┘
```

分层约束：`backend/perp` 是纯确定性内核，无 I/O、不读时钟、不碰账本、不 import `matching`；所有副作用
（冻结、账本、事件、持久化）在服务层。现货 `matching` 完全不被本子项目触碰。

---

## 4. 撮合内核 `backend/perp`

### 4.1 类型

```go
type Side   uint8 // Long, Short
type Type   uint8 // Limit, Market
type TIF    uint8 // GTC, IOC
type Status uint8 // New, PartiallyFilled, Filled, Cancelled, Rejected

type OrderID uint64 // 引擎分配，从 1 单调递增
type Seq     uint64 // 时间优先序，从 1 单调递增
type Price   uint64 // quote 最小单位 / 1 整 base 单位
type TradeID uint64 // 成交序号，从 1 单调递增
type MarketID uint32
```

`Size`/`Amount`/`Margin` 用 `*uint256.Int`；`Price` 用 `uint64`（沿用现货约定，`price ≥ 1`）。

### 4.2 市场配置

```go
type Market struct {
    ID           MarketID
    Symbol       string
    Base, Quote  ledger.AssetID   // quote = 保证金/结算币
    BaseDecimals uint8            // notional = floor(size*price/10^BaseDecimals)
    MaxLeverage  uint32           // 如 10
    MinQty       *uint256.Int     // 可空
    MaxQty       *uint256.Int     // 可空；缺省 2^128
}
```

`Market.Validate()`：`Base ≠ Quote`、`MaxLeverage ≥ 1`、`10^BaseDecimals` 可表达、`MinQty`/`MaxQty` 非负。
`NewEngine(markets []Market)` 拒绝重复 `ID`/`Symbol` 与非法市场。

### 4.3 仓位与订单

```go
// Position 是逐仓账户在某一市场的净仓位。Side 与 Size 表示方向与数量；
// 同一账户在同一市场只能有一个方向（开反向仓即减仓，减到 0 后方向才翻转）。
type Position struct {
    Market     MarketID
    Account    ledger.AccountID
    Side       Side
    Size       *uint256.Int // base 数量，> 0
    EntryPrice Price        // 数量加权开仓均价
    Margin     *uint256.Int // 已冻结保证金（quote）
    Leverage   uint32       // 用户选定杠杆（≤ 市场上限）
}

// Order 是入簿或撮合中的挂单。
type Order struct {
    ID      OrderID
    Seq     Seq
    Market  MarketID
    Account ledger.AccountID
    Side    Side
    Type    Type
    TIF     TIF
    Price   Price       // 限价必填；市价 0
    Qty     *uint256.Int // base 数量（两类订单一致）
    MaxNotional *uint256.Int // 市价单必填：开仓名义额上限（quote），用于推导保证金；限价单可选，作安全上界
    Filled  *uint256.Int
    Status  Status
    reserved *uint256.Int // 内部：当前仍冻结的保证金
    elem    *list.Element
}

// Trade 是永续撮合的一笔成交，语义是「仓位配对」而非资产转账。
type Trade struct {
    ID         TradeID
    Market     MarketID
    Price      Price
    Qty        *uint256.Int // 成交 base 数量
    Notional   *uint256.Int // floor(Qty*Price/10^BaseDecimals)
    MakerOrder OrderID
    TakerOrder OrderID
    Maker      ledger.AccountID
    Taker      ledger.AccountID
    MakerSide  Side
}
```

### 4.4 冻结额推导（保证金）

保证金**按订单价**计算并冻结，**成交价不重算保证金**。沿用现货的「整单 ceil 冻结 + 逐笔 floor 扣减」技巧，
靠 `floor` 的次可加性保证冻结永不被超扣（`Σ floor(逐笔) ≤ floor(整单) ≤ ceil(整单)`）：

- **订单级冻结（下单时）**：`required = ceil(notional(qty, orderPrice) / leverage)`（quote），做多做空一致，都
  冻结 quote。这是服务层 `holds.Reserve` 的一次性预冻结。
- **市价单（无订单价）**：`orderPrice` **必须为 0**（非 0 即拒绝，防止攻击者用自选价压低保证金）；改用
  `MaxNotional` 推导冻结额 `required = ceil(MaxNotional / leverage)`（quote）；因此 **`MaxNotional` 对市价单必填**
  （对限价单只是可选的安全上界，v1 仅对市价单生效）。市价单的持仓级记账同样以成交价 `fillPrice` 为准。
- **持仓级记账（开仓/加仓成交时）**：`margin = floor(notional(fillQty, orderPrice) / leverage)`，从该订单的
  冻结残额转入 `Position.Margin`。总冻结量不变（只是从「订单」归因到「持仓」）。市价单以成交价 `fillPrice`
  计算 `floor(notional(fillQty, fillPrice)/leverage)`，因成交价必 ≤ `MaxNotional` 对应的均价，`Σfloor ≤ required`。
- **减仓/平仓**：不新增冻结，释放被减仓部分对应的保证金 `floor(pos.Margin × closed / prevSize)`。
- **离簿/撤销**：释放订单冻结残额（`ceil` 与 `Σ floor` 之差）。

成交价与订单价的差异导致的杠杆漂移，是 6a 的**已知简化**——真实风控靠 6c 的维持保证金（按标记价）+ 强平
兜底，6a 不做。

`RequiredMargin(req)` 是纯函数，返回订单级冻结额 `ceil(notional(qty, orderPrice)/leverage)`。金额运算全部走
显式溢出检查（`MulOverflow`/`AddOverflow`/`SubOverflow`），溢出即 `ErrAmountTooLarge` 拒绝，绝不静默取模——
这是从现货子项目 3 学到的关键安全属性。

### 4.5 订单簿与撮合语义

- 订单簿结构与现货一致：价格级 + FIFO，买降序/卖升序，O(1) 撤单。
- 撮合循环、STP、限价 GTC 残部入簿、市价 IOC 残部撤销、市价买按 `MaxNotional` 裁量——照搬现货语义。
- **差异**：成交不产生 `TransferOp`，而是输出仓位变更。内核返回：

```go
type Result struct {
    Order    *Order
    Trades   []Trade
    // Releases 是「因减仓/撤销而释放的保证金」清单（服务层据此 holds.Release）。
    // 开仓/加仓不产生 Release：保证金从订单冻结残额转入持仓，总冻结量不变。
    Releases []MarginRelease
    // PnL 是「因减仓/平仓而实现」的净盈亏，服务层据此做 DepositOp/WithdrawOp。
    PnL      []PnLSettlement
}
```

- **仓位更新**（内核内部维护 `map[account][market]Position`）：
  - 做多方向成交：买方 `Size += qty`（开多/加多），卖方 `Size -= qty`（减多/平多/翻空）
  - 做空方向成交：卖方 `Size += qty`（开空/加空），买方 `Size -= qty`（减空/平空/翻多）
  - 减到 0 即平仓；同一账户反向成交先减仓，减到 0 后剩余量才作为反向开仓。
- **未实现盈亏**：`upnl(pos, markPrice)`，做多 `notional(mark)-notional(entry)`，做空反之。

### 4.6 内核不变量（必须由测试守护）

1. 同一账户在同一市场至多一个仓位（`Size > 0`），且 `Side` 与 `Size` 一致。
2. 仓位 `EntryPrice` 是**数量加权**均价：`EntryPrice == floor(Σ(qty·price) / Σqty)`，允许 ±1 个 quote 最小单位
   的增量取整误差（逐笔整数平均会复用已取整的均价）。注意：不是 `Σnotional/Σsize`——`notional` 已除过
   `10^BaseDecimals`，那样会差一个量纲并在小仓位上塌缩为 0。
3. 任意**在簿**挂单的 `reserved` 覆盖其剩余量应冻结的保证金（`reserved ≥ Σ floor(剩余逐笔)`）。
4. **保证金全账恒等式（防泄漏，现货教训）**：`Σ(订单级冻结) == Σ(Releases) + Σ(在簿 reserved 残额) + Σ(持仓 Margin)`。
   任何开仓/减仓/撤销都必须守恒——这是发现「残额未释放」这类泄漏的唯一守护。
5. 成交在仓位层零和：每笔成交买卖双方 `Size` 增减绝对值相等。
6. **确定性**：随机指令流下两台同配置引擎产出逐字段相同的 `Trades` 与最终 `Position`。

---

## 5. 服务层 `services/perp_order_book.go`

### 5.1 生命周期

实现 `services.Service`，沿用 `runner` 约定：`Start` 重建通道、`Stop` 幂等、可重复 Start/Stop。

### 5.2 下单/平仓流程（顺序即裁定，不可交换）

```
Place(req):
  1. svc 互斥锁
  2. required := engine.RequiredMargin(req)      // 纯函数
     holds.Reserve(account, quote, required)     // 失败 → 拒绝，仓位零改动
  3. journal.Append(placeCmd(req))               // 唯一真相源
  4. res := engine.Place(req)                    // 纯确定性
     被拒 → 释放 required，回滚，返回错误
  5. 结算平仓盈亏（每笔成交按仓位方向）：
       盈利 → ledger.Apply(DepositOp{user, quote, pnl})
       亏损 → ledger.Apply(WithdrawOp{user, quote, |pnl|})
     失败（含击穿）→ 致命降级
  6. 释放 res.Releases 的保证金
  7. EventBus 发布 trade / position 事件
```

- **结算顺序**：先账本、后释放冻结（沿用现货教训，避免窗口期虚增可用余额）。
- 减仓/平仓的保证金释放：按被减仓位占比释放（`release = floor(margin × closedSize / prevSize)`）。
- 开仓均价更新在引擎内部完成，服务层只搬运。

### 5.3 成交的账本编码

平仓/减仓的净盈亏，按仓位方向与成交价相对 `EntryPrice` 计算，物化为一条 `DepositOp` 或 `WithdrawOp`。
持仓不变（加仓、开仓）不产生盈亏结算，只做保证金冻结。

**幂等键必须含账户**：`perp:pnl:<tradeID>:<account>`。原因：一笔成交可能**双方都在平仓**（maker 与 taker 各
自减仓），引擎会为同一个 `TradeID` 产出**两条** `PnLSettlement`。若只用 `perp:pnl:<tradeID>`，账本按 `Ref`
去重会静默丢弃其中一条（资金被销毁或凭空铸造）。加账户后缀使每条结算唯一。

### 5.4 指令日志与崩溃重放

沿用现货 `Journal`：`orders.wal`，`place`/`cancel` 两类指令；尾损容忍；重启重放重建仓位与冻结额；盈亏结算
按 `perp:pnl:<tradeID>:<account>` 幂等。保证金冻结由 `Holds.Reconcile` 以内核最终状态一次设定（**含在簿挂单的
`reserved` 残额**，否则重启后可用余额虚高、后续撤单会下溢）。

### 5.5 致命降级

账本 `Apply` 失败（含击穿的 `WithdrawOp` 失败）：置 `fatalErr`，后续 `Place/Cancel` 返回 `ErrServiceDegraded`，
发布 `perp.degraded` 事件。恢复方式 = 重启 + 重放（指令日志是真相源，账本按幂等键收敛）。

---

## 6. 配置

| 键 | 含义 | 默认 |
|----|------|------|
| `PERP_MARKETS` | 永续市场定义，格式 `ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]`，`;` 分隔 | 空（不启用） |
| `PERP_DATA_DIR` | 指令日志目录（空则内存） | 空 |

例：`1:ETH-PERP:2:1:18:10:1000000000000000`（base=2、quote=1、base 18 位、10x、最小 0.001）。

解析失败必须启动失败，不得静默跳过。

---

## 7. 测试策略

### 7.1 `perp` 单测（表驱动）

- 开多/开空/加仓/减仓/平仓/翻仓的仓位与 `EntryPrice` 正确。
- 保证金推导：`ceil(notional/leverage)`；杠杆上限校验；溢出拒绝。
- 订单簿价格优先/时间优先/跨级/部分成交/STP/市价裁量（照搬现货用例，但断言仓位而非转账）。
- 不变量 4.6 逐条，含随机确定性测试与保证金全账恒等式。

### 7.2 `services` 单测

- 端到端开仓→平仓：账本余额变化正确、保证金冻结/释放正确、未实现盈亏→已实现盈亏正确。
- 击穿：构造亏损 > 保证金 → `WithdrawOp` 失败 → 致命降级（显式守护已知缺口）。
- 崩溃重放：指令已落盘、账本未结算 → 重启 → 仓位/冻结/账本收敛。
- 重放不重复结算；`Start→Stop→Start→Stop` 不 panic。

### 7.3 回归

现货 `matching` 与全量 Go + Solidity 测试必须保持绿色（本子项目不改现货、不改合约）。

---

## 8. 文件布局

```
backend/
  perp/
    market.go, market_test.go      # 永续市场配置与校验
    order.go, order_test.go        # 仓位、订单、类型、保证金规则
    book.go, book_test.go          # 价格级 + 时间优先订单簿
    engine.go, engine_test.go      # 撮合循环、仓位更新、撤单、市价裁量
    invariants_test.go             # 4.6 不变量 + 确定性随机测试
  services/
    perp_order_book.go             # 服务装配、指令 WAL、降级、盈亏结算
    perp_order_book_test.go
  config/config.go                 # 改：PERP_* 配置
  README.md                        # 改：仓位语义、击穿缺口、运行方式
docs/superpowers/specs/2026-10-18-orderbook-dex-perp-design.md   # 本文档
docs/superpowers/plans/2026-10-18-orderbook-dex-perp.md          # 实施计划
```

---

## 9. 已定决策记录

| 决策 | 结论 |
|------|------|
| 内核形态 | 独立 `backend/perp`，现货 `matching` 零改动 |
| 保证金模式 | 逐仓隔离（账户 × 市场） |
| 保证金资产 | 市场 quote 资产 |
| 杠杆 | 市场设上限 + 用户自选（`ceil(notional/leverage)`） |
| 盈亏结算 | 平仓时一次性结算进账本；持有期仅「未实现」 |
| 开仓均价 | notional 加权 `Σnotional/Σsize` |
| 击穿 | 亏损 > 保证金 → 致命降级（6a 已知缺口，交 6c 强平） |
| 冻结语义 | 多空都冻结 quote 保证金（不复用现货「卖冻结 base」） |
| 保证金定价 | 按**订单价**冻结，成交价不重算；杠杆漂移是 6a 已知简化，6c 用维持保证金兜底 |
| 保证金冻结模型 | 订单级 `ceil(notional(qty,orderPrice)/lev)` 预冻结；持仓级 `floor(notional(fillQty,orderPrice)/lev)` 逐笔记账；减仓 `floor(margin×closed/prevSize)` 释放；离簿释放 `ceil-Σfloor` 残额。`floor` 次可加性保证永不被超扣（现货子项目 3 技巧） |
| 市价单保证金 | 无订单价，改用 `MaxNotional` 推导：`required = ceil(MaxNotional/lev)`；`MaxNotional` 对市价单必填 |
| 开仓均价 | **数量加权** `floor(Σ(qty·price)/Σqty)`；不是 `Σnotional/Σsize`（差量纲、会塌缩为 0） |
| 幂等键 | 盈亏结算 `perp:pnl:<tradeID>:<account>`（**必须含账户**：一笔成交可能双方都平仓，同一 TradeID 产出两条结算，仅用 tradeID 会被账本去重丢一条）；保证金冻结走 Holds.Reconcile（含在簿 reserved） |
| 本轮未做（显式延后） | 标记价/oracle、资金费率（6b）、强平（6c）、全仓保证金、永续网关、手续费、合约改动 |

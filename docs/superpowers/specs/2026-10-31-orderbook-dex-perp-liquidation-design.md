# 子项目 6c 设计：强平引擎

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：子项目 6 的第三部分（6c）——维持保证金、强平引擎、保险基金。
> **前置**：6a（仓位内核）与 6b（标记价格 + 资金费率）已实现（PR #7、#8）。
> **日期**：2026-10-31
> **状态**：设计草案，待 owner 评审

---

## 1. 背景与目标

6a/6b 之后，永续合约的最后一个致命缺口是**击穿**（6a spec §2.3 第一条）：

> 6a 无强平引擎，若价格剧烈波动导致未实现亏损超过保证金，平仓时的 `WithdrawOp` 会因余额不足失败并触发服务致命降级。

也就是说：一个用户的仓位亏穿保证金，整个永续订单簿停机，所有用户都无法交易。这不是"小缺口"，这是**单点故障导致的市场级停摆**。

6c 要消除这个缺口：在亏损吃掉保证金之前，由系统主动强制平仓，把亏损限制在该用户的保证金内。

### 1.1 本子项目目标

1. **维持保证金（maintenance margin）**：仓位级的最低保证金要求，低于它即触发强平。
2. **强平触发与执行**：周期性扫描所有仓位，对不满足维持保证金的仓位强制平仓。
3. **强平价格与执行方式**：优先挂单平仓（限价，可被撮合），超时未成交则市价平仓；两者都保证**不吃穿保证金**。
4. **保险基金**：承接强平后的剩余亏损（穿仓部分），使系统整体不产生坏账。
5. **击穿语义降级**：即便强平引擎也来不及（极端跳空），账户亏穿的差额由保险基金吸收，**订单簿不再致命降级**。
6. **强平事件与可观测性**：每次强平发布事件，可查询强平历史。

### 1.2 非目标

自动减仓（ADL）、全仓交叉保证金、部分强平（v1 一次性全平）、多币种保证金、链上保险基金合约。

---

## 2. 关键裁定

| 议题 | 结论 | 理由 |
|------|------|------|
| 维持保证金率 | 市场配置 `MaintenanceMarginRate`（默认 0.5% = 初始保证金的 1/20 量级） | 与最大杠杆挂钩：杠杆越高，容错越窄 |
| 强平判断价 | **标记价格**（6b） | 用成交价会被插针操纵触发不当强平；标记价抗操纵 |
| 触发阈值 | `equity < maintenanceMargin`，其中 `equity = margin + unrealizedPnL` | 行业标准 |
| 平仓方式 | 先限价 IOC（挂在对手盘最优价上，立即尝试成交），残部再市价 IOC | 减少对盘口的冲击 |
| 亏损上限 | 强平**最多**扣到该仓位保证金为 0；超出部分由保险基金吸收 | 保护用户，不追缴 |
| 保险基金 | 运营方注资的一个 ledger 账户；不足时运营方账户兜底，并显式记录 | v1 简化，6c 后演进为链上基金 |
| 击穿降级 | **移除** `WithdrawOp` 失败即降级的路径，改为"扣到 0，差额记坏账事件" | 6c 存在的意义就是消除这个停摆风险 |
| 强平频率 | 每 `PERP_LIQUIDATION_INTERVAL`（默认 1s）扫描一次 | 足够快；不阻塞下单路径 |
| 部分强平 | 不做（v1 全平） | 简单；部分强平需要更复杂的风险计算 |

---

## 3. 设计

### 3.1 维持保证金

```go
// Market 新增字段
type Market struct {
    ...
    MaintenanceMarginRateE18 *uint256.Int  // e.g. 5e15 = 0.5%
    LiquidatorFeeE18         *uint256.Int  // 强平奖励，e.g. 1e15 = 0.1%
}
```

```
maintenanceMargin(pos) = ceil(notional(pos.Size, mark) * MaintenanceMarginRateE18 / 1e18)
equity(pos)            = pos.Margin + unrealizedPnL(pos, mark)
liquidatable           = equity < maintenanceMargin(pos)
```

**默认值**：`MaintenanceMarginRateE18 = 5e15`（0.5% 名义额）。10x 杠杆的初始保证金是 10%，维持保证金 0.5% 意味着价格反向移动约 9.5% 才触发强平，给强平引擎留出反应时间。

### 3.2 强平执行流程

```
scan(now):
  对每个市场、每个仓位：
    1. mark, ok := tracker.LastMark; if !ok → 跳过（无价不强平）
    2. if equity >= maintenanceMargin → continue
    3. 计算强平单：
       a. 限价 IOC：以对手盘最优价下反向单，数量 = pos.Size
          - 立即撮合，成交部分按 applyFill 语义结算（含 PnL）
       b. 残部市价 IOC：MaxNotional = 剩余数量 × mark
    4. 结算后，若仓位仍存在（盘口无对手盘）：
       - 挂一个限价单等待（可选，v1 直接市价 IOC 至盘口深度耗尽）
    5. 扣收强平费：从残余保证金中取 LiquidatorFee，转给运营方账户
    6. 发布 Liquidation 事件
```

**强平单不走用户下单路径**：它由服务层内部构造 `PlaceRequest` 并调用 `engine.Place`，但**不预先冻结用户保证金**（用户已无可冻资金）。因此需要一个内核新方法：

```go
// Liquidate 是引擎内部的强平路径：不冻结新保证金，直接以账户现有仓位反向成交。
func (e *Engine) Liquidate(m MarketID, a ledger.AccountID, maxQty *uint256.Int) (Result, error)
```

`Liquidate` 复用 `Place` 的撮合循环，但：
- 跳过 `holds.Reserve`（服务层直接调用引擎，不经 `Place` 服务方法）
- 订单的 `reserved = 0`，开仓腿的保证金从仓位自身扣（而不是从未冻结的订单）
- 盈亏结算走现有的 `PnLSettlement`

### 3.3 击穿处理（替代致命降级）

当平仓产生的 `WithdrawOp` 金额 > 账户可用余额时（即 `equity < 0`，亏损超过保证金）：

1. 扣减该账户**全部可用余额**（不会变成负数）
2. 差额记为**坏账**，由保险基金 `DepositOp` 补足
3. 发布 `perp.baddebt` 事件（含金额）
4. **不降级**

这需要修改 `settlePnL`：捕获 `ErrInsufficientBalance`，改为"扣可用余额 + 保险基金补差"。

**保险基金账户**：配置 `PERP_INSURANCE_FUND_ACCOUNT`（20 字节十六进制）；未配置时用固定的保留账户 `0x...dead`。

### 3.4 服务层装配

```go
type PerpOrderBookService struct {
    ...
    liquidationOpts LiquidationOptions
    insuranceFund   ledger.AccountID
}
```

runner 的 tick 增加强平扫描（与价格刷新、资金费同一个 loop）。

### 3.5 事件

```go
const TopicPerpLiquidation = "perp.liquidation"
const TopicPerpBadDebt     = "perp.baddebt"

type LiquidationEvent struct {
    Market   perp.MarketID
    Account  ledger.AccountID
    Size     *uint256.Int
    Mark     perp.Price
    Equity   *big.Int
    Fee      *uint256.Int
    At       int64
}

type BadDebtEvent struct {
    Market  perp.MarketID
    Account ledger.AccountID
    Amount  *uint256.Int
    At      int64
}
```

### 3.6 配置

| 键 | 含义 | 默认 |
|----|------|------|
| `PERP_MAINTENANCE_MARGIN_RATE` | 维持保证金率（1e18 定点） | `5000000000000000`（0.5%） |
| `PERP_LIQUIDATOR_FEE` | 强平费（1e18 定点） | `1000000000000000`（0.1%） |
| `PERP_INSURANCE_FUND_ACCOUNT` | 保险基金账户 | 保留账户 |
| `PERP_LIQUIDATION_INTERVAL` | 强平扫描间隔（秒） | `1` |

---

## 4. 测试策略

### 4.1 `perp` 单测

- `MaintenanceMargin`：不同仓位大小/标记价。
- `Liquidate`：反向平仓、部分成交、盘口不足。
- 击穿：`equity < 0` 时的处理。

### 4.2 `services` 单测

- 触发强平：价格下跌使多头低于维持保证金 → 自动平仓 → 保证金释放 → 无仓。
- 不触发：健康仓位价格波动不触发。
- 保险基金：亏损超过保证金时由基金补足，账户余额不为负，**服务不降级**。
- 无标记价时不强平（安全）。
- 强平后账本守恒：所有账户余额 + 保险基金 = 初始总额（手续费除外）。

### 4.3 回归

全量 Go + Solidity 必须绿色。**特别验证**：6a 的"击穿致命降级"测试必须改为"击穿由保险基金吸收"。

---

## 5. 文件布局

```
backend/
  perp/
    risk.go               # MaintenanceMargin, Equity, Liquidatable
    risk_test.go
    engine.go             # + Liquidate
  services/
    perp_risk.go          # 强平扫描循环
    perp_risk_test.go
    perp_order_book.go    # + 击穿处理改造、强平事件
    perp_funding.go       # 不变
  config/config.go        # + 6c 配置
docs/superpowers/specs/2026-10-31-orderbook-dex-perp-liquidation-design.md
docs/superpowers/plans/2026-10-31-orderbook-dex-perp-liquidation.md
```

---

## 6. 残余信任假设

1. **保险基金规模由运营方决定**：基金不足以吸收坏账时，坏账累积在系统内（会计上体现为负权益）。真实系统用 ADL 最后处理，6c 不做。
2. **强平依赖标记价新鲜**：无价时不强平，这是有意的保守选择——用陈旧价强平可能误伤。
3. **全平而非部分平**：v1 一次性平掉整个仓位，可能比必要更激进。
4. **强平不保证成交**：盘口深度不足时，仓位可能只平掉一部分，剩余继续暴露。v1 接受这一点并在事件中标记 `partial`。

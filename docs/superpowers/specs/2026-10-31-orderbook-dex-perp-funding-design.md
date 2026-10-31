# 子项目 6b 设计：标记价格 + 资金费率

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：子项目 6 的第二部分（6b）——oracle 喂价、标记价格、资金费率定期结算。
> **前置**：6a（永续仓位内核 + 逐仓保证金）已实现（PR #7）。
> **日期**：2026-10-31
> **状态**：设计草案，待 owner 评审

---

## 1. 背景与目标

6a 的永续内核能用，但有两个显式缺口（6a spec §2.3）：

1. **无标记价**：未实现盈亏、以及平仓时如果只用成交价，会暴露在价格操纵与插针上。真实交易所结算用**标记价格**
   （多来源指数价 + 盘口平滑），而不是最新成交价。
2. **无资金费率**：永续合约靠资金费率把价格锚回现货指数。没有它，多头/空头长期失衡，仓位价格会持续偏离。

6b 只做**定价与资金流**。强平（6c）依赖 6b 的标记价与维持保证金，不在本文档。

### 1.1 本子项目目标

1. **指数价（index price）**：由一个可替换的 `PriceSource` 接口提供，支持多来源聚合与陈旧/离群值校验。
2. **标记价（mark price）**：由指数价与永续盘口推导，抗单点操纵（详见 §3.2）。
3. **资金费率**：按固定周期（默认 1 小时）根据**溢价指数**计算，正费率多头付空头，负费率反之。
4. **资金费结算**：周期性把资金费从一方账户扣到另一方账户（账本 `TransferOp`），幂等且可重放。
5. **PnL 改用标记价**：未实现盈亏的展示改按标记价；6a 的已实现盈亏仍按成交价（真实成交对手方价格），
   这是正确的——已实现盈亏必须用实际成交价，标记价只用于未实现盈亏与风控。

### 1.2 非目标

强平（6c）、保险基金、ADL（自动减仓）、多币种保证金、跨市场资金费优化、链上 oracle 合约。

---

## 2. 关键裁定

| 议题 | 结论 | 理由 |
|------|------|------|
| 价格来源 | 服务层可替换的 `perp.PriceSource` 接口；内核**不**读价格 | 内核必须保持纯确定性（6a §1.3.6），价格由服务层注入 |
| 确定性 | 标记价与资金费**作为指令落 WAL**，重放时用落盘值而不是重新拉取 | 否则重启后拉到不同价格，重放结果与账本不一致 |
| 资金费周期 | 固定 `FundingInterval`（默认 3600s），由服务层 runner 驱动 | 简单；间隔短到足以锚定，长到不产生过量交易 |
| 资金费公式 | `rate = clamp(premiumIndex / intervalHours, -maxRate, +maxRate)`，`premiumIndex = (mark - index) / index` | BitMEX / dYdX 同款 |
| 资金费结算 | `amount = floor(positionSizeNotional * rate)`，多空按标记价名义额算 | 行业标准；按标记价而非成交价 |
| 结算方向 | 正费率：多头付空头；负费率：空头付多头 | 费率 > 0 表示永续溢价，多头需付费 |
| 账本编码 | `TransferOp{From: payer, To: receiver, ...}` + 幂等键 `perp:funding:<market>:<fundingIndex>:<account>` | 双方都记账；幂等键含轮次与账户 |
| 资金来源 | 资金费只从**可用余额**扣（不走 Holds），不足则从该账户的**保证金**中扣，扣完仍不足则**记欠账**并在下次结算补扣 | 避免因资金费触发 6a 的致命降级 |
| 标记价平滑 | 取「指数价 + 盘口中间价偏离」的最小/最大夹紧（±0.5%），防止盘口被单笔大单拉动 | dYdX v3 的 mark price 做法 |

---

## 3. 设计

### 3.1 `perp.PriceSource` 接口（服务层）

```go
// PriceSource supplies the external index price for a market. It is an
// interface so tests can inject deterministic prices, and production can swap
// in an aggregator (median of N venues) without touching the kernel.
type PriceSource interface {
    // Index returns the index price for the market, and false if no fresh
    // price is available (stale, missing, or all sources down).
    Index(ctx context.Context, m MarketID) (Price, bool)
}
```

内置实现：

- `StaticPriceSource`：测试与控制台用，价格从配置 `PERP_INDEX_PRICES=1:2000000000000000000;2:...` 读。
- `MedianPriceSource`：聚合多个 `PriceSource`，取中位数；少于 `quorum` 个来源可用时返回 false。

**陈旧判定**：`PriceSource` 只返回 `bool`，服务层不知道价格多旧。因此服务层给每次拉取记一个时戳，超过
`MaxPriceAge`（默认 60s）就视为不可用。不可用时：

- **标记价回退**：用上一次有效标记价（持续不超过 `MaxMarkStaleness`，默认 5 分钟）。
- 超过 5 分钟仍无价：**资金费跳过本轮**（不结算），**未实现盈亏显示为 0 并标记 `stale`**（展示层问题，
  不影响账本）。

### 3.2 标记价推导

```
mid = (bestBid + bestAsk) / 2        // 盘口中间价；单边空则取指数价
clamped = clamp(mid, index * (1 - maxDeviation), index * (1 + maxDeviation))
mark = (index * w_index + clamped * w_book) / (w_index + w_book)
```

默认 `maxDeviation = 0.5%`，`w_index = 1`，`w_book = 1`（各半）。`maxDeviation`、权重来自市场配置
（`Market.MarkConfig`，新增字段，不影响 6a 的 `Validate`）。

**盘口为空**：`mid = index`，`mark = index`。

**盘口只有一侧**：用该侧最佳价作为 mid，但仍夹紧。

### 3.3 资金费率

```
premium = (mark - index) / index                    // 有理数
rate    = clamp(premium / (FundingInterval / 1h), -maxRate, +maxRate)
```

`maxRate` 来自市场配置（默认 0.0075 = 0.75% 每小时，dYdX v3 同款）。

**计算精度**：全部用 `uint256` 整数运算，`premium` 用定点 `1e18` 表示：

```
premiumScaled = (mark - index) * 1e18 / index       // 有符号
rateScaled    = premiumScaled / intervalHours
rateScaled    = clamp(rateScaled, -maxRateScaled, +maxRateScaled)
```

`mark < index` 时 `premiumScaled` 为负，用带符号的 `math/big.Int` 处理（不影响确定性，仅计算期短暂使用）。

### 3.4 资金费结算

对每个市场、每个非零仓位：

```
notional := market.Notional(pos.Size, markPrice)      // quote
amount   := floor(notional * |rateScaled| / 1e18)     // quote
if rate > 0 && pos.Side == Long  →  payer = pos.Account
if rate > 0 && pos.Side == Short →  receiver = pos.Account
if rate < 0 && pos.Side == Long  →  receiver = pos.Account
if rate < 0 && pos.Side == Short →  payer = pos.Account
```

**一对多**：同一轮里可能有多个多头和多个空头。资金费**不是**多空之间直接配对，而是：

1. 多头应付总额 = Σ(多头名义额 × rate)
2. 空头应付总额 = Σ(空头名义额 × rate)（费率为负时相反）
3. **净额结算**：`totalPaid` 与 `totalReceived` 必须相等。因为多空总名义额可能不等（持仓不平衡），
   差额由**保险基金**吸收（6c 引入），6b 阶段：**差额从运营方账户扣/补**，并在事件中显式标记 `imbalanced`。

**简化裁定（6b v1）**：只对**可扣款**的账户结算。若某账户资金不足（可用余额 + 保证金都不够），
该账户本轮**记欠账**（`FundingDebt` 持久化到 WAL），下一轮优先补扣。欠账不触发致命降级。

### 3.5 指令日志扩展

新增 WAL 指令类型：

```json
{"kind":"mark","market":1,"index":"...","mark":"...","at":1234567890}
{"kind":"funding","market":1,"index":7,"rate":"-12500000000000000","mark":"...","at":...}
```

- `mark` 指令：每次拉取到有效价格后落盘，**用于重放时重建标记价历史**（供未实现盈亏与 6c 使用）。
- `funding` 指令：每轮资金费一条，包含本轮所有结算的 `(payer, receiver, amount, account)` 列表。
  重放时按条重放，账本按幂等键去重。

**为什么落盘而不是重拉**：重放必须确定。如果重放时重新拉 oracle，价格可能已变，导致账本与仓位不一致。

### 3.6 服务层改动

`PerpOrderBookService` 新增：

```go
type PerpOrderBookService struct {
    ...
    prices   PriceSource      // nil = 无标记价（6a 行为）
    mark     map[MarketID]MarkPrice   // 最新标记价
    funding  map[MarketID]FundingState // 上一轮资金费状态
    debts    map[fundingDebtKey]*uint256.Int
}

func (s *PerpOrderBookService) SetPriceSource(p PriceSource)
func (s *PerpOrderBookService) MarkPrice(m MarketID) (MarkPrice, bool)
func (s *PerpOrderBookService) FundingState(m MarketID) (FundingState, bool)
```

`Start()` 的 runner 回调改为**每 `PERP_TICK_INTERVAL`（默认 5s）拉一次价格**；资金费按市场各自的
`FundingInterval` 触发（记录 `nextFundingAt`，用单调计数而非墙钟——但资金费触发**允许**读时钟，
因为它不是撮合，只是周期性动作；落盘后的重放不依赖时钟）。

### 3.7 未实现盈亏改用标记价

新增纯函数（`perp` 包，不读外部状态）：

```go
func (e *Engine) UnrealizedPnL(m MarketID, p Position, mark Price) *uint256.Int {
    // long:  notional(size, mark) - notional(size, entry)
    // short: notional(size, entry) - notional(size, mark)
}
```

**已实现盈亏不受影响**：6a 的 `applyFill` 用成交价，这是正确的。

### 3.8 配置

| 键 | 含义 | 默认 |
|----|------|------|
| `PERP_INDEX_PRICES` | 静态指数价，`ID:PRICE;` 分隔（仅无真实源时使用） | 空 |
| `PERP_TICK_INTERVAL` | 价格拉取间隔（秒） | 5 |
| `PERP_FUNDING_INTERVAL` | 资金费周期（秒） | 3600 |
| `PERP_MAX_PRICE_AGE` | 价格陈旧阈值（秒） | 60 |
| `PERP_MAX_MARK_STALENESS` | 标记价回退最大时长（秒） | 300 |
| `PERP_MAX_FUNDING_RATE` | 资金费率上限（定点 1e18） | 7500000000000000（0.75%） |
| `PERP_MARK_MAX_DEVIATION` | 盘口价相对指数价最大偏离（定点 1e18） | 5000000000000000（0.5%） |

---

## 4. 测试策略

### 4.1 `perp` 单测

- `UnrealizedPnL`：多头/空头、盈利/亏损、零仓位。
- 标记价推导：盘口空/单边/双边；夹紧生效；权重。

### 4.2 `services` 单测

- 资金费结算：正/负费率；多头付/空头付；余额不足记欠账；下轮补扣；幂等。
- 价格陈旧：超过 `MaxPriceAge` 回退；超过 `MaxMarkStaleness` 跳过资金费。
- 重放：`mark` + `funding` 指令落盘后重放，账本一致。
- 6a 回归：不设 `PriceSource` 时行为与 6a 完全一致。

### 4.3 回归

全量 Go + Solidity 测试必须绿色。

---

## 5. 文件布局

```
backend/
  perp/
    price.go              # PriceSource, MarkPrice, 标记价推导
    price_test.go
    funding.go            # 资金费率计算（纯函数）
    funding_test.go
    engine.go             # + UnrealizedPnL
  services/
    perp_prices.go        # 服务层价格拉取与陈旧判定
    perp_prices_test.go
    perp_funding.go       # 资金费结算循环
    perp_funding_test.go
    perp_journal.go       # + mark/funding 指令类型
    perp_order_book.go    # + SetPriceSource / MarkPrice / FundingState
  config/config.go        # + PERP_* 配置
docs/superpowers/specs/2026-10-31-orderbook-dex-perp-funding-design.md   # 本文档
docs/superpowers/plans/2026-10-31-orderbook-dex-perp-funding.md          # 实施计划
```

---

## 6. 残余信任假设（必须写进 README）

1. **oracle 是运营方信任点**：v1 使用运营方配置的 `PriceSource`。链上 oracle 或多签喂价留给后续。
2. **资金费不平衡由运营方兜底**：多空名义额不等时，差额从运营方账户扣/补；6c 引入保险基金后改为保险基金。
3. **欠账可累积**：极端行情下账户可能累积资金费欠账，直到平仓时从盈亏中抵扣。6c 的强平会提前处理。

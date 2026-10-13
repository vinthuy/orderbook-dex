# 子项目 3：链下订单簿与撮合引擎 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在子项目 1/2 的账本与结算栈之上交付**交易所内核**：纯确定性撮合引擎（限价 GTC / 市价 IOC / 撤单 / 部分成交 / STP 撤旧单）、引擎侧资金冻结（账本零改动）、订单簿指令 WAL 与崩溃重放自愈，并让提现只能提取可用余额。

**Architecture:** 新增 `backend/matching`（纯内核：无 I/O、不读时钟、不用随机、不 import `services`）与 `backend/ledger/holds.go`（可用余额 = 余额 − 冻结）。`services/order_book.go` 把二者与账本、`EventBus` 装配成 `OrderBookService`，顺序固定为「冻结 → 落指令 WAL → 撮合 → 账本零和转移 → 释放冻结残额 → 发事件」。撮合结果必然是零和的 `TransferOp` 对，幂等键由确定性成交序号派生，故崩溃后重放即自愈。

**Tech Stack:** Go 1.25（module `web3-amm-dex-backend`）、`github.com/holiman/uint256`；沿用现有 `Service`/`ServiceRegistry`/`EventBus` 与 `ledger.Store` 的 WAL 约定。本子项目**不动 Solidity 合约**。

**Spec:** `docs/superpowers/specs/2026-10-13-orderbook-dex-matching-engine-design.md`（§4 内核、§5 冻结、§6 服务、§8 测试）
**前置分支:** `feat/matching-engine`，基于 `master` = `e80b679`（PR #1/#2/#3 已合入）

## 基线（已实测于 `e80b679`，改动前必须仍然成立）

```
go build ./...            # exit 0
go vet ./...              # exit 0
go test ./... -count=1    # ok api / config / ledger / services / tests/integration
npx hardhat test          # 33 passing（本子项目不改合约，必须保持 33）
```

## Global Constraints

- Go module 名固定 `web3-amm-dex-backend`；金额一律 `github.com/holiman/uint256`，JSON 中为十进制字符串。
- **`backend/matching` 不得** import `services`、不得调用 `time.Now()`、不得使用 `math/rand`/`crypto/rand`、不得触碰 `ledger.Ledger`（只允许用其 `AccountID`/`AssetID` 类型）。确定性是硬要求。
- **叶子编码、Merkle 建树、`Ledger` 现有语义与文件格式不得改动**（跨语言冻结）。本子项目**不新增** `Op` 类型、不改 `OpRecord` JSON。
- 命令在 Windows PowerShell 下执行：`&&` 不可用，用 `;` 分条执行；`$env:CGO_ENABLED='0'` 必须先设（本机无 gcc）。
- 测试位置：Go 用同包 `_test.go`。
- 提交信息前缀：`feat:` / `test:` / `fix:` / `docs:` / `chore:`。
- **本轮不做**（继续延后，勿顺手实现）：HTTP/WS API、鉴权限流、链上批量结算、订单 TTL/退出即自动撤单、手续费、post-only/FOK/止损、订单簿快照、保证金/永续。
- 关键裁定见 Spec §2.1 与 §10；实现时**不得**擅自交换「先账本转移后释放冻结」「先落日志后改订单簿」的顺序。

## File Structure

```
backend/
  ledger/holds.go, holds_test.go          # T1 新增
  matching/market.go, market_test.go      # T2 新增
  matching/order.go,  order_test.go       # T2 新增
  matching/book.go,   book_test.go        # T3 新增
  matching/engine.go, engine_test.go      # T4/T5/T6 新增
  matching/invariants_test.go             # T7 新增（不变量 + 确定性随机）
  services/journal.go, journal_test.go    # T8 新增（指令 WAL，复用尾损容忍规则）
  services/order_book.go                  # T9/T10 新增
  services/order_book_test.go             # T9/T10 新增
  services/settlement.go                  # T10 修改：Withdraw 经 Holds.Spend
  services/stack.go                       # T11 修改：装配 OrderBookService
  config/config.go                        # T11 修改：ORDERBOOK_* 配置
  README.md                               # T11 修改：冻结语义与残余假设
```

---

## 共享接口契约（本计划内固定，实现时不得擅自改名）

```go
// T1：backend/ledger/holds.go
type HoldKey struct { Account AccountID; Asset AssetID }

type Holds struct { /* 内部：mutex + ledger + map[HoldKey]*uint256.Int */ }

func NewHolds(l *Ledger) *Holds
func (h *Holds) Held(a AccountID, asset AssetID) *uint256.Int
func (h *Holds) Available(a AccountID, asset AssetID) *uint256.Int
func (h *Holds) Reserve(a AccountID, asset AssetID, amount *uint256.Int) error
func (h *Holds) Release(a AccountID, asset AssetID, amount *uint256.Int) error
func (h *Holds) Reconcile(open map[HoldKey]*uint256.Int) error
// Spend 在持有内部锁时校验「可用 ≥ amount」并执行 debit；与 Reserve 互斥。
func (h *Holds) Spend(a AccountID, asset AssetID, amount *uint256.Int, debit func() error) error

// T2：backend/matching（类型不得改名）
type (
	Side   uint8 // SideBuy, SideSell
	Type   uint8 // TypeLimit, TypeMarket
	TIF    uint8 // TIFGTC, TIFIOC
	Status uint8 // StatusNew, StatusPartiallyFilled, StatusFilled, StatusCancelled, StatusRejected
	OrderID uint64
	Seq     uint64
	Price   uint64
	TradeID uint64
	MarketID uint32
)

type Market struct {
	ID           MarketID
	Symbol       string
	Base, Quote  ledger.AssetID
	BaseDecimals uint8
	MinQty       *uint256.Int
}
func (m Market) Validate() error
func (m Market) Notional(qty *uint256.Int, p Price) *uint256.Int // floor(qty*p/10^BaseDecimals)
func (m Market) NotionalCeil(qty *uint256.Int, p Price) *uint256.Int

type Order struct {
	ID OrderID; Seq Seq; Market MarketID
	Account ledger.AccountID
	Side Side; Type Type; TIF TIF
	Price Price                  // 限价必填；市价为 0
	Qty, MaxNotional, Filled *uint256.Int
	Status Status
}
func (o *Order) Remaining() *uint256.Int
func (o *Order) IsOpen() bool

type Trade struct {
	ID TradeID; Market MarketID; Price Price
	Qty, Notional *uint256.Int
	MakerOrder, TakerOrder OrderID
	Maker, Taker ledger.AccountID
	MakerSide Side
}
// 幂等键（T9 使用）：fmt.Sprintf("trade:%d:base", t.ID) / ("trade:%d:quote", t.ID)

// T3/T4：backend/matching/engine.go
type PlaceRequest struct {
	Market MarketID; Account ledger.AccountID
	Side Side; Type Type; TIF TIF
	Price Price
	Qty, MaxNotional *uint256.Int
}

type HoldRelease struct { Account ledger.AccountID; Asset ledger.AssetID; Amount *uint256.Int }

type Result struct {
	Order    *Order
	Trades   []Trade
	Releases []HoldRelease
}

type LevelView struct { Price Price; Total *uint256.Int }

type Engine struct { /* 内部 */ }
func NewEngine(markets []Market) (*Engine, error)
func (e *Engine) Markets() []Market
func (e *Engine) RequiredReserve(req PlaceRequest) (asset ledger.AssetID, amount *uint256.Int, err error)
func (e *Engine) Place(req PlaceRequest) (Result, error)
func (e *Engine) Cancel(market MarketID, account ledger.AccountID, orderID OrderID) (Result, error)
func (e *Engine) Order(orderID OrderID) (*Order, bool)
func (e *Engine) OpenReserves() map[ledger.HoldKey]*uint256.Int   // 重放后重建冻结用
func (e *Engine) Book(market MarketID, depth int) (bids, asks []LevelView)
func (e *Engine) NextSeq() Seq                                   // 透明性/测试用

// 哨兵错误
var (
	ErrUnknownMarket  = errors.New("matching: unknown market")
	ErrBadRequest     = errors.New("matching: invalid order")
	ErrOrderNotFound  = errors.New("matching: order not found")
	ErrNotOwner       = errors.New("matching: not order owner")
)
```

**语义冻结（实现与测试都以本节为准）：**

- 买入冻结 `sum = ceil(Qty*Price/10^BaseDecimals)`（限价）或 `MaxNotional`（市价）。
  成交时按该笔 `Notional = floor(成交qty*Price/10^BaseDecimals)` 从 `reserved` 累减；
  离簿时把残额一次性释放。卖出冻结 `= Qty`，按成交 base 量累减。
- `Market + GTC` 非法（`NewEngine` 拒绝该市场？否——在 `Place` 校验时以 `ErrBadRequest` 拒绝，
  且 `market.Validate()` 只管市场本身）。
- STP：对手挂单 `Account` 与入单相同 ⇒ 撤销**对手挂单**（较旧者），释放其残额，入单继续撮合；
  入单自身不因 STP 撤销。
- IOC 残部不入簿；市价单恒 IOC；市价买在 `MaxNotional` 预算耗尽时停止撮合，残部撤销。
- `Price == 0` 的限价单以 `ErrBadRequest` 拒绝。

---

## Task 1：账本 — 冻结/可用余额 `ledger.Holds`

**Files:** `backend/ledger/holds.go`、`backend/ledger/holds_test.go`

- [ ] 按契约实现 `Holds`。`Available` 用 `max(0, balance − held)`；`Reserve` 先算可用，不足返回
      `ErrInsufficientBalance` 且**不改状态**；`Release` 减到 0 删除键，减成负数为内部错误（返回错误，
      不得静默钳零）；`Reconcile` 以传入 map 整体替换（跳过零值键）。
- [ ] `Spend` 在内部锁内完成「可用校验 + debit」，与 `Reserve` 互斥（Spec §5）。
- [ ] 测试（每条都要有能失败的断言）：
  - `Available` 在 `Reserve` 后立即扣除；`Release` 后恢复。
  - `Reserve` 超过可用 → `ErrInsufficientBalance` 且 `Held`/`Available` 不变。
  - `Release` 至 0 后键被删除；超额 `Release` 返回错误。
  - `Reconcile` 覆盖旧冻结额。
  - **并发互斥**：`Reserve` 与 `Spend` 并发（`sync.WaitGroup`，多 goroutine）后
    `Σ(成功 reserve) + Σ(成功 spend) ≤ 初始余额`（断言不超发）。
- [ ] `Spend` 的 debit 返回错误时不得扣减 `Held`（debit 是关闭动作）。

**验收：** `go test ./ledger/ -count=1` 全绿，新增用例覆盖上述各条；既有账本用例不受影响。

---

## Task 2：撮合 — 市场与订单类型

**Files:** `backend/matching/market.go`、`backend/matching/market_test.go`、`backend/matching/order.go`、`backend/matching/order_test.go`

- [ ] `Market.Validate()`：Base ≠ Quote、`BaseDecimals ≤ 76`（保证 `10^BaseDecimals` 溢不出 uint256）、
      `MinQty` 为空或非负；`Symbol` 非空。错误信息带市场 symbol。
- [ ] `Notional` = `floor(qty*p/10^BaseDecimals)`；`NotionalCeil` = 同式的向上取整。
      **两者都不得在 `p == 0` 时静默返回 0**（限价单不该走到这里；仍以测试固定行为）。
- [ ] `NewEngine` 校验市场集合：重复 `ID`、重复 `Symbol`、非法市场 → 返回错误（**必须失败启动**，不得跳过）。
- [ ] 测试：`Notional`/`NotionalCeil` 的边界（整除、余 1、`qty=0`）；`10^BaseDecimals` 在
      `BaseDecimals = 18 / 76 / 77` 的行为；`Validate` 各失败分支；`NewEngine` 重复 ID/Symbol。

**验收：** `go test ./matching/ -count=1` 全绿（本任务后 `matching` 包首次可编译）。

---

## Task 3：撮合 — 价格级与时间优先

**Files:** `backend/matching/book.go`、`backend/matching/book_test.go`

- [ ] 按 Spec §4.5 实现 `book`/`side`/`level`：买降序、卖升序的 `prices []Price`（二分插入/删除），
      `levels map[Price]*level`，级内 `*list.List` FIFO；订单持 `*list.Element` 以 O(1) 撤单。
- [ ] 维护 `level.total`；级空即从 `prices` 与 `levels` 同时移除（不残留空级）。
- [ ] 测试（`book` 为包内私有，用同包白盒测试）：
  - 插入乱序价格 → `prices` 严格有序；买降序、卖升序。
  - 同级多单 → FIFO 顺序 == `Seq` 升序。
  - 撤单（含表头、表尾、中间）→ `level.total` 精确减少、元素移除；级清空后价格级消失。
  - 随机插入/撤单序列后，`level.total == Σ(该级挂单剩余量)`、`prices` 与 `levels` 键集合一致。
- [ ] 不引入 `time`/`rand`（确定性）。

**验收：** `go test ./matching/ -count=1` 全绿，含随机一致性用例。

---

## Task 4：撮合 — 限价 GTC 与部分成交

**Files:** `backend/matching/engine.go`、`backend/matching/engine_test.go`

- [ ] `RequiredReserve` 实现 Spec §4.4（限价买 `ceil` / 限价卖 `Qty` / 市价买 `MaxNotional` /
      市价卖 `Qty`），纯函数不改状态。
- [ ] `Place` 校验 → 分配 `ID`/`Seq` → 撮合循环（价格约束 + 时间优先）→ GTC 残部入簿：
  - `Filled == 0` → `StatusNew`；`0 < Filled < Qty` → `StatusPartiallyFilled`；`Filled == Qty` → `StatusFilled`。
  - 校验失败 → `ErrBadRequest`（或 `ErrUnknownMarket`），**状态零改动、`Releases` 为空**。
  - 无对手可撮合且 GTC → 直接入簿，`Trades` 空。
- [ ] `Result.Releases` 汇总本单与对手挂单因成交/离簿而减少的冻结额（同一 `(account,asset)` 可
      出现多次，调用方按序累减即可；**不得**把一次成交的两条腿合并成一条）。
- [ ] 测试：
  - 单笔完全成交：双方 `Filled`、`Status`、`Trades` 字段（含 `Maker`/`Taker`/`MakerSide`/`Notional`）逐字段断言。
  - 对手挂单量小于入单 → 部分成交，入单残部入簿且成为该级 FIFO 尾部。
  - 价格优先：跨多个价格级吃单的顺序正确。
  - 时间优先：同级两笔挂单，先入者先成交。
  - **不可整除价位**：`BaseDecimals=18`，价位使 `notional` 产生余数；断言 `Notional` 恒等于公式、
    且成交后入单与对手的冻结残额分别为 `ceil` 语义下的正确值（**无资金泄漏**）。
  - `Qty = 0`、`Qty < MinQty`、限价 `Price = 0` → `ErrBadRequest` 且订单簿不变。

**验收：** `go test ./matching/ -count=1` 全绿。

---

## Task 5：撮合 — IOC 与市价单

**Files:** `backend/matching/engine.go`、`backend/matching/engine_test.go`

- [ ] `TIF = IOC`：残部**不入簿**，`Filled == Qty` → `StatusFilled`，否则 `StatusCancelled`。
- [ ] `Type = Market`：恒 IOC；`Price` 为 0，无价格约束。
  - 市价买**必须**给 `MaxNotional > 0`，否则 `ErrBadRequest`；
  - 市价买在预算耗尽时停止撮合：若下一笔整级成交会超预算，则按
    `floor(剩余预算 × 10^BaseDecimals / Price)` **裁量**只吃这个量；裁量后 base 为 0 则停止。
    **预算约束用 `已花费 + 本笔Notional ≤ MaxNotional` 判定**，`已花费 = Σ各笔 Notional`。
  - 市价卖无预算约束（`MaxNotional` 可空）。
- [ ] `Type = Market && TIF = GTC` → `ErrBadRequest`（在 `Place` 校验）。
- [ ] 测试：
  - IOC 限价单残部撤销且**不入簿**（`Book` 不出现该价级）。
  - 市价买恰好用尽预算（边界：`剩余预算 == 整笔 Notional`）→ 该笔成交，随后停止。
  - 市价买预算不足一笔 → 裁量成交；`floor` 后为 0 则零成交且 `Releases` 释放全部 `MaxNotional`。
  - 市价卖吃穿整本后残部撤销。
  - `Market + GTC`、市价买 `MaxNotional` 为空/0 → `ErrBadRequest`。
  - 市价买后该账户 `reserved` 归零（预算全花或撤销释放）。

**验收：** `go test ./matching/ -count=1` 全绿。

---

## Task 6：撮合 — STP（撤旧单）与撤单

**Files:** `backend/matching/engine.go`、`backend/matching/engine_test.go`

- [ ] STP：撮合循环中对手挂单 `Account == 入单 Account` ⇒ 撤销该挂单（`StatusCancelled`），
      把其残额并入 `Result.Releases`，**继续对下一张挂单撮合**；入单不因 STP 被撤销，
      被撤挂单不计入 `Result.Trades`。
- [ ] `Cancel`：仅可撤自己的单；未知/已离簿 → `ErrOrderNotFound`；他人 → `ErrNotOwner`；
      成功 → 离簿、`StatusCancelled`、残额进入 `Result.Releases`。
- [ ] 测试：
  - 自成交被阻止：同账户买挂单 + 卖入单 → 挂单被撤、`Trades` 为空、入单继续与其他人成交。
  - 被撤挂单的冻结额**全额**释放。
  - 撤单权限、未知单、重复撤单（第二次 `ErrOrderNotFound`）。
  - 撤单后该价格级在 `Book` 中消失（级空即移除）。

**验收：** `go test ./matching/ -count=1` 全绿。

---

## Task 7：撮合 — 不变量与确定性

**Files:** `backend/matching/invariants_test.go`

- [ ] 实现一个**纯确定性**的伪随机指令生成器（自己写一个线性同余/LFSR，**不用 `math/rand`**，
      固定种子），在多个种子下生成数百条 `Place`/`Cancel` 指令。
- [ ] 每轮结束后断言 Spec §4.7 全部不变量：
  1. `level.total == Σ(该级挂单剩余量)`；无空级残留。
  2. `side.prices` 与 `levels` 键集合一致且严格有序。
  3. 同级 `fifo` 顺序 == `Seq` 升序。
  4. 每张挂单 `0 < Filled ≤ Qty`、`Remaining > 0`。
  5. `reserved ≥ 0` 且 `reserved ≥ NotionalCeil(Remaining, Price)`（卖为 `reserved == Remaining`）。
  6. 每笔成交 `Notional == Market.Notional(Qty, Price)`。
  7. **确定性**：同种子新建两台引擎、喂同一指令序列（含被拒指令与撤单失败），
     `Trades` 逐字段相等、`Book(depth)` 相等、每单 `reserved` 相等、`NextSeq()` 相等。
- [ ] 另加一条**跨账户守恒**用例：给定每账户初始余额，按指令串算出所有成交的转移，
      断言任意资产 `Σ(账户余额变化) == 0`（成交只做零和转移）。
- [ ] 测试必须有能失败的能力：写完后**临时**破坏一条不变量（如级空不移除价格）确认用例变红，再还原。

**验收：** `go test ./matching/ -count=1` 全绿；破坏不变量时用例确实变红。

---

## Task 8：服务 — 指令 WAL `services/journal.go`

**Files:** `backend/services/journal.go`、`backend/services/journal_test.go`

- [ ] JSON 行格式 `{seq, kind:"place"|"cancel", payload}`；`kind` 未知或 `seq` 不递增必须报错
      （中间损坏绝不静默）。
- [ ] **复用子项目 2 的尾损容忍规则**（`backend/ledger/store.go` 的 `Replay` 已是该规则的参考实现）：
  末尾半条（最后一条非空行解析失败，或文件末字节非 `\n`）→ 忽略该条并告警；**中间**损坏 → 报错。
- [ ] 提供 `Append(entry) error`（单次 `Write` + `Sync`）、`Replay() ([]Entry, error)`、`Close() error`；
      `dir == ""` 时为内存实现（不落盘，`Replay` 返回空）。
- [ ] 测试：正常追加/回放；尾部截断半条 → 回放成功且忽略该条；中间注入非法 JSON → 报错；
      末字节非 `\n` → 成功忽略；`dir == ""` 的内存模式；`seq` 非递增 → 报错。

**验收：** `go test ./services/ -count=1` 全绿，新增用例覆盖上述各条。

---

## Task 9：服务 — `OrderBookService` 装配与结算

**Files:** `backend/services/order_book.go`、`backend/services/order_book_test.go`

- [ ] 实现 `services.Service`（`Name() == "OrderBookService"`），沿用 `runner` 约定：`Start` 重建通道、
      `Stop` 幂等、`Start→Stop→Start→Stop` 不 panic。
- [ ] `Place` 严格按 Spec §6.2 顺序：`RequiredReserve` → `holds.Reserve`（失败即拒、订单簿零改动）→
      `journal.Append` → `engine.Place` → 逐笔 `ledger.Apply(baseTransfer, quoteTransfer)` →
      逐条 `holds.Release` → `EventBus` 发布。
  - `engine.Place` 返回 `Rejected`/错误时：**回滚第 2 步的冻结**（`holds.Release` 全额）并返回错误。
  - 成交编码按 Spec §6.3：`TransferOp{From: seller, To: buyer, Asset: Base, Amount: Qty, Ref: "trade:<id>:base"}`
    与 `…Asset: Quote, Amount: Notional, Ref: "trade:<id>:quote"`，**同一 `Apply` 内原子提交**。
- [ ] `Cancel` 同构：`journal.Append` → `engine.Cancel` → `holds.Release` → 事件。
- [ ] 事件 topic 固定：`orderbook.order`（含 `order`, `status`）、`orderbook.trade`（含 `trade`）。
- [ ] 测试（用真实 `ledger.Ledger` + `NewHolds`，`dir == ""` 内存日志）：
  - 端到端两账户成交：断言双方 `Base`/`Quote` 余额精确变化、`Available` 正确；
  - **守恒**：成交前后 `TotalBalance(base)`、`TotalBalance(quote)` 不变；
  - 下单余额不足 → 拒绝且订单簿零改动（`Book` 为空、`Holds` 全零）；
  - 撤单释放冻结、可用余额恢复；
  - 事件在 `EventBus` 上按 topic 收到（含 `trade`）。
- [ ] 断言成交后**可用余额不被虚增**：成交前后买卖双方的 `Available` 差值只来自成交本身
      （Spec §5 的「先转移后释放」若被写反，此用例必须变红）。
      **注意（实现期修正）**：这一条曾经写错了——终态断言对顺序是**盲的**，因为转移与释放在终态
      下可交换。必须改用**观测点**验证：`OrderBookService` 保留非导出钩子 `beforeRelease`，在
      「账本已结算、冻结未释放」处回调；测试在该点断言 `mid + 释放额 == final` 且 `mid < final`
      （即可用余额被低估而非高估）。实测把两步对调后该用例变红，而端态用例仍通过。

**验收：** `go test ./services/ -count=1` 全绿。

---

## Task 10：服务 — 崩溃重放自愈、提现不动冻结、致命降级

**Files:** `backend/services/order_book.go`、`services/order_book_test.go`、`services/settlement.go`、`services/settlement_test.go`

- [ ] **重放自愈**：`Start`/构造时 `journal.Replay()` → 逐条重放（`RequiredReserve` 阶段跳过冻结，
      改由最终 `holds.Reconcile(engine.OpenReserves())` 一次性设定）→ 账本对已结算成交幂等跳过、
      对「已落盘未结算」的成交**补结算**。
- [ ] 测试：
  - 构造「指令已落盘、账本未结算」的日志（直接写 journal 或用注入的 `Apply` 失败制造）→
    新实例启动 → 账本被补结算、`Book(depth)`、`OpenReserves()`、每单 `reserved` 与崩溃前逐字段一致。
  - **重放不重复结算**：完整日志重放两遍，余额与冻结额不变。
  - **提现不能动冻结**：账户有挂单时，`SettlementService.Withdraw` 提现超过「可用」被拒
    （`ErrInsufficientBalance`），冻结部分不可提走；提现「可用」额度应成功。
  - **致命降级**：注入 `ledger.Apply` 失败 → 后续 `Place`/`Cancel` 返回 `ErrServiceDegraded`
    并发布 `orderbook.degraded` 事件。
- [ ] `SettlementService` 改造：新增可选 `HoldProvider`（`Held/Spend` 两个方法即可，
      建议直接接 `*ledger.Holds`）；`Withdraw` 的「可用校验 + `WithdrawOp`」整体走 `holds.Spend`。
      **未注入 `HoldProvider` 时行为不变**（既有提现用例必须全部保持通过）。
- [ ] `SettlementService` 的钉住逻辑（子项目 2 的 `withdrawals` 记录）**不得**被破坏：
      重试收敛、失败重发钉住 calldata、重复 ref 拒绝，三条既有用例必须仍绿。

**验收：** `go test ./services/ -count=1` 全绿；`go test ./... -count=1` 全绿。

---

## Task 11：配置、装配、文档与收尾

**Files:** `backend/config/config.go`、`services/stack.go`、`services/stack_test.go`、`backend/README.md`

- [ ] `Config` 增加 `OrderBookDataDir string`、`OrderBookMarkets string`；从 `ORDERBOOK_DATA_DIR`、
      `ORDERBOOK_MARKETS` 读取。市场串格式见 Spec §7：`ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS[:MIN_QTY]`，
      多市场以 `;` 分隔；**解析失败必须返回错误使启动失败**，不得静默跳过。
- [ ] 为 `SettlementStack` 增加 `OrderBook *OrderBookService`，`BuildSettlementStack` 在同一次装配中
      创建（与 `Settlement` 共用同一个 `*ledger.Ledger` 与同一个 `*ledger.Holds`），
      `Close()` 一并停止并关闭指令日志。未配置市场时不启用订单簿（保持向后兼容）。
- [ ] `stack_test.go`：装配出订单簿、`Close` 可重复调用不 panic、市场串解析失败时返回错误。
- [ ] `backend/README.md`：补「订单簿与冻结语义」「`ORDERBOOK_*` 环境变量」「残余信任假设
      （强制退出与挂单、运营方可撤单、指令日志单点）」三节。
- [ ] 最终复核：`go build ./... `、`go vet ./...`、`go test ./... -count=1`、`npx hardhat test`（必须仍 33）。

**验收：** 四条命令全绿；`ORDERBOOK_MARKETS` 缺失时 `main.go` 行为与 `e80b679` 一致。

---

## 风险与注意事项

1. **顺序即安全**：`先账本转移、后释放冻结` 与 `先落指令日志、后改订单簿` 是本子项目的两条安全
   顺序，写反会分别导致「提现与成交竞争使转移失败（半成交）」与「崩溃后订单簿无法重建」。
   两者都必须有能变红的测试（Task 9 的可用余额断言、Task 10 的重放用例）。
2. **伪随机测试不得引入 `math/rand`**：`matching` 的确定性是硬约束，随机生成器自带固定种子。
3. **`uint256` 别名陷阱**：`*uint256.Int` 是指针，函数返回必须返回副本（`new(uint256.Int).Set(x)`），
   否则一处修改会污染另一处状态；这是本子项目最容易出的静默 bug。
4. **幂等键与金额一致**：账本按键去重，键相同而金额不同会被静默忽略。成交金额由确定性撮合产出，
   Task 7 的确定性测试是该前提的守护；改动撮合公式必须同步跑 Task 7。
5. **不要顺手改账本与合约**：`Op`/`OpRecord`/叶子编码/`Settlement.sol` 均冻结，本子项目只加不改。
6. **`-race` 不可用**：本机无 gcc，`-race` 需 CGO；并发用例只能在无 `-race` 下跑，须在报告中如实说明，
   不得以未运行的验证冒充验证。

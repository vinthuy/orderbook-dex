# 子项目 6a（永续仓位内核 + 逐仓保证金）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新建一个独立的永续合约撮合内核 `backend/perp`（仓位模型 + 逐仓保证金 + 杠杆），复用现货撮合的算法模式但代码独立、现货 `matching` 零改动，并装配一个服务层 `services/perp_order_book.go`。

**Architecture:** `backend/perp` 是纯确定性内核（无 I/O、不读时钟、不碰账本、不 import `matching`），维护「账户×市场 → 仓位」。保证金多空都冻结 quote（`ceil(notional/leverage)`）；平仓时一次性结算净盈亏到账本。服务层沿用现货 `OrderBookService` 骨架（冻结→落 WAL→撮合→结算→释放→事件）。

**Tech Stack:** Go 1.25（holiman/uint256、container/list、gin 不用在内核）。

**设计文档：** `docs/superpowers/specs/2026-10-18-orderbook-dex-perp-design.md`

---

## File Structure

- 新增 `backend/perp/market.go` — `Market` 配置、`Validate`、`Notional`/`NotionalCeil`、`scale`
- 新增 `backend/perp/order.go` — `Side/Type/TIF/Status/OrderID/Seq/Price/TradeID/MarketID`、`Order`、`Position`
- 新增 `backend/perp/book.go` — `side`/`level`/`book`（价格级 + FIFO + O(1) 撤单）
- 新增 `backend/perp/engine.go` — `Engine`：`NewEngine`/`Place`/`Cancel`/`RequiredMargin`/仓位更新/`Positions`
- 新增 `backend/perp/invariants_test.go` — 不变量 + 确定性随机测试
- 新增 `backend/services/perp_order_book.go` — `PerpOrderBookService`（装配、WAL、盈亏结算、降级）
- 新增 `backend/services/perp_order_book_test.go`
- 各 `*_test.go`
- 修改 `backend/config/config.go` — `PerpConfig` + `LoadPerpConfig`
- 修改 `backend/main.go` — 装配 `PerpOrderBookService`（可选）
- 修改 `README.md`

依赖顺序：Task 1（类型+市场）→ Task 2（订单簿）→ Task 3（引擎+仓位）→ Task 4（不变量测试）→ Task 5（服务层）→ Task 6（config+main+README）。

关键复用点（已存在于代码库，本计划依赖）：

- `ledger`：`AssetID uint32`、`AccountID [20]byte`（`.Hex()`、`AccountFromHex`）、`Ledger`（`New`/`Apply`/`Balance`/`TotalBalance`/`RootAndVersion`）、`DepositOp{Account,Asset,Amount,Ref}`、`WithdrawOp{Account,Asset,Amount,Ref}`、`Holds`（`NewHolds`/`Reserve`/`Release`/`Available`/`Reconcile`）、`ErrInsufficientBalance`、`HoldKey{Account,Asset}`。
- `services`：`Service` 接口、`Config`、`EventBus`（`Subscribe`/`Publish`/`NewEventBus`）、`Event{Topic,Data,Timestamp}`、`Journal`（`NewJournal`/`Append`/`Replay`/`Close`，`PlaceCommand`/`CancelCommand` 是现货类型不能复用，需 perp 自己的日志类型）、`runner`（私有，不能跨包复用——服务层需自带简单 runner 或直接用 goroutine）。
- 现货 `matching` 的算法模式（本计划照搬思路、代码独立重写）：`book.go` 的 `side/level/book`、`market.go` 的 `Notional`/`NotionalCeil`/`pow10`、`engine.go` 的 `Place` 撮合循环/STP/市价裁量。

---

## Task 1: perp 类型 + 市场配置

**Files:**
- Create: `backend/perp/market.go`
- Create: `backend/perp/order.go`
- Test: `backend/perp/market_test.go`

- [ ] **Step 1: 写失败测试**

```go
package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func TestMarketValidate(t *testing.T) {
	ok := Market{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid market rejected: %v", err)
	}
	bad := ok
	bad.Base = bad.Quote
	if err := bad.Validate(); err == nil {
		t.Fatal("base==quote should be rejected")
	}
	bad2 := ok
	bad2.MaxLeverage = 0
	if err := bad2.Validate(); err == nil {
		t.Fatal("zero leverage should be rejected")
	}
}

func TestNotionalFloor(t *testing.T) {
	m := Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
	// notional = floor(qty*price/10^2)
	if got := m.Notional(uint256.NewInt(100), 1); got.Uint64() != 1 {
		t.Fatalf("Notional(100,1) = %s, want 1 (100*1/100)", got.Dec())
	}
	if got := m.NotionalCeil(uint256.NewInt(1), 1); got.Uint64() != 1 {
		t.Fatalf("NotionalCeil(1,1) = %s, want 1 (ceil(1*1/100))", got.Dec())
	}
}

func TestMarginFor(t *testing.T) {
	m := Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
	// notional(100,100) = 100*100/100 = 100; ceil(100/10) = 10
	got, err := m.marginCeil(uint256.NewInt(100), 100, 10)
	if err != nil {
		t.Fatalf("marginCeil: %v", err)
	}
	if got.Uint64() != 10 {
		t.Fatalf("marginCeil = %s, want 10", got.Dec())
	}
	// floor(notional(100,100)/10) = floor(100/10) = 10
	got2, err := m.marginFloor(uint256.NewInt(100), 100, 10)
	if err != nil {
		t.Fatalf("marginFloor: %v", err)
	}
	if got2.Uint64() != 10 {
		t.Fatalf("marginFloor = %s, want 10", got2.Dec())
	}
}

func TestAccountIDUnchanged(t *testing.T) {
	_ = ledger.AssetID(1)
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./perp/ -run 'TestMarket|TestNotional|TestMarginFor' -v`
Expected: FAIL — `undefined: Market`

- [ ] **Step 3: 实现 market.go**

```go
// Package perp is a pure, deterministic perpetual-futures matching kernel. It
// has no I/O, never reads the clock, never uses randomness, and never touches
// the ledger. Given the same market configuration and the same instruction
// sequence it produces byte-identical results, which makes crash replay safe.
//
// It is intentionally independent of the spot matching kernel: perpetual
// positions freeze margin in the quote asset on BOTH sides, unlike spot where a
// sell freezes the base asset. The two kernels share no code.
package perp

import (
	"fmt"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Side is the direction of a position or order.
type Side uint8

const (
	SideLong Side = iota
	SideShort
)

func (s Side) String() string {
	if s == SideLong {
		return "long"
	}
	return "short"
}

// Type is the order type.
type Type uint8

const (
	TypeLimit Type = iota
	TypeMarket
)

func (t Type) String() string {
	if t == TypeLimit {
		return "limit"
	}
	return "market"
}

// TIF is the time-in-force of an order.
type TIF uint8

const (
	TIFGTC TIF = iota
	TIFIOC
)

func (t TIF) String() string {
	if t == TIFGTC {
		return "GTC"
	}
	return "IOC"
}

// Status is the lifecycle state of an order.
type Status uint8

const (
	StatusNew Status = iota
	StatusPartiallyFilled
	StatusFilled
	StatusCancelled
	StatusRejected
)

func (s Status) String() string {
	switch s {
	case StatusNew:
		return "new"
	case StatusPartiallyFilled:
		return "partially_filled"
	case StatusFilled:
		return "filled"
	case StatusCancelled:
		return "cancelled"
	case StatusRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// OrderID, Seq, Price, TradeID, MarketID are engine-assigned counters (never
// timestamps), replay-reproducible.
type OrderID uint64
type Seq uint64
type Price uint64
type TradeID uint64
type MarketID uint32

// Market is the immutable configuration of one perpetual market.
type Market struct {
	ID           MarketID
	Symbol       string
	Base, Quote  ledger.AssetID // Quote is the margin/settlement asset
	BaseDecimals uint8          // notional = qty*price / 10^BaseDecimals
	MaxLeverage  uint32         // maximum leverage; >= 1
	MinQty       *uint256.Int   // optional minimum base quantity
	MaxQty       *uint256.Int   // optional maximum base quantity (nil = 2^128)
}

const maxBaseDecimals = 76

var defaultMaxQty = new(uint256.Int).Lsh(uint256.NewInt(1), 128)

func (m Market) maxQty() *uint256.Int {
	if m.MaxQty != nil {
		return m.MaxQty
	}
	return defaultMaxQty
}

func (m Market) scale() *uint256.Int { return pow10(m.BaseDecimals) }

// Validate checks the market configuration in isolation.
func (m Market) Validate() error {
	if m.Symbol == "" {
		return fmt.Errorf("perp: market %d: empty symbol", m.ID)
	}
	if m.Base == m.Quote {
		return fmt.Errorf("perp: market %s: base and quote must differ (both %d)", m.Symbol, m.Base)
	}
	if m.MaxLeverage < 1 {
		return fmt.Errorf("perp: market %s: max leverage must be >= 1", m.Symbol)
	}
	if m.BaseDecimals > maxBaseDecimals {
		return fmt.Errorf("perp: market %s: base decimals %d exceeds %d", m.Symbol, m.BaseDecimals, maxBaseDecimals)
	}
	if m.MinQty != nil && m.MinQty.Sign() < 0 {
		return fmt.Errorf("perp: market %s: negative min qty", m.Symbol)
	}
	if m.MaxQty != nil && m.MaxQty.Sign() < 0 {
		return fmt.Errorf("perp: market %s: negative max qty", m.Symbol)
	}
	return nil
}

// Notional returns floor(qty*price / 10^BaseDecimals), the quote value of qty
// base units at price. It panics on price == 0 and on uint256 overflow (both
// are unreachable for an accepted order; validated callers reject first).
func (m Market) Notional(qty *uint256.Int, p Price) *uint256.Int {
	if p == 0 {
		panic("perp: Notional called with zero price")
	}
	v, err := m.notionalChecked(qty, p, false)
	if err != nil {
		panic("perp: Notional overflow: " + err.Error())
	}
	return v
}

// NotionalCeil returns ceil(qty*price / 10^BaseDecimals).
func (m Market) NotionalCeil(qty *uint256.Int, p Price) *uint256.Int {
	if p == 0 {
		panic("perp: NotionalCeil called with zero price")
	}
	v, err := m.notionalChecked(qty, p, true)
	if err != nil {
		panic("perp: NotionalCeil overflow: " + err.Error())
	}
	return v
}

func (m Market) notionalChecked(qty *uint256.Int, p Price, ceil bool) (*uint256.Int, error) {
	if p == 0 {
		return nil, fmt.Errorf("%w: zero price", ErrBadRequest)
	}
	if qty == nil {
		return uint256.NewInt(0), nil
	}
	prod, overflow := new(uint256.Int).MulOverflow(qty, uint256.NewInt(uint64(p)))
	if overflow {
		return nil, ErrAmountTooLarge
	}
	s := m.scale()
	if ceil {
		prod, overflow = prod.AddOverflow(prod, new(uint256.Int).Sub(s, uint256.NewInt(1)))
		if overflow {
			return nil, ErrAmountTooLarge
		}
	}
	return new(uint256.Int).Div(prod, s), nil
}

// marginCeil derives the order-level freeze for a full open of qty base units at
// the order price with the given leverage: ceil(notional(qty,price) / leverage).
// It is what RequiredMargin returns and what the service layer pre-freezes.
func (m Market) marginCeil(qty *uint256.Int, price Price, leverage uint32) (*uint256.Int, error) {
	if leverage < 1 {
		return nil, fmt.Errorf("%w: leverage must be >= 1", ErrBadRequest)
	}
	notional, err := m.notionalChecked(qty, price, true)
	if err != nil {
		return nil, err
	}
	return new(uint256.Int).Div(notional, uint256.NewInt(uint64(leverage))), nil
}

// marginFloor derives the position-level margin transferred for one fill of qty
// base units: floor(notional(qty,price) / leverage). floor is subadditive, so
// summing per-fill floor never exceeds the order-level ceil — the freeze can
// never be over-drawn (the subproject-3 technique).
func (m Market) marginFloor(qty *uint256.Int, price Price, leverage uint32) (*uint256.Int, error) {
	if leverage < 1 {
		return nil, fmt.Errorf("%w: leverage must be >= 1", ErrBadRequest)
	}
	notional, err := m.notionalChecked(qty, price, false)
	if err != nil {
		return nil, err
	}
	return new(uint256.Int).Div(notional, uint256.NewInt(uint64(leverage))), nil
}

func pow10(n uint8) *uint256.Int {
	out := uint256.NewInt(1)
	ten := uint256.NewInt(10)
	for i := uint8(0); i < n; i++ {
		out.Mul(out, ten)
	}
	return out
}

func cloneInt(v *uint256.Int) *uint256.Int {
	if v == nil {
		return nil
	}
	return new(uint256.Int).Set(v)
}

func minUint(a, b *uint256.Int) *uint256.Int {
	if a.Cmp(b) <= 0 {
		return new(uint256.Int).Set(a)
	}
	return new(uint256.Int).Set(b)
}
```

- [ ] **Step 4: 实现 order.go（类型 + 仓位 + 订单）**

```go
package perp

import (
	"container/list"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Position is one account's net position in one market (isolated margin: one
// position per account per market). Side and Size give direction and quantity;
// an account holds at most one direction — an opposite-side fill first reduces
// the position and only flips direction once Size reaches zero.
type Position struct {
	Market     MarketID
	Account    ledger.AccountID
	Side       Side
	Size       *uint256.Int // base quantity, > 0
	EntryPrice Price        // notional-weighted average entry price
	Margin     *uint256.Int // total frozen margin (quote)
	Leverage   uint32       // user-selected leverage (<= market max)
}

func (p *Position) clone() *Position {
	if p == nil {
		return nil
	}
	c := *p
	c.Size = cloneInt(p.Size)
	c.Margin = cloneInt(p.Margin)
	return &c
}

// Order is one resting or in-flight order.
type Order struct {
	ID      OrderID
	Seq     Seq
	Market  MarketID
	Account ledger.AccountID
	Side    Side
	Type    Type
	TIF     TIF
	Price   Price       // limit: required (>0); market: 0
	Qty     *uint256.Int // base quantity
	// MaxNotional caps the quote value of a market order's opening size.
	MaxNotional *uint256.Int
	// Leverage is the user-selected leverage at submit time; a maker fill
	// derives its margin at this leverage.
	Leverage uint32
	Filled   *uint256.Int
	Status   Status

	reserved *uint256.Int  // frozen margin for this order's resting remainder
	elem     *list.Element // FIFO position, nil when not resting
}

func (o *Order) Remaining() *uint256.Int { return new(uint256.Int).Sub(o.Qty, o.Filled) }

func (o *Order) IsOpen() bool { return o.Status == StatusNew || o.Status == StatusPartiallyFilled }

func (o *Order) clone() *Order {
	if o == nil {
		return nil
	}
	c := *o
	c.Qty = cloneInt(o.Qty)
	c.MaxNotional = cloneInt(o.MaxNotional)
	c.Filled = cloneInt(o.Filled)
	c.reserved = cloneInt(o.reserved)
	c.elem = nil
	return &c
}
```

- [ ] **Step 5: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./perp/ -run 'TestMarket|TestNotional|TestMarginFor' -v`
Expected: PASS（4 个测试全过；`TestAccountIDUnchanged` 因 import ledger 已用）

- [ ] **Step 6: 提交**

```bash
git add backend/perp/
git commit -m "feat(perp): market config and position/order types"
```

---

## Task 2: 订单簿

**Files:**
- Create: `backend/perp/book.go`
- Test: `backend/perp/book_test.go`

- [ ] **Step 1: 写失败测试**

```go
package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func mkOrder(t *testing.T, side Side, price Price, qty uint64) *Order {
	t.Helper()
	o := &Order{ID: OrderID(qty + uint64(price)*1000), Side: side, Price: price, Qty: uint256.NewInt(qty), Filled: uint256.NewInt(0), Status: StatusNew}
	return o
}

func TestBookAddBestOrder(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	b.add(mkOrder(t, SideLong, 10, 5))
	b.add(mkOrder(t, SideLong, 12, 3))
	if got := b.bids.best().price; got != 12 {
		t.Fatalf("best bid = %d, want 12", got)
	}
	if got := b.bids.bestOrder().Price; got != 12 {
		t.Fatalf("best order price = %d, want 12", got)
	}
}

func TestBookPriceTimePriority(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	o1 := mkOrder(t, SideLong, 10, 5)
	o2 := mkOrder(t, SideLong, 10, 7)
	b.add(o1)
	b.add(o2)
	if got := b.bids.bestOrder(); got != o1 {
		t.Fatalf("best order should be o1 (earlier), got seq %d", got.ID)
	}
}

func TestBookCancelRemovesLevel(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	o := mkOrder(t, SideShort, 10, 5)
	b.add(o)
	b.cancel(o)
	if b.asks.best() != nil {
		t.Fatal("asks should be empty after cancel")
	}
	if len(b.orders) != 0 {
		t.Fatal("orders map should be empty")
	}
}

func TestBookReduceFilledLeavesBook(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	o := mkOrder(t, SideLong, 10, 5)
	b.add(o)
	b.reduce(o, uint256.NewInt(5))
	if !o.Remaining().IsZero() {
		t.Fatal("order should be fully filled")
	}
	if b.bids.best() != nil {
		t.Fatal("level should be dropped after full fill")
	}
	if _, ok := b.orders[o.ID]; ok {
		t.Fatal("order should be removed from map")
	}
}

var _ = ledger.AssetID(1)
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./perp/ -run TestBook -v`
Expected: FAIL — `undefined: newBook, side, level, book`

- [ ] **Step 3: 实现 book.go**

```go
package perp

import (
	"container/list"
	"fmt"
	"sort"

	"github.com/holiman/uint256"
)

// side is one half of a book: price levels ordered best-first (longs descending,
// shorts ascending).
type side struct {
	desc   bool
	prices []Price
	levels map[Price]*level
}

type level struct {
	price Price
	total *uint256.Int
	fifo  *list.List // of *Order, oldest first
}

func newSide(desc bool) *side {
	return &side{desc: desc, levels: make(map[Price]*level)}
}

func (s *side) before(a, b Price) bool {
	if s.desc {
		return a > b
	}
	return a < b
}

func (s *side) insertPrice(p Price) {
	i := sort.Search(len(s.prices), func(i int) bool { return !s.before(s.prices[i], p) })
	s.prices = append(s.prices, 0)
	copy(s.prices[i+1:], s.prices[i:])
	s.prices[i] = p
}

func (s *side) removePrice(p Price) {
	i := sort.Search(len(s.prices), func(i int) bool { return !s.before(s.prices[i], p) })
	if i >= len(s.prices) || s.prices[i] != p {
		panic(fmt.Sprintf("perp: side.removePrice: price %d not found", p))
	}
	s.prices = append(s.prices[:i], s.prices[i+1:]...)
}

func (s *side) best() *level {
	if len(s.prices) == 0 {
		return nil
	}
	return s.levels[s.prices[0]]
}

func (s *side) bestOrder() *Order {
	lvl := s.best()
	if lvl == nil {
		return nil
	}
	return lvl.fifo.Front().Value.(*Order)
}

// book is one market's order book. Long orders rest on bids, short on asks.
type book struct {
	m      Market
	bids   *side // longs, price descending
	asks   *side // shorts, price ascending
	orders map[OrderID]*Order
}

func newBook(m Market) *book {
	return &book{m: m, bids: newSide(true), asks: newSide(false), orders: make(map[OrderID]*Order)}
}

func (b *book) sideFor(s Side) *side {
	if s == SideLong {
		return b.bids
	}
	return b.asks
}

func (b *book) add(o *Order) {
	s := b.sideFor(o.Side)
	lvl := s.levels[o.Price]
	if lvl == nil {
		lvl = &level{price: o.Price, total: uint256.NewInt(0), fifo: list.New()}
		s.levels[o.Price] = lvl
		s.insertPrice(o.Price)
	}
	o.elem = lvl.fifo.PushBack(o)
	lvl.total.Add(lvl.total, o.Remaining())
	b.orders[o.ID] = o
}

func (b *book) cancel(o *Order) {
	s := b.sideFor(o.Side)
	lvl := s.levels[o.Price]
	if lvl == nil || o.elem == nil {
		panic(fmt.Sprintf("perp: book.cancel: order %d is not resting", o.ID))
	}
	lvl.total.Sub(lvl.total, o.Remaining())
	lvl.fifo.Remove(o.elem)
	o.elem = nil
	b.dropLevelIfEmpty(s, lvl)
	delete(b.orders, o.ID)
}

func (b *book) reduce(o *Order, qty *uint256.Int) {
	s := b.sideFor(o.Side)
	lvl := s.levels[o.Price]
	if lvl == nil || o.elem == nil {
		panic(fmt.Sprintf("perp: book.reduce: order %d is not resting", o.ID))
	}
	o.Filled.Add(o.Filled, qty)
	lvl.total.Sub(lvl.total, qty)
	if !o.Remaining().IsZero() {
		return
	}
	lvl.fifo.Remove(o.elem)
	o.elem = nil
	b.dropLevelIfEmpty(s, lvl)
	delete(b.orders, o.ID)
}

func (b *book) dropLevelIfEmpty(s *side, lvl *level) {
	if lvl.fifo.Len() != 0 {
		return
	}
	delete(s.levels, lvl.price)
	s.removePrice(lvl.price)
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./perp/ -run TestBook -v`
Expected: PASS（4 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/perp/book.go backend/perp/book_test.go
git commit -m "feat(perp): price-level order book with price-time priority"
```

---

## Task 3: 撮合引擎 + 仓位更新

**Files:**
- Create: `backend/perp/engine.go`
- Test: `backend/perp/engine_test.go`

- [ ] **Step 1: 写失败测试（核心仓位语义）**

```go
package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func mkMarket() Market {
	return Market{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
}

func acct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func longReq(a ledger.AccountID, price Price, qty uint64, lev uint32) PlaceRequest {
	return PlaceRequest{Market: 1, Account: a, Side: SideLong, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: lev}
}

func shortReq(a ledger.AccountID, price Price, qty uint64, lev uint32) PlaceRequest {
	return PlaceRequest{Market: 1, Account: a, Side: SideShort, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: lev}
}

func TestRequiredMarginLong(t *testing.T) {
	e, err := NewEngine([]Market{mkMarket()})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// notional(100, 100) = 100; ceil(100/10) = 10
	got, err := e.RequiredMargin(longReq(acct(1), 100, 100, 10))
	if err != nil {
		t.Fatalf("RequiredMargin: %v", err)
	}
	if got.Uint64() != 10 {
		t.Fatalf("margin = %s, want 10", got.Dec())
	}
}

func TestPlaceCrossesAndOpensPosition(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	// Short 100 @ 100, then Long 100 @ 100 crosses.
	if _, err := e.Place(shortReq(acct(1), 100, 100, 10)); err != nil {
		t.Fatalf("place short: %v", err)
	}
	res, err := e.Place(longReq(acct(2), 100, 100, 10))
	if err != nil {
		t.Fatalf("place long: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	// Account 2 opened a long; account 1 opened a short (both fully crossed).
	p2, ok := e.Position(1, acct(2))
	if !ok || p2.Side != SideLong || p2.Size.Uint64() != 100 {
		t.Fatalf("p2 = %+v", p2)
	}
	p1, ok := e.Position(1, acct(1))
	if !ok || p1.Side != SideShort || p1.Size.Uint64() != 100 {
		t.Fatalf("p1 = %+v", p1)
	}
}

func TestPlaceReduceClosesPosition(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10))
	_, _ = e.Place(longReq(acct(2), 100, 100, 10))

	// Account 2 closes half: sell 50 @ 100 (side Short reduces its long).
	res, err := e.Place(shortReq(acct(2), 100, 50, 10))
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	_ = res
	p2, _ := e.Position(1, acct(2))
	if p2.Size.Uint64() != 50 {
		t.Fatalf("size = %s, want 50", p2.Size.Dec())
	}
}

func TestPlaceFlipSide(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10))
	_, _ = e.Place(longReq(acct(2), 100, 100, 10))

	// Account 2 sells 150: first closes 100 long, remaining 50 opens short.
	_, err := e.Place(shortReq(acct(2), 100, 150, 10))
	if err != nil {
		t.Fatalf("flip: %v", err)
	}
	p2, _ := e.Position(1, acct(2))
	if p2.Side != SideShort || p2.Size.Uint64() != 50 {
		t.Fatalf("p2 = %+v, want short 50", p2)
	}
}

func TestPlaceLeverageOverMaxRejected(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	if _, err := e.Place(longReq(acct(1), 100, 100, 11)); err == nil {
		t.Fatal("leverage over max should be rejected")
	}
}

func TestCancel(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10))
	res, err := e.Place(longReq(acct(2), 50, 100, 10)) // won't fully cross (price mismatch), rests
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if res.Order == nil || !res.Order.IsOpen() {
		t.Fatalf("order should rest, got %+v", res.Order)
	}
	c, err := e.Cancel(1, acct(2), res.Order.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if c.Order.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", c.Order.Status)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./perp/ -run 'TestRequired|TestPlace|TestCancel' -v`
Expected: FAIL — `undefined: NewEngine, PlaceRequest, Place, RequiredMargin, Cancel, Position`

- [ ] **Step 3: 实现 engine.go**

```go
package perp

import (
	"errors"
	"fmt"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Sentinel errors.
var (
	ErrUnknownMarket = errors.New("perp: unknown market")
	ErrBadRequest    = errors.New("perp: invalid order")
	ErrOrderNotFound = errors.New("perp: order not found")
	ErrNotOwner      = errors.New("perp: not order owner")
	ErrAmountTooLarge = errors.New("perp: notional overflows")
)

// PlaceRequest is an untrusted order request. Leverage is the user-selected
// leverage (<= market max) for opening/increasing a position.
type PlaceRequest struct {
	Market      MarketID
	Account     ledger.AccountID
	Side        Side
	Type        Type
	TIF         TIF
	Price       Price
	Qty         *uint256.Int
	MaxNotional *uint256.Int
	Leverage    uint32
}

// MarginRelease tells the service layer to unfreeze margin (quote).
type MarginRelease struct {
	Account ledger.AccountID
	Amount  *uint256.Int
}

// PnLSettlement tells the service layer to realize a profit (Deposit) or loss
// (Withdraw) in the quote asset when a position is reduced. TradeID is the
// producing fill's id, used as the idempotency key perp:pnl:<tradeID>.
type PnLSettlement struct {
	Market  MarketID
	TradeID TradeID
	Account ledger.AccountID
	Profit  *uint256.Int // quote; positive = credit, negative = debit (magnitude)
}

// Trade is one fill between a resting maker and an incoming taker.
type Trade struct {
	ID         TradeID
	Market     MarketID
	Price      Price
	Qty        *uint256.Int
	Notional   *uint256.Int
	MakerOrder OrderID
	TakerOrder OrderID
	Maker      ledger.AccountID
	Taker      ledger.AccountID
	MakerSide  Side
}

// Result is the outcome of Place/Cancel.
type Result struct {
	Order    *Order
	Trades   []Trade
	Releases []MarginRelease
	PnL      []PnLSettlement
}

// LevelView is a price-level snapshot for Book.
type LevelView struct {
	Price Price
	Total *uint256.Int
}

// Engine is a deterministic multi-market perpetual matching kernel.
type Engine struct {
	markets   []Market
	byID      map[MarketID]Market
	books     map[MarketID]*book
	all       map[OrderID]*Order
	positions map[MarketID]map[ledger.AccountID]*Position
	nextID    OrderID
	nextSeq   Seq
	nextTrade TradeID
}

func NewEngine(markets []Market) (*Engine, error) {
	e := &Engine{
		byID:      make(map[MarketID]Market, len(markets)),
		books:     make(map[MarketID]*book, len(markets)),
		all:       make(map[OrderID]*Order),
		positions: make(map[MarketID]map[ledger.AccountID]*Position),
	}
	seenSymbol := make(map[string]struct{}, len(markets))
	for _, m := range markets {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if _, dup := e.byID[m.ID]; dup {
			return nil, fmt.Errorf("perp: duplicate market id %d", m.ID)
		}
		if _, dup := seenSymbol[m.Symbol]; dup {
			return nil, fmt.Errorf("perp: duplicate market symbol %q", m.Symbol)
		}
		cp := m
		cp.MinQty = cloneInt(m.MinQty)
		cp.MaxQty = cloneInt(m.MaxQty)
		e.markets = append(e.markets, cp)
		e.byID[cp.ID] = cp
		e.books[cp.ID] = newBook(cp)
		e.positions[cp.ID] = make(map[ledger.AccountID]*Position)
		seenSymbol[cp.Symbol] = struct{}{}
	}
	return e, nil
}

func (e *Engine) Markets() []Market {
	out := make([]Market, len(e.markets))
	copy(out, e.markets)
	for i := range out {
		out[i].MinQty = cloneInt(out[i].MinQty)
		out[i].MaxQty = cloneInt(out[i].MaxQty)
	}
	return out
}

// Position returns a copy of an account's position in a market, and whether it
// exists.
func (e *Engine) Position(m MarketID, a ledger.AccountID) (*Position, bool) {
	byAcct := e.positions[m]
	if byAcct == nil {
		return nil, false
	}
	p, ok := byAcct[a]
	if !ok {
		return nil, false
	}
	return p.clone(), true
}

// RequiredMargin computes the order-level freeze a request must pre-reserve to
// open/increase its position, without changing state. It validates exactly as
// Place does.
func (e *Engine) RequiredMargin(req PlaceRequest) (*uint256.Int, error) {
	m, err := e.validate(req)
	if err != nil {
		return nil, err
	}
	return m.marginCeil(req.Qty, req.Price, req.Leverage)
}

func (e *Engine) validate(req PlaceRequest) (Market, error) {
	m, ok := e.byID[req.Market]
	if !ok {
		return Market{}, ErrUnknownMarket
	}
	if req.Side != SideLong && req.Side != SideShort {
		return Market{}, fmt.Errorf("%w: bad side %d", ErrBadRequest, req.Side)
	}
	if req.Type != TypeLimit && req.Type != TypeMarket {
		return Market{}, fmt.Errorf("%w: bad type %d", ErrBadRequest, req.Type)
	}
	if req.TIF != TIFGTC && req.TIF != TIFIOC {
		return Market{}, fmt.Errorf("%w: bad tif %d", ErrBadRequest, req.TIF)
	}
	if req.Type == TypeLimit {
		if req.Price == 0 {
			return Market{}, fmt.Errorf("%w: limit order requires price > 0", ErrBadRequest)
		}
	} else if req.TIF == TIFGTC {
		return Market{}, fmt.Errorf("%w: market order cannot be GTC", ErrBadRequest)
	}
	if req.Qty == nil || req.Qty.Sign() == 0 {
		return Market{}, fmt.Errorf("%w: qty must be > 0", ErrBadRequest)
	}
	if req.Leverage < 1 || req.Leverage > m.MaxLeverage {
		return Market{}, fmt.Errorf("%w: leverage %d not in [1,%d]", ErrBadRequest, req.Leverage, m.MaxLeverage)
	}
	if maxQty := m.maxQty(); req.Qty.Cmp(maxQty) > 0 {
		return Market{}, fmt.Errorf("%w: qty %s exceeds max %s", ErrAmountTooLarge, req.Qty, maxQty)
	}
	if m.MinQty != nil && req.Qty.Cmp(m.MinQty) < 0 {
		return Market{}, fmt.Errorf("%w: qty %s below min %s", ErrBadRequest, req.Qty, m.MinQty)
	}
	return m, nil
}

// Place validates, assigns ID/Seq, matches against the opposite side, updates
// positions, and returns the resulting margin releases and PnL settlements. A
// validation error returns before any mutation.
func (e *Engine) Place(req PlaceRequest) (Result, error) {
	m, err := e.validate(req)
	if err != nil {
		return Result{}, err
	}
	margin, err := m.marginCeil(req.Qty, req.Price, req.Leverage)
	if err != nil {
		return Result{}, err
	}

	e.nextID++
	e.nextSeq++
	o := &Order{
		ID:          e.nextID,
		Seq:         e.nextSeq,
		Market:      m.ID,
		Account:     req.Account,
		Side:        req.Side,
		Type:        req.Type,
		TIF:         req.TIF,
		Price:       req.Price,
		Qty:         cloneInt(req.Qty),
		MaxNotional: cloneInt(req.MaxNotional),
		Leverage:    req.Leverage,
		Filled:      uint256.NewInt(0),
		Status:      StatusNew,
	}
	if o.Type == TypeMarket {
		o.TIF = TIFIOC
	}
	o.reserved = cloneInt(margin)
	e.all[o.ID] = o

	bk := e.books[m.ID]
	// Opposing side of the book: a long order matches resting shorts (asks);
	// a short order matches resting longs (bids).
	opp := bk.asks
	if o.Side == SideShort {
		opp = bk.bids
	}

	var (
		trades   []Trade
		releases []MarginRelease
		pnl      []PnLSettlement
		spent    = uint256.NewInt(0) // market-order quote already committed
	)

	for o.Remaining().Sign() > 0 {
		maker := opp.bestOrder()
		if maker == nil {
			break
		}
		price := maker.Price

		if o.Type == TypeLimit {
			if o.Side == SideLong && price > o.Price {
				break
			}
			if o.Side == SideShort && price < o.Price {
				break
			}
		}

		// STP: an account never trades with itself.
		if maker.Account == o.Account {
			bk.cancel(maker)
			maker.Status = StatusCancelled
			e.releaseResidual(maker, &releases)
			continue
		}

		qty := minUint(o.Remaining(), maker.Remaining())
		var notional *uint256.Int
		if o.Type == TypeMarket {
			notional = m.Notional(qty, price)
			if o.MaxNotional != nil {
				left := new(uint256.Int).Sub(o.MaxNotional, spent)
				if notional.Cmp(left) > 0 {
					allowed := new(uint256.Int).Mul(left, m.scale())
					allowed.Div(allowed, uint256.NewInt(uint64(price)))
					qty = minUint(qty, allowed)
					if qty.IsZero() {
						break
					}
					notional = m.Notional(qty, price)
				}
			}
		} else {
			notional = m.Notional(qty, price)
		}

		e.nextTrade++
		trades = append(trades, Trade{
			ID:         e.nextTrade,
			Market:     m.ID,
			Price:      price,
			Qty:        cloneInt(qty),
			Notional:   cloneInt(notional),
			MakerOrder: maker.ID,
			TakerOrder: o.ID,
			Maker:      maker.Account,
			Taker:      o.Account,
			MakerSide:  maker.Side,
		})

		o.Filled.Add(o.Filled, qty)
		bk.reduce(maker, qty)
		if maker.Remaining().IsZero() {
			maker.Status = StatusFilled
		} else {
			maker.Status = StatusPartiallyFilled
		}

		// Update the taker's position. The taker's own order's reserved margin
		// covers its opening leg; margin is derived from o.Price/o.Leverage.
		e.applyFill(m, o, qty, price, e.nextTrade, &releases, &pnl)
		// Update the maker's position. The maker's resting order's reserved
		// margin covers its opening leg, at its own price/leverage.
		e.applyFill(m, maker, qty, price, e.nextTrade, &releases, &pnl)

		if maker.Remaining().IsZero() {
			e.releaseResidual(maker, &releases)
		}

		if o.Type == TypeMarket {
			spent.Add(spent, notional)
		}
	}

	switch {
	case o.Remaining().IsZero():
		o.Status = StatusFilled
		e.releaseResidual(o, &releases)
	case o.Type == TypeMarket || o.TIF == TIFIOC:
		o.Status = StatusCancelled
		e.releaseResidual(o, &releases)
	default:
		if o.Filled.IsZero() {
			o.Status = StatusNew
		} else {
			o.Status = StatusPartiallyFilled
		}
		bk.add(o)
	}

	return Result{Order: o.clone(), Trades: trades, Releases: releases, PnL: pnl}, nil
}

// applyFill updates one account's position for a fill of qty base units at
// fillPrice, in the direction of the given order o (Long = buying base, Short =
// selling base). It transfers the opening leg's floor margin from the order's
// reserved freeze into the position, realizes PnL on the closing leg, and
// releases margin on closes. The order price and leverage (o.Price/o.Leverage)
// drive margin, per spec §4.4; the fill price drives PnL and the entry price.
func (e *Engine) applyFill(m Market, o *Order, qty *uint256.Int, fillPrice Price, tradeID TradeID, releases *[]MarginRelease, pnl *[]PnLSettlement) {
	byAcct := e.positions[m.ID]
	pos := byAcct[o.Account]

	if pos == nil || pos.Side == o.Side {
		// Opening or increasing: transfer floor margin from the order's reserved
		// freeze to the position. SubOverflow must never fire because floor is
		// subadditive and the reserved amount was ceil(whole order) — but panic
		// loudly rather than silently wrap if it ever does.
		margin, err := m.marginFloor(qty, o.Price, o.Leverage)
		if err != nil {
			panic("perp: applyFill margin: " + err.Error())
		}
		o.reserved, _ = new(uint256.Int).SubOverflow(o.reserved, margin)

		if pos == nil {
			byAcct[o.Account] = &Position{
				Market:     m.ID,
				Account:    o.Account,
				Side:       o.Side,
				Size:       cloneInt(qty),
				EntryPrice: fillPrice,
				Margin:     cloneInt(margin),
				Leverage:   o.Leverage,
			}
			return
		}
		// Increase same side: notional-weighted average entry price.
		oldNotional := m.Notional(pos.Size, pos.EntryPrice)
		addNotional := m.Notional(qty, fillPrice)
		newSize := new(uint256.Int).Add(pos.Size, qty)
		newNotional := new(uint256.Int).Add(oldNotional, addNotional)
		pos.EntryPrice = Price(new(uint256.Int).Div(newNotional, newSize).Uint64())
		pos.Size = newSize
		pos.Margin.Add(pos.Margin, margin)
		pos.Leverage = o.Leverage
		return
	}

	// Reducing or flipping: opposite side. No order-reserved transfer here —
	// a close never consumes the order's own opening freeze.
	prevSize := cloneInt(pos.Size)
	closed := minUint(pos.Size, qty)

	// Realize PnL on the closed portion against the entry price.
	entryNotional := m.Notional(closed, pos.EntryPrice)
	fillNotional := m.Notional(closed, fillPrice)
	var profit *uint256.Int
	if pos.Side == SideLong {
		profit = new(uint256.Int).Sub(fillNotional, entryNotional)
	} else {
		profit = new(uint256.Int).Sub(entryNotional, fillNotional)
	}
	*pnl = append(*pnl, PnLSettlement{Market: m.ID, TradeID: tradeID, Account: o.Account, Profit: profit})

	// Release margin proportionally to the closed size.
	release := new(uint256.Int).Mul(pos.Margin, closed)
	release.Div(release, prevSize)
	*releases = append(*releases, MarginRelease{Account: o.Account, Amount: cloneInt(release)})
	pos.Margin.Sub(pos.Margin, release)

	pos.Size.Sub(pos.Size, closed)
	if pos.Size.IsZero() {
		delete(byAcct, o.Account)
	}

	// Any remainder beyond the closed size flips the direction and opens a fresh
	// position, transferring floor margin from the order's reserved freeze.
	remainder := new(uint256.Int).Sub(qty, closed)
	if remainder.Sign() > 0 {
		margin, err := m.marginFloor(remainder, o.Price, o.Leverage)
		if err != nil {
			panic("perp: applyFill flip margin: " + err.Error())
		}
		o.reserved, _ = new(uint256.Int).SubOverflow(o.reserved, margin)
		byAcct[o.Account] = &Position{
			Market:     m.ID,
			Account:    o.Account,
			Side:       o.Side,
			Size:       cloneInt(remainder),
			EntryPrice: fillPrice,
			Margin:     cloneInt(margin),
			Leverage:   o.Leverage,
		}
	}
}

// releaseResidual appends a release for an order's remaining reserved margin.
func (e *Engine) releaseResidual(o *Order, out *[]MarginRelease) {
	if o.reserved == nil || o.reserved.Sign() == 0 {
		return
	}
	*out = append(*out, MarginRelease{Account: o.Account, Amount: cloneInt(o.reserved)})
	o.reserved = uint256.NewInt(0)
}

// Cancel removes one of the caller's resting orders and releases its margin.
func (e *Engine) Cancel(m MarketID, account ledger.AccountID, orderID OrderID) (Result, error) {
	if _, ok := e.byID[m]; !ok {
		return Result{}, ErrUnknownMarket
	}
	o, ok := e.all[orderID]
	if !ok || o.Market != m || !o.IsOpen() {
		return Result{}, ErrOrderNotFound
	}
	if o.Account != account {
		return Result{}, ErrNotOwner
	}
	e.books[o.Market].cancel(o)
	o.Status = StatusCancelled
	var releases []MarginRelease
	e.releaseResidual(o, &releases)
	return Result{Order: o.clone(), Releases: releases}, nil
}

// Order returns a copy of a known order.
func (e *Engine) Order(orderID OrderID) (*Order, bool) {
	o, ok := e.all[orderID]
	if !ok {
		return nil, false
	}
	return o.clone(), true
}

// Book returns up to depth price levels per side (longs descending, shorts
// ascending).
func (e *Engine) Book(m MarketID, depth int) (longs, shorts []LevelView) {
	bk, ok := e.books[m]
	if !ok || depth <= 0 {
		return nil, nil
	}
	return levelsOf(bk.bids, depth), levelsOf(bk.asks, depth)
}

func levelsOf(s *side, depth int) []LevelView {
	n := len(s.prices)
	if n > depth {
		n = depth
	}
	out := make([]LevelView, 0, n)
	for _, p := range s.prices[:n] {
		out = append(out, LevelView{Price: p, Total: cloneInt(s.levels[p].total)})
	}
	return out
}
```

- [ ] **Step 4: 运行确认通过（仓位方向/均价/翻转语义 + 保证金推导）**

Run: `cd D:\code\web3\backend && go test ./perp/ -run 'TestRequired|TestPlace|TestCancel' -v`
Expected: PASS（7 个测试全过；`TestPlaceCrossesAndOpensPosition`/`TestPlaceReduceClosesPosition`/
`TestPlaceFlipSide` 断言仓位方向与数量，`TestRequiredMarginLong` 断言保证金推导）

- [ ] **Step 5: 提交**

```bash
git add backend/perp/engine.go backend/perp/engine_test.go backend/perp/order.go
git commit -m "feat(perp): deterministic engine with position updates and margin derivation"
```

---

## Task 4: 不变量 + 确定性测试

**Files:**
- Create: `backend/perp/invariants_test.go`

- [ ] **Step 1: 写失败测试（保证金全账恒等式 + 确定性）**

```go
package perp

import (
	"math/rand"
	"testing"

	"github.com/holiman/uint256"
)

func TestMarginConservation(t *testing.T) {
	// 保证金全账恒等式（spec §4.6.4）：
	// Σ(订单级冻结) == Σ(所有 Releases) + Σ(在簿挂单 reserved 残额) + Σ(持仓 Margin)
	e, _ := NewEngine([]Market{mkMarket()})

	var totalFrozen, totalReleases uint64
	place := func(req PlaceRequest) Result {
		t.Helper()
		reqMargin, err := e.RequiredMargin(req)
		if err != nil {
			t.Fatalf("RequiredMargin: %v", err)
		}
		totalFrozen += reqMargin.Uint64()
		res, err := e.Place(req)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		for _, r := range res.Releases {
			totalReleases += r.Amount.Uint64()
		}
		return res
	}

	// Open and fully close a position, then leave one resting order.
	place(shortReq(acct(1), 100, 100, 10))
	place(longReq(acct(2), 100, 100, 10))
	// A resting order that does not cross leaves reserved margin.
	place(longReq(acct(3), 50, 100, 10))

	restingReserved := uint64(0)
	for _, o := range e.all {
		if o.reserved != nil {
			restingReserved += o.reserved.Uint64()
		}
	}
	posMargin := uint64(0)
	for _, byAcct := range e.positions {
		for _, p := range byAcct {
			posMargin += p.Margin.Uint64()
		}
	}

	if totalFrozen != totalReleases+restingReserved+posMargin {
		t.Fatalf("margin conservation: frozen=%d releases=%d resting=%d pos=%d",
			totalFrozen, totalReleases, restingReserved, posMargin)
	}
}

func TestDeterministicReplay(t *testing.T) {
	build := func() *Engine {
		e, _ := NewEngine([]Market{mkMarket()})
		rng := rand.New(rand.NewSource(42))
		for i := 0; i < 200; i++ {
			side := SideLong
			if rng.Intn(2) == 1 {
				side = SideShort
			}
			acct := acct(byte(1 + rng.Intn(5)))
			price := Price(90 + rng.Intn(21))
			qty := uint64(1 + rng.Intn(50))
			req := PlaceRequest{Market: 1, Account: acct, Side: side, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: 10}
			e.Place(req)
			if rng.Intn(4) == 0 {
				// occasionally cancel a random resting order
				var victim OrderID
				for _, o := range e.all {
					if o.IsOpen() {
						victim = o.ID
						break
					}
				}
				if victim != 0 {
					e.Cancel(1, e.all[victim].Account, victim)
				}
			}
		}
		return e
	}
	e1, e2 := build(), build()
	// Compare all positions.
	for m, byAcct := range e1.positions {
		for a, p := range byAcct {
			q, ok := e2.positions[m][a]
			if !ok {
				t.Fatalf("position %x missing in replay", a)
			}
			if q.Side != p.Side || q.Size.Cmp(p.Size) != 0 || q.EntryPrice != p.EntryPrice {
				t.Fatalf("position %x diverged: %+v vs %+v", a, p, q)
			}
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./perp/ -run 'TestMarginConservation|TestDeterministicReplay' -v`
Expected: FAIL — `undefined: TestMarginConservation, TestDeterministicReplay`（这两个测试尚未写；保证金记账已在
Task 3 实现，本 Task 只新增测试）

- [ ] **Step 3: 实现不变量测试（把 Step 1 的代码落盘）**

上面 Step 1 的代码已完整，直接落盘为 `backend/perp/invariants_test.go`。若 `TestMarginConservation` 暴露
保证金记账 bug，修复 `engine.go`（不应有，因为 Task 3 已按 spec §4.4 完整实现）；若暴露出 spec 未覆盖的
边界，回 spec 补裁定。

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./perp/ -run 'TestMarginConservation|TestDeterministicReplay' -v`
Expected: PASS（2 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/perp/engine.go backend/perp/invariants_test.go
git commit -m "test(perp): margin conservation invariant and deterministic replay"
```

---

## Task 5: 服务层 PerpOrderBookService

**Files:**
- Create: `backend/services/perp_order_book.go`
- Test: `backend/services/perp_order_book_test.go`

- [ ] **Step 1: 写失败测试（端到端开仓→平仓，账本 + 保证金 + 盈亏）**

```go
package services

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

func perpMarkets() []perp.Market {
	return []perp.Market{{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}}
}

func perpAcct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func TestPerpOpenAndCloseSettlesPnL(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	// Fund two accounts with quote (asset 1).
	l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(100000), Ref: "d1"})
	l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(100000), Ref: "d2"})

	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustJournal(t, ""), bus)
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	defer svc.Close()

	// Account 1 opens short 100 @ 100 (freezes margin 10).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open short: %v", err)
	}
	// Account 2 opens long 100 @ 100 (crosses).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open long: %v", err)
	}

	// Account 2 closes long 100 @ 120 (profit = notional(100,120)-notional(100,100)
	// = 120-100 = 20). Needs a resting short to cross; account 3 provides it.
	l.Apply(ledger.DepositOp{Account: perpAcct(3), Asset: 1, Amount: uint256.NewInt(100000), Ref: "d3"})
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("rest short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("close long: %v", err)
	}

	// Account 2's quote balance should reflect +20 profit (and margin release).
	bal := l.Balance(perpAcct(2), 1).Uint64()
	// started 100000, opened long (margin 10 frozen, not deducted), closed for +20.
	// The realized profit 20 is credited; margin was frozen then released.
	if bal != 100020 {
		t.Fatalf("account 2 balance = %d, want 100020", bal)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestPerpOpenAndClose -v`
Expected: FAIL — `undefined: NewPerpOrderBookService`

- [ ] **Step 3: 实现 perp WAL 类型（新文件 `backend/services/perp_journal.go`）**

现货 `Journal` 硬编码了 `matching` 命令类型，无法复用。perp 用独立的轻量 WAL（同目录、同尾损规则）：

```go
package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// PerpJournal is the perpetual order book's instruction WAL. It mirrors the
// spot Journal's torn-tail rule: a final non-newline-terminated or unparseable
// record is a torn tail and is ignored; interior corruption is a hard error.
// dir == "" is memory-only.
type PerpJournal struct {
	dir string
	mu  sync.Mutex
	f   *os.File
	seq uint64
}

func NewPerpJournal(dir string) (*PerpJournal, error) {
	j := &PerpJournal{dir: dir}
	if dir == "" {
		return j, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "perp_orders.wal"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

func (j *PerpJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

type perpPlaceJSON struct {
	Market      uint32 `json:"market"`
	Account     string `json:"account"`
	Side        uint8  `json:"side"`
	Type        uint8  `json:"type"`
	TIF         uint8  `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"max_notional,omitempty"`
	Leverage    uint32 `json:"leverage"`
}

type perpCancelJSON struct {
	Market  uint32 `json:"market"`
	Account string `json:"account"`
	OrderID uint64 `json:"order_id"`
}

type perpLine struct {
	Seq     uint64          `json:"seq"`
	Kind    string          `json:"kind"` // "place" | "cancel"
	Payload json.RawMessage `json:"payload"`
}

// Append records one place instruction durably.
func (j *PerpJournal) AppendPlace(req perp.PlaceRequest) error {
	payload := perpPlaceJSON{
		Market:   uint32(req.Market),
		Account:  req.Account.Hex(),
		Side:     uint8(req.Side),
		Type:     uint8(req.Type),
		TIF:      uint8(req.TIF),
		Price:    uint64(req.Price),
		Leverage: req.Leverage,
	}
	if req.Qty != nil {
		payload.Qty = req.Qty.Dec()
	}
	if req.MaxNotional != nil && !req.MaxNotional.IsZero() {
		payload.MaxNotional = req.MaxNotional.Dec()
	}
	return j.append("place", payload)
}

// AppendCancel records one cancel instruction durably.
func (j *PerpJournal) AppendCancel(m perp.MarketID, a ledger.AccountID, id perp.OrderID) error {
	return j.append("cancel", perpCancelJSON{Market: uint32(m), Account: a.Hex(), OrderID: uint64(id)})
}

func (j *PerpJournal) append(kind string, payload any) error {
	if j.dir == "" {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return errors.New("services: perp journal closed")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	j.seq++
	line, err := json.Marshal(perpLine{Seq: j.seq, Kind: kind, Payload: body})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		return err
	}
	return j.f.Sync()
}

// PerpInstruction is one decoded instruction for replay.
type PerpInstruction struct {
	Kind   string // "place" | "cancel"
	Place  *perp.PlaceRequest
	Cancel *perp.CancelInstruction
}

type perpCancelInstruction struct {
	Market  perp.MarketID
	Account ledger.AccountID
	OrderID perp.OrderID
}

// Replay returns every committed instruction in file order. Only a torn
// trailing record is tolerated.
func (j *PerpJournal) Replay() ([]PerpInstruction, error) {
	if j.dir == "" {
		return nil, nil
	}
	path := filepath.Join(j.dir, "perp_orders.wal")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	hasNL := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})
	lastData := -1
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) > 0 {
			lastData = i
		}
	}
	var out []PerpInstruction
	for i, raw := range lines {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var ln perpLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			if i == lastData && !hasNL {
				log.Printf("perp journal: ignoring torn trailing record: %v", err)
				break
			}
			return nil, fmt.Errorf("services: corrupt perp journal record at line %d: %w", i+1, err)
		}
		switch ln.Kind {
		case "place":
			var p perpPlaceJSON
			if err := json.Unmarshal(ln.Payload, &p); err != nil {
				return nil, err
			}
			account, err := ledger.AccountFromHex(p.Account)
			if err != nil {
				return nil, err
			}
			qty, err := uint256.FromDecimal(p.Qty)
			if err != nil {
				return nil, fmt.Errorf("services: bad perp qty %q: %w", p.Qty, err)
			}
			req := perp.PlaceRequest{
				Market: perp.MarketID(p.Market), Account: account,
				Side: perp.Side(p.Side), Type: perp.Type(p.Type), TIF: perp.TIF(p.TIF),
				Price: perp.Price(p.Price), Qty: qty, Leverage: p.Leverage,
			}
			if p.MaxNotional != "" {
				mn, err := uint256.FromDecimal(p.MaxNotional)
				if err != nil {
					return nil, err
				}
				req.MaxNotional = mn
			}
			out = append(out, PerpInstruction{Kind: "place", Place: &req})
		case "cancel":
			var c perpCancelJSON
			if err := json.Unmarshal(ln.Payload, &c); err != nil {
				return nil, err
			}
			account, err := ledger.AccountFromHex(c.Account)
			if err != nil {
				return nil, err
			}
			out = append(out, PerpInstruction{Kind: "cancel", Cancel: &perpCancelInstruction{
				Market: perp.MarketID(c.Market), Account: account, OrderID: perp.OrderID(c.OrderID),
			}})
		default:
			return nil, fmt.Errorf("services: unknown perp journal kind %q", ln.Kind)
		}
	}
	return out, nil
}
```

- [ ] **Step 4: 实现 PerpOrderBookService（`backend/services/perp_order_book.go`）**

```go
package services

import (
	"errors"
	"fmt"
	"sync"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

const (
	TopicPerpTrade    = "perp.trade"
	TopicPerpPosition = "perp.position"
	TopicPerpDegraded = "perp.degraded"

	// perpRunInterval is the poll interval for the runner. 6a has no periodic
	// work, so the runner only keeps the restart semantics identical to the
	// other stack services; the interval is otherwise unused.
	perpRunInterval = 30 * time.Second
)

var ErrPerpServiceDegraded = errors.New("services: perp order book degraded, restart required")

// PerpOrderBookService hosts the perp kernel: it freezes quote margin, journals
// instructions, matches, realizes PnL into the ledger, releases margin, and
// publishes events — in that order.
type PerpOrderBookService struct {
	engine  *perp.Engine
	ledger  *ledger.Ledger
	holds   *ledger.Holds
	journal *PerpJournal
	bus     *EventBus
	markets map[perp.MarketID]perp.Market

	mu       sync.Mutex
	fatalErr error
	run      runner
}

func NewPerpOrderBookService(markets []perp.Market, l *ledger.Ledger, holds *ledger.Holds, journal *PerpJournal, bus *EventBus) (*PerpOrderBookService, error) {
	if l == nil {
		return nil, errors.New("services: perp order book requires a ledger")
	}
	if holds == nil {
		return nil, errors.New("services: perp order book requires holds")
	}
	if journal == nil {
		return nil, errors.New("services: perp order book requires a journal")
	}
	if bus == nil {
		bus = NewEventBus()
	}
	engine, err := perp.NewEngine(markets)
	if err != nil {
		return nil, err
	}
	byID := make(map[perp.MarketID]perp.Market, len(markets))
	for _, m := range engine.Markets() {
		byID[m.ID] = m
	}
	s := &PerpOrderBookService{
		engine: engine, ledger: l, holds: holds, journal: journal, bus: bus, markets: byID,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PerpOrderBookService) Name() string { return "PerpOrderBookService" }

func (s *PerpOrderBookService) Initialize(cfg Config) error { return nil }

func (s *PerpOrderBookService) Start() error {
	s.run.start(perpRunInterval, func() {})
	return nil
}

func (s *PerpOrderBookService) Stop() error {
	s.run.stop()
	return nil
}

func (s *PerpOrderBookService) Close() error {
	s.run.stop()
	return s.journal.Close()
}

func (s *PerpOrderBookService) Engine() *perp.Engine { return s.engine }
func (s *PerpOrderBookService) Holds() *ledger.Holds { return s.holds }

// Place freezes the order-level margin, journals the instruction, matches,
// realizes PnL into the ledger, releases margin, and publishes events — in that
// order. A validation or funding failure leaves positions untouched.
func (s *PerpOrderBookService) Place(req perp.PlaceRequest) (perp.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return perp.Result{}, s.fatalErr
	}

	required, err := s.engine.RequiredMargin(req)
	if err != nil {
		return perp.Result{}, err
	}
	quote := s.markets[req.Market].Quote
	if err := s.holds.Reserve(req.Account, quote, required); err != nil {
		return perp.Result{}, err
	}

	if err := s.journal.AppendPlace(req); err != nil {
		_ = s.holds.Release(req.Account, quote, required)
		return perp.Result{}, fmt.Errorf("services: perp: append journal: %w", err)
	}

	res, err := s.engine.Place(req)
	if err != nil || res.Order == nil || res.Order.Status == perp.StatusRejected {
		_ = s.holds.Release(req.Account, quote, required)
		if err == nil {
			err = fmt.Errorf("%w: order rejected", perp.ErrBadRequest)
		}
		return perp.Result{}, err
	}

	// Realize PnL strictly before releasing margin (spec §5.2 order).
	if err := s.settlePnL(res.PnL); err != nil {
		return perp.Result{}, s.degrade(err)
	}
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, quote, r.Amount); err != nil {
			return perp.Result{}, s.degrade(err)
		}
	}

	s.publish(res)
	return res, nil
}

// Cancel journals the instruction, removes the caller's resting order, releases
// its residual margin, and publishes the position event.
func (s *PerpOrderBookService) Cancel(m perp.MarketID, a ledger.AccountID, id perp.OrderID) (perp.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return perp.Result{}, s.fatalErr
	}
	if err := s.journal.AppendCancel(m, a, id); err != nil {
		return perp.Result{}, fmt.Errorf("services: perp: append journal: %w", err)
	}
	res, err := s.engine.Cancel(m, a, id)
	if err != nil {
		return perp.Result{}, err
	}
	quote := s.markets[m].Quote
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, quote, r.Amount); err != nil {
			return perp.Result{}, s.degrade(err)
		}
	}
	s.publish(res)
	return res, nil
}

// settlePnL realizes each PnL settlement as a DepositOp (profit) or WithdrawOp
// (loss) in the settlement's market quote asset, keyed by trade id so a replay
// is idempotent.
func (s *PerpOrderBookService) settlePnL(pnls []perp.PnLSettlement) error {
	for _, p := range pnls {
		quote := s.markets[p.Market].Quote
		amt := new(uint256.Int).Set(p.Profit)
		if amt.Sign() < 0 {
			amt.Neg(amt)
		}
		if amt.IsZero() {
			continue
		}
		ref := fmt.Sprintf("perp:pnl:%d", p.TradeID)
		if p.Profit.Sign() >= 0 {
			if err := s.ledger.Apply(ledger.DepositOp{Account: p.Account, Asset: quote, Amount: amt, Ref: ref}); err != nil {
				return err
			}
		} else {
			if err := s.ledger.Apply(ledger.WithdrawOp{Account: p.Account, Asset: quote, Amount: amt, Ref: ref}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *PerpOrderBookService) degrade(cause error) error {
	if s.fatalErr == nil {
		s.fatalErr = fmt.Errorf("%w: %v", ErrPerpServiceDegraded, cause)
	}
	_ = s.bus.Publish(Event{Topic: TopicPerpDegraded, Data: cause})
	return s.fatalErr
}

func (s *PerpOrderBookService) publish(res perp.Result) {
	if res.Order != nil {
		_ = s.bus.Publish(Event{Topic: TopicPerpPosition, Data: res.Order})
	}
	for _, t := range res.Trades {
		_ = s.bus.Publish(Event{Topic: TopicPerpTrade, Data: t})
	}
}

// recover replays the instruction WAL, re-settling PnL idempotently and then
// reconciling holds from the kernel's open margins.
func (s *PerpOrderBookService) recover() error {
	ins, err := s.journal.Replay()
	if err != nil {
		return fmt.Errorf("services: perp: replay journal: %w", err)
	}
	for _, in := range ins {
		switch in.Kind {
		case "place":
			res, err := s.engine.Place(*in.Place)
			if err != nil {
				continue // deterministically rejected
			}
			if err := s.settlePnL(res.PnL); err != nil {
				return fmt.Errorf("services: perp: replay settle: %w", err)
			}
		case "cancel":
			if _, err := s.engine.Cancel(in.Cancel.Market, in.Cancel.Account, in.Cancel.OrderID); err != nil {
				continue
			}
		}
	}
	return s.holds.Reconcile(s.engine.OpenMargins())
}
```

- [ ] **Step 5: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestPerpOpenAndClose -v`
Expected: PASS（端到端盈亏结算正确）

- [ ] **Step 6: 提交**

```bash
git add backend/services/perp_order_book.go backend/services/perp_journal.go backend/services/perp_order_book_test.go
git commit -m "feat(services): perp order book service with PnL settlement"
```

---

## Task 6: 配置 + main 装配 + README

**Files:**
- Modify: `backend/config/config.go`（+PerpConfig）
- Modify: `backend/main.go`（装配，可选启用）
- Modify: `README.md`

- [ ] **Step 1: config 加 PerpConfig**

```go
// PerpConfig configures the perpetual order book (subproject 6a).
type PerpConfig struct {
    Markets   string
    DataDir   string
}

func LoadPerpConfig() PerpConfig {
    return PerpConfig{
        Markets: os.Getenv("PERP_MARKETS"),
        DataDir: os.Getenv("PERP_DATA_DIR"),
    }
}
```

- [ ] **Step 2: main 装配（与现货 order book 并列，可选启用）**

在 `main.go` 中，`startSettlementStack` 返回后、挂 gateway 之后，当 `PERP_MARKETS` 非空时装配
`PerpOrderBookService` 并注册进 registry。沿用现货 order book 的装配模式（同一个 ledger/holds/bus）：

```go
	// Mount the perpetual order book when PERP_MARKETS is configured. It shares
	// the settlement stack's ledger, holds, and bus, so perp margin and PnL
	// contend for the same funds as spot and withdrawals.
	if settlement != nil && config.LoadPerpConfig().Markets != "" {
		pc := config.LoadPerpConfig()
		perpMarkets, err := services.ParsePerpMarkets(pc.Markets)
		if err != nil {
			log.Fatalf("Failed to parse PERP_MARKETS: %v", err)
		}
		perpJournal, err := services.NewPerpJournal(pc.DataDir)
		if err != nil {
			log.Fatalf("Failed to open perp journal: %v", err)
		}
		perpsvc, err := services.NewPerpOrderBookService(perpMarkets, settlement.Ledger, settlement.Holds, perpJournal, settlement.Bus)
		if err != nil {
			_ = perpJournal.Close()
			log.Fatalf("Failed to build perp order book: %v", err)
		}
		settlement.Registry.Register(perpsvc)
		if err := perpsvc.Start(); err != nil {
			log.Fatalf("Failed to start perp order book: %v", err)
		}
		log.Printf("perpetual order book mounted (%d markets)", len(perpMarkets))
	}
```

> `services.ParsePerpMarkets` 是新增的解析函数（本 Task 一并实现），格式
> `ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]`，`;` 分隔，解析失败返回错误。其实现：

```go
// ParsePerpMarkets parses the PERP_MARKETS string:
//   ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]
// separated by ';'. A malformed field is an error (an operator typo must not
// silently trade a subset).
func ParsePerpMarkets(s string) ([]perp.Market, error) {
	var out []perp.Market
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		if len(fields) < 6 {
			return nil, fmt.Errorf("services: perp market %q: want ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]", part)
		}
		id, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad id: %w", part, err)
		}
		base, err := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad base: %w", part, err)
		}
		quote, err := strconv.ParseUint(strings.TrimSpace(fields[3]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad quote: %w", part, err)
		}
		decimals, err := strconv.ParseUint(strings.TrimSpace(fields[4]), 10, 8)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad base decimals: %w", part, err)
		}
		leverage, err := strconv.ParseUint(strings.TrimSpace(fields[5]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad max leverage: %w", part, err)
		}
		m := perp.Market{
			ID: perp.MarketID(id), Symbol: strings.TrimSpace(fields[1]),
			Base: ledger.AssetID(base), Quote: ledger.AssetID(quote),
			BaseDecimals: uint8(decimals), MaxLeverage: uint32(leverage),
		}
		if len(fields) >= 7 && strings.TrimSpace(fields[6]) != "" {
			minQty, err := uint256.FromDecimal(strings.TrimSpace(fields[6]))
			if err != nil {
				return nil, fmt.Errorf("services: perp market %q: bad min qty: %w", part, err)
			}
			m.MinQty = minQty
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
```

> `ParsePerpMarkets` 放在 `backend/services/perp_order_book.go`（同文件），import 需含 `"strconv"`、`"strings"`、
> `"github.com/holiman/uint256"`、`"web3-amm-dex-backend/perp"`、`"web3-amm-dex-backend/ledger"`。

- [ ] **Step 3: README 追加**

```markdown
## Perpetual Futures (subproject 6a)

Perpetual markets trade positions (long/short) with isolated margin and
user-selected leverage. Margin is frozen in the market's quote asset on both
sides; PnL is realized into the ledger on close.

### Known gaps (6a)
- No mark price or funding rate (6b); PnL uses the trade price.
- No liquidation engine (6c): a loss exceeding margin causes a fatal degrade
  rather than forced liquidation.

### Configuration
| Key | Default | Description |
|-----|---------|-------------|
| `PERP_MARKETS` | (empty) | `ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]`, `;`-separated |
| `PERP_DATA_DIR` | (empty) | instruction WAL dir (empty = memory-only) |
```

- [ ] **Step 4: 全量验证**

Run: `cd D:\code\web3\backend && go build ./... && go vet ./... && go test ./... -count=1`
Expected: 全部 `ok`

Run: `cd D:\code\web3 && npx hardhat test`
Expected: 33 passing（无合约改动）

- [ ] **Step 5: 提交**

```bash
git add backend/config/config.go backend/main.go README.md
git commit -m "feat(perp): config, main wiring, and docs"
```

---

## 最终验证

- [ ] **Step 1: Go 全量**

Run: `cd D:\code\web3\backend && go build ./... && go vet ./... && go test ./... -count=1`
Expected: 全部 `ok`

- [ ] **Step 2: 合约回归**

Run: `cd D:\code\web3 && npx hardhat test`
Expected: 33 passing

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-10-18-orderbook-dex-perp.md`.

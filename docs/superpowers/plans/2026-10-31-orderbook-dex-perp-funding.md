# 子项目 6b：标记价格 + 资金费率 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 6a 的永续内核加上标记价格与资金费率：可替换的 index price 源、抗操纵的 mark price 推导、周期性资金费结算（含欠账与幂等重放），并把未实现盈亏改用标记价。

**Architecture:** `perp` 包新增纯函数（标记价推导、资金费计算、未实现盈亏），不读时钟、不碰 I/O；服务层负责拉价、陈旧判定、周期性触发与账本结算；`mark`/`funding` 作为新指令类型落 WAL，重放用落盘值保证确定性。

**Tech Stack:** Go 1.x、`github.com/holiman/uint256`、`math/big`（仅资金费率的有符号中间计算）、bbolt（已有，未改动）。

**Spec:** `docs/superpowers/specs/2026-10-31-orderbook-dex-perp-funding-design.md`

## Global Constraints

- `backend/perp` 必须保持纯确定性：无 I/O、不读时钟、不读环境变量、不使用随机数。
- 所有金额运算走 `uint256` 并对 `MulOverflow`/`AddOverflow`/`SubOverflow` 显式检查；溢出即 `ErrAmountTooLarge`，绝不静默取模。
- 未设置 `PriceSource` 时，服务行为必须与 6a **逐字节一致**（不产生 mark/funding 指令，不改任何已有测试的期望）。
- 定点标度统一 `1e18`（常量 `scaleE18`）。
- 全量 `go build ./...` 与 `go test ./... -count=1` 必须绿色；`npx hardhat test` 必须绿色（本子项目不改合约，但回归要跑）。

---

### Task 1: 标记价推导（纯函数）

**Files:**
- Create: `backend/perp/price.go`
- Test: `backend/perp/price_test.go`

**Interfaces:**
- Consumes: `Market`（含新增 `MarkConfig`）、`Price`、`LevelView`。
- Produces:
  - `type MarkConfig struct { MaxDeviationE18 *uint256.Int; IndexWeight, BookWeight uint32 }`
  - `func (m Market) MarkPrice(index Price, bestBid, bestAsk Price, hasBid, hasAsk bool) (Price, error)`
  - `const scaleE18 uint64 = 1_000_000_000_000_000_000`

- [ ] **Step 1: 写失败测试**

```go
package perp

import (
	"testing"

	"github.com/holiman/uint256"
)

func testMarkMarket() Market {
	return Market{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 18, MaxLeverage: 10,
		MarkConfig: MarkConfig{
			MaxDeviationE18: uint256.NewInt(5_000_000_000_000_000), // 0.5%
			IndexWeight:     1,
			BookWeight:      1,
		},
	}
}

func TestMarkPriceEmptyBookEqualsIndex(t *testing.T) {
	m := testMarkMarket()
	got, err := m.MarkPrice(2000, 0, 0, false, false)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2000 {
		t.Fatalf("want index 2000, got %d", got)
	}
}

func TestMarkPriceTwoSidedIsAverageOfIndexAndMid(t *testing.T) {
	m := testMarkMarket()
	// index 2000, mid 2002 -> (2000+2002)/2 = 2001
	got, err := m.MarkPrice(2000, 2001, 2003, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2001 {
		t.Fatalf("want 2001, got %d", got)
	}
}

func TestMarkPriceClampsBookDeviation(t *testing.T) {
	m := testMarkMarket()
	// mid 3000 is 50% above index 2000 -> clamped to 2010 (index*1.005)
	got, err := m.MarkPrice(2000, 2999, 3001, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	// clamped mid = 2010, average with index = 2005
	if got != 2005 {
		t.Fatalf("want 2005 (average of 2000 and clamped 2010), got %d", got)
	}
}

func TestMarkPriceOneSidedUsesThatSide(t *testing.T) {
	m := testMarkMarket()
	got, err := m.MarkPrice(2000, 2004, 0, true, false)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	// clamped mid 2004, average = 2002
	if got != 2002 {
		t.Fatalf("want 2002, got %d", got)
	}
}

func TestMarkPriceRejectsZeroIndex(t *testing.T) {
	m := testMarkMarket()
	if _, err := m.MarkPrice(0, 2000, 2001, true, true); err == nil {
		t.Fatal("want error for zero index")
	}
}

func TestMarkPriceClampsDownward(t *testing.T) {
	m := testMarkMarket()
	// mid 1000 is 50% below index 2000 -> clamped to 1990, average 1995
	got, err := m.MarkPrice(2000, 999, 1001, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 1995 {
		t.Fatalf("want 1995, got %d", got)
	}
}

func TestMarkPriceDefaultConfigApplies(t *testing.T) {
	m := testMarkMarket()
	m.MarkConfig = MarkConfig{} // all zero -> defaults
	got, err := m.MarkPrice(2000, 2001, 2003, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2001 {
		t.Fatalf("want 2001 with default config, got %d", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd backend; go test ./perp -run MarkPrice -v`
Expected: FAIL with "undefined: MarkConfig" / "m.MarkPrice undefined"

- [ ] **Step 3: 实现 price.go**

```go
package perp

import (
	"fmt"

	"github.com/holiman/uint256"
)

// scaleE18 is the fixed-point scale for rates and deviation bounds.
const scaleE18 uint64 = 1_000_000_000_000_000_000

// Defaults for mark-price derivation (spec §3.2).
var (
	defaultMaxDeviationE18 = uint256.NewInt(5_000_000_000_000_000) // 0.5%
	defaultMarkWeight      = uint32(1)
)

// MarkConfig configures how a market derives its mark price from the index
// price and the perpetual book (spec §3.2). The zero value means defaults.
type MarkConfig struct {
	// MaxDeviationE18 bounds |bookMid/index - 1|, scaled by 1e18.
	MaxDeviationE18 *uint256.Int
	// IndexWeight and BookWeight are the weights of the index price and the
	// (clamped) book mid in the mark price. Both zero means 1:1.
	IndexWeight uint32
	BookWeight  uint32
}

func (c MarkConfig) maxDeviation() *uint256.Int {
	if c.MaxDeviationE18 == nil || c.MaxDeviationE18.IsZero() {
		return new(uint256.Int).Set(defaultMaxDeviationE18)
	}
	return c.MaxDeviationE18
}

func (c MarkConfig) weights() (uint32, uint32) {
	if c.IndexWeight == 0 && c.BookWeight == 0 {
		return defaultMarkWeight, defaultMarkWeight
	}
	if c.IndexWeight == 0 {
		return defaultMarkWeight, c.BookWeight
	}
	if c.BookWeight == 0 {
		return c.IndexWeight, defaultMarkWeight
	}
	return c.IndexWeight, c.BookWeight
}

// MarkPrice derives the mark price from the index price and the best bid/ask
// (spec §3.2):
//
//	mid     = (bestBid + bestAsk) / 2         (one-sided uses that side; no book -> index)
//	clamped = clamp(mid, index*(1-dev), index*(1+dev))
//	mark    = (index*wi + clamped*wb) / (wi + wb)
//
// It is a pure function: no clock, no I/O, no randomness. It returns an error
// only for a zero index price (a caller must treat a missing price as "no mark",
// not as a zero mark).
func (m Market) MarkPrice(index Price, bestBid, bestAsk Price, hasBid, hasAsk bool) (Price, error) {
	if index == 0 {
		return 0, fmt.Errorf("%w: mark price requires a positive index price", ErrBadRequest)
	}
	idx := uint256.NewInt(uint64(index))

	var mid *uint256.Int
	switch {
	case hasBid && hasAsk:
		// Integer average of two uint64 prices cannot overflow uint256.
		sum := new(uint256.Int).Add(uint256.NewInt(uint64(bestBid)), uint256.NewInt(uint64(bestAsk)))
		mid = sum.Div(sum, uint256.NewInt(2))
	case hasBid:
		mid = uint256.NewInt(uint64(bestBid))
	case hasAsk:
		mid = uint256.NewInt(uint64(bestAsk))
	default:
		return index, nil
	}

	dev := m.MarkConfig.maxDeviation()
	// upper = index * (1e18 + dev) / 1e18 ; lower = index * (1e18 - dev) / 1e18
	e18 := uint256.NewInt(scaleE18)
	upper, overflow := new(uint256.Int).MulOverflow(idx, new(uint256.Int).Add(e18, dev))
	if overflow {
		return 0, ErrAmountTooLarge
	}
	upper.Div(upper, e18)
	var devSub *uint256.Int
	if dev.Cmp(e18) >= 0 {
		// A deviation >= 100% makes the lower bound zero rather than negative.
		devSub = uint256.NewInt(0)
	} else {
		devSub = new(uint256.Int).Sub(e18, dev)
	}
	lower, overflow := new(uint256.Int).MulOverflow(idx, devSub)
	if overflow {
		return 0, ErrAmountTooLarge
	}
	lower.Div(lower, e18)

	clamped := mid
	if clamped.Cmp(upper) > 0 {
		clamped = upper
	} else if clamped.Cmp(lower) < 0 {
		clamped = lower
	}

	wi, wb := m.MarkConfig.weights()
	wsum := uint64(wi) + uint64(wb)
	num, overflow := new(uint256.Int).MulOverflow(idx, uint256.NewInt(uint64(wi)))
	if overflow {
		return 0, ErrAmountTooLarge
	}
	bookNum, overflow := new(uint256.Int).MulOverflow(clamped, uint256.NewInt(uint64(wb)))
	if overflow {
		return 0, ErrAmountTooLarge
	}
	num.Add(num, bookNum)
	num.Div(num, uint256.NewInt(wsum))
	if num.IsZero() || !num.IsUint64() {
		return 0, ErrAmountTooLarge
	}
	return Price(num.Uint64()), nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd backend; go test ./perp -run MarkPrice -v`
Expected: PASS (7 tests)

- [ ] **Step 5: 提交**

```bash
git add backend/perp/price.go backend/perp/price_test.go
git commit -m "feat(perp): derive an anti-manipulation mark price from index and book"
```

---

### Task 2: 资金费率计算（纯函数）

**Files:**
- Create: `backend/perp/funding.go`
- Test: `backend/perp/funding_test.go`

**Interfaces:**
- Consumes: `Price`, `scaleE18`, `Market.Notional`.
- Produces:
  - `type FundingRate struct { Scaled *big.Int }`（有符号，1e18 定点）
  - `func FundingRateFromMark(index, mark Price, intervalSeconds uint64, maxRateE18 *uint256.Int) (*big.Int, error)`
  - `func (m Market) FundingAmount(size *uint256.Int, mark Price, rateScaled *big.Int) (*uint256.Int, error)`
  - `func (r *big.Int) String()` 已有。

- [ ] **Step 1: 写失败测试**

```go
package perp

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"
)

func TestFundingRatePositivePremium(t *testing.T) {
	// index 2000, mark 2002, 1h interval -> premium = 0.001 -> rate = 0.001
	rate, err := FundingRateFromMark(2000, 2002, 3600, uint256.NewInt(75_000_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want, _ := new(big.Int).SetString("1000000000000000", 10) // 0.001
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateNegativePremium(t *testing.T) {
	// index 2000, mark 1998 -> premium -0.001
	rate, err := FundingRateFromMark(2000, 1998, 3600, uint256.NewInt(75_000_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want, _ := new(big.Int).SetString("-1000000000000000", 10)
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateClampsToMax(t *testing.T) {
	// 10% premium with 0.75% cap -> 0.75%
	rate, err := FundingRateFromMark(2000, 2200, 3600, uint256.NewInt(7_500_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want := new(big.Int).Set(uint256.NewInt(7_500_000_000_000_000).ToBig())
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateScalesWithInterval(t *testing.T) {
	// 0.1% premium over an 8h interval -> 0.0125% per hour
	rate, err := FundingRateFromMark(2000, 2002, 8*3600, uint256.NewInt(7_500_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want, _ := new(big.Int).SetString("125000000000000", 10) // 0.000125
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateZeroIndexRejected(t *testing.T) {
	if _, err := FundingRateFromMark(0, 2000, 3600, uint256.NewInt(1)); err == nil {
		t.Fatal("want error for zero index")
	}
}

func TestFundingAmountPositive(t *testing.T) {
	m := testMarkMarket()
	// size 10 base (1e19 at 18 decimals), mark 2000 -> notional 20000e18, rate 0.001
	size, _ := uint256.FromDecimal("10000000000000000000")
	rate, _ := new(big.Int).SetString("1000000000000000", 10)
	got, err := m.FundingAmount(size, 2000, rate)
	if err != nil {
		t.Fatalf("FundingAmount: %v", err)
	}
	// 20000e18 * 1e15 / 1e18 = 20e15
	want, _ := uint256.FromDecimal("20000000000000000")
	if got.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestFundingAmountNegativeRateIsAbsolute(t *testing.T) {
	m := testMarkMarket()
	size, _ := uint256.FromDecimal("10000000000000000000")
	rate, _ := new(big.Int).SetString("-1000000000000000", 10)
	got, err := m.FundingAmount(size, 2000, rate)
	if err != nil {
		t.Fatalf("FundingAmount: %v", err)
	}
	want, _ := uint256.FromDecimal("20000000000000000")
	if got.Cmp(want) != 0 {
		t.Fatalf("want absolute %s, got %s", want, got)
	}
}

func TestFundingAmountZeroSizeIsZero(t *testing.T) {
	m := testMarkMarket()
	got, err := m.FundingAmount(uint256.NewInt(0), 2000, new(big.Int))
	if err != nil {
		t.Fatalf("FundingAmount: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("want 0, got %s", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd backend; go test ./perp -run Funding -v`
Expected: FAIL with "undefined: FundingRateFromMark"

- [ ] **Step 3: 实现 funding.go**

```go
package perp

import (
	"fmt"
	"math/big"

	"github.com/holiman/uint256"
)

// FundingRateFromMark computes the periodic funding rate from the premium
// between mark and index (spec §3.3):
//
//	premium = (mark - index) / index
//	rate    = clamp(premium / intervalHours, -maxRate, +maxRate)
//
// The result is SIGNED and scaled by 1e18 (positive means longs pay shorts).
// intervalSeconds must be > 0; the division is by the number of whole hours,
// floored to at least one.
func FundingRateFromMark(index, mark Price, intervalSeconds uint64, maxRateE18 *uint256.Int) (*big.Int, error) {
	if index == 0 {
		return nil, fmt.Errorf("%w: funding rate requires a positive index price", ErrBadRequest)
	}
	if intervalSeconds == 0 {
		return nil, fmt.Errorf("%w: funding interval must be > 0", ErrBadRequest)
	}
	// premiumE18 = (mark - index) * 1e18 / index, signed.
	diff := new(big.Int).Sub(new(big.Int).SetUint64(uint64(mark)), new(big.Int).SetUint64(uint64(index)))
	premium := diff.Mul(diff, new(big.Int).SetUint64(scaleE18))
	premium.Div(premium, new(big.Int).SetUint64(uint64(index)))

	hours := intervalSeconds / 3600
	if hours == 0 {
		hours = 1
	}
	rate := premium.Div(premium, new(big.Int).SetUint64(hours))

	if maxRateE18 != nil {
		max := maxRateE18.ToBig()
		neg := new(big.Int).Neg(max)
		if rate.Cmp(max) > 0 {
			rate.Set(max)
		} else if rate.Cmp(neg) < 0 {
			rate.Set(neg)
		}
	}
	return rate, nil
}

// FundingAmount returns the absolute funding payment for a position of size
// base units at mark price, given a signed rate scaled by 1e18 (spec §3.4):
//
//	amount = floor(notional(size, mark) * |rate| / 1e18)
//
// The sign of the rate is the caller's to interpret: positive means longs pay,
// negative means shorts pay. A zero rate yields zero.
func (m Market) FundingAmount(size *uint256.Int, mark Price, rateScaled *big.Int) (*uint256.Int, error) {
	if rateScaled == nil || rateScaled.Sign() == 0 || size == nil || size.IsZero() {
		return uint256.NewInt(0), nil
	}
	if rateScaled.Sign() < 0 {
		rateScaled = new(big.Int).Neg(rateScaled)
	}
	notional, err := m.notionalChecked(size, mark, false)
	if err != nil {
		return nil, err
	}
	prod, overflow := new(uint256.Int).MulOverflow(notional, uint256.MustFromBig(rateScaled))
	if overflow {
		return nil, ErrAmountTooLarge
	}
	return prod.Div(prod, uint256.NewInt(scaleE18)), nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd backend; go test ./perp -run Funding -v`
Expected: PASS (8 tests)

- [ ] **Step 5: 提交**

```bash
git add backend/perp/funding.go backend/perp/funding_test.go
git commit -m "feat(perp): signed funding rate and per-position funding amount"
```

---

### Task 3: 未实现盈亏（按标记价）

**Files:**
- Modify: `backend/perp/engine.go`（文件末尾新增方法）
- Test: `backend/perp/engine_test.go`（追加测试）

**Interfaces:**
- Consumes: `Position`, `Market.Notional`.
- Produces:
  - `func (m Market) UnrealizedPnL(side Side, size *uint256.Int, entry, mark Price) (*big.Int, error)`（返回有符号 quote，`big.Int` 以支持负值）

注意：使用 `big.Int` 而非 `uint256` 是为了能表示负数；调用方（服务层 JSON）负责格式化。这与 `PnLSettlement.Profit`（`uint256` + 符号约定）不同，后者是**已实现**盈亏，保持不变。

- [ ] **Step 1: 写失败测试**

```go
// append to backend/perp/engine_test.go
func TestUnrealizedPnL(t *testing.T) {
	m := testMarkMarket()
	size, _ := uint256.FromDecimal("10000000000000000000") // 10 base

	// long entry 2000, mark 2100 -> +1000 quote
	got, err := m.UnrealizedPnL(SideLong, size, 2000, 2100)
	if err != nil {
		t.Fatalf("UnrealizedPnL: %v", err)
	}
	want, _ := new(big.Int).SetString("1000000000000000000000", 10) // 1000e18
	if got.Cmp(want) != 0 {
		t.Fatalf("long: want %s, got %s", want, got)
	}

	// short entry 2000, mark 2100 -> -1000 quote
	got, err = m.UnrealizedPnL(SideShort, size, 2000, 2100)
	if err != nil {
		t.Fatalf("UnrealizedPnL: %v", err)
	}
	want, _ = new(big.Int).SetString("-1000000000000000000000", 10)
	if got.Cmp(want) != 0 {
		t.Fatalf("short: want %s, got %s", want, got)
	}

	// zero size -> zero
	got, err = m.UnrealizedPnL(SideLong, uint256.NewInt(0), 2000, 2100)
	if err != nil {
		t.Fatalf("UnrealizedPnL: %v", err)
	}
	if got.Sign() != 0 {
		t.Fatalf("zero size: want 0, got %s", got)
	}
}

func TestUnrealizedPnLRequiresPositiveMark(t *testing.T) {
	m := testMarkMarket()
	if _, err := m.UnrealizedPnL(SideLong, uint256.NewInt(1), 2000, 0); err == nil {
		t.Fatal("want error for zero mark")
	}
}
```

需要在 `engine_test.go` 顶部 import `math/big`（若尚未导入）。

- [ ] **Step 2: 运行确认失败**

Run: `cd backend; go test ./perp -run UnrealizedPnL -v`
Expected: FAIL with "m.UnrealizedPnL undefined"

- [ ] **Step 3: 实现**

```go
// append to backend/perp/engine.go

// UnrealizedPnL returns the signed quote PnL of a position of size base units
// at mark price (spec §3.7): long profits when mark > entry, short the reverse.
// It returns a signed big.Int because the value can be negative; it is a pure
// function over its arguments.
func (m Market) UnrealizedPnL(side Side, size *uint256.Int, entry, mark Price) (*big.Int, error) {
	if mark == 0 {
		return nil, fmt.Errorf("%w: unrealized PnL requires a positive mark price", ErrBadRequest)
	}
	if size == nil || size.IsZero() {
		return new(big.Int), nil
	}
	entryNotional, err := m.notionalChecked(size, entry, false)
	if err != nil {
		return nil, err
	}
	markNotional, err := m.notionalChecked(size, mark, false)
	if err != nil {
		return nil, err
	}
	if side == SideLong {
		return new(big.Int).Sub(markNotional.ToBig(), entryNotional.ToBig()), nil
	}
	return new(big.Int).Sub(entryNotional.ToBig(), markNotional.ToBig()), nil
}
```

需要在 `engine.go` 顶部 import `"math/big"`。

- [ ] **Step 4: 运行确认通过**

Run: `cd backend; go test ./perp -run UnrealizedPnL -v`
Expected: PASS (2 tests)

- [ ] **Step 5: 提交**

```bash
git add backend/perp/engine.go backend/perp/engine_test.go
git commit -m "feat(perp): mark-price unrealized PnL"
```

---

### Task 4: WAL 新增 mark/funding 指令

**Files:**
- Modify: `backend/services/perp_journal.go`
- Test: `backend/services/perp_journal_test.go`（追加）

**Interfaces:**
- Consumes: `perp.MarketID`、`perp.Price`。
- Produces:
  - `type MarkCmd struct { Market perp.MarketID; Index, Mark perp.Price; At int64 }`
  - `type FundingCmd struct { Market perp.MarketID; Index uint64; Rate string; Mark perp.Price; At int64; Legs []FundingLeg }`
  - `type FundingLeg struct { Payer, Receiver ledger.AccountID; Amount *uint256.Int }`
  - `func (j *PerpJournal) AppendMark(c MarkCmd) error`
  - `func (j *PerpJournal) AppendFunding(c FundingCmd) error`
  - `Instruction` 扩展 `Kind` 为 `"mark"`/`"funding"`，并带 `Mark *MarkCmd`、`Funding *FundingCmd`。

- [ ] **Step 1: 先读现有 journal 结构**

Run: `cat backend/services/perp_journal.go`（或直接 Read 工具）
确认 `Instruction` 的 JSON 结构、`AppendPlace`/`AppendCancel` 的写法，照同一模式扩展。**不要改动已有字段名**，否则旧 WAL 无法重放。

- [ ] **Step 2: 写失败测试**

```go
func TestPerpJournalAppendsMarkAndFunding(t *testing.T) {
	dir := t.TempDir()
	j, err := NewPerpJournal(dir)
	if err != nil {
		t.Fatalf("NewPerpJournal: %v", err)
	}
	defer j.Close()

	if err := j.AppendMark(MarkCmd{Market: 1, Index: 2000, Mark: 2001, At: 100}); err != nil {
		t.Fatalf("AppendMark: %v", err)
	}
	payer := ledger.AccountID([20]byte{1})
	receiver := ledger.AccountID([20]byte{2})
	amt, _ := uint256.FromDecimal("1000000000000000")
	if err := j.AppendFunding(FundingCmd{
		Market: 1, Index: 7, Rate: "1000000000000000", Mark: 2001, At: 200,
		Legs: []FundingLeg{{Payer: payer, Receiver: receiver, Amount: amt}},
	}); err != nil {
		t.Fatalf("AppendFunding: %v", err)
	}

	ins, err := j.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(ins) != 2 {
		t.Fatalf("want 2 instructions, got %d", len(ins))
	}
	if ins[0].Kind != "mark" || ins[0].Mark == nil || ins[0].Mark.Mark != 2001 {
		t.Fatalf("bad mark instruction: %+v", ins[0])
	}
	if ins[1].Kind != "funding" || ins[1].Funding == nil || len(ins[1].Funding.Legs) != 1 {
		t.Fatalf("bad funding instruction: %+v", ins[1])
	}
	if ins[1].Funding.Legs[0].Amount.Cmp(amt) != 0 {
		t.Fatalf("funding leg amount mismatch")
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `cd backend; go test ./services -run PerpJournalAppendsMarkAndFunding -v`
Expected: FAIL with "undefined: MarkCmd"

- [ ] **Step 4: 实现**

按 `AppendPlace` 的完全同构写法实现 `AppendMark`/`AppendFunding`，`Instruction` 加两个可选指针字段，
`Replay` 中按 `Kind` 分派到已有解析路径（未知 kind 保持现有的严格报错行为）。

- [ ] **Step 5: 运行确认通过**

Run: `cd backend; go test ./services -run PerpJournal -v`
Expected: PASS（含既有测试）

- [ ] **Step 6: 提交**

```bash
git add backend/services/perp_journal.go backend/services/perp_journal_test.go
git commit -m "feat(perp): journal mark and funding instructions"
```

---

### Task 5: 服务层价格拉取与陈旧判定

**Files:**
- Create: `backend/services/perp_prices.go`
- Test: `backend/services/perp_prices_test.go`

**Interfaces:**
- Consumes: `perp.PriceSource`（需先在 Task 6 定义，但本任务可先在 `services` 包里定义 `PriceSource` 别名接口以避免循环依赖——见下）。
- Produces:
  - 在 `backend/perp/price.go` 中新增 `type PriceSource interface { Index(m MarketID) (Price, bool) }`（无 ctx，保持内核无 I/O 语义；真实实现由服务层注入）
  - `backend/services/perp_prices.go`：`type PerpPriceTracker struct { ... }`，方法 `Observe(m, index, now)`、`Mark(m, index, bestBid, bestAsk, hasBid, hasAsk, now) (perp.Price, bool)`、`Stale(m, now) bool`

- [ ] **Step 1: 先在 perp/price.go 追加 PriceSource**

```go
// PriceSource supplies an external index price per market. It is injected by
// the service layer; the kernel itself never calls it.
type PriceSource interface {
	Index(m MarketID) (Price, bool)
}
```

- [ ] **Step 2: 写失败测试**

```go
func TestPerpPriceTrackerReturnsMarkWhenFresh(t *testing.T) {
	m := perp.Market{ID: 1, Symbol: "ETH-PERP", Base: 1, Quote: 2, BaseDecimals: 18, MaxLeverage: 10}
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	mark, ok := tr.Mark(m, 2000, 2001, 2003, true, true, time.Unix(1000, 0))
	if !ok {
		t.Fatal("want a fresh mark")
	}
	if mark != 2001 {
		t.Fatalf("want 2001, got %d", mark)
	}
}

func TestPerpPriceTrackerRejectsStaleIndex(t *testing.T) {
	m := perp.Market{ID: 1, Symbol: "ETH-PERP", Base: 1, Quote: 2, BaseDecimals: 18, MaxLeverage: 10}
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	tr.Observe(1, 2000, time.Unix(1000, 0))
	if !tr.Stale(1, time.Unix(1000+61, 0)) {
		t.Fatal("want stale after max price age")
	}
	if tr.Stale(1, time.Unix(1000+30, 0)) {
		t.Fatal("want fresh within max price age")
	}
}

func TestPerpPriceTrackerFallsBackToLastMark(t *testing.T) {
	m := perp.Market{ID: 1, Symbol: "ETH-PERP", Base: 1, Quote: 2, BaseDecimals: 18, MaxLeverage: 10}
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	first, ok := tr.Mark(m, 2000, 2001, 2003, true, true, time.Unix(1000, 0))
	if !ok || first != 2001 {
		t.Fatalf("first mark: %d %v", first, ok)
	}
	// Index goes stale but within max mark staleness: last mark is reused.
	got, ok := tr.Mark(m, 0, 0, 0, false, false, time.Unix(1000+120, 0))
	if !ok || got != 2001 {
		t.Fatalf("fallback: want 2001, got %d (ok=%v)", got, ok)
	}
	// Beyond max mark staleness: no mark at all.
	if _, ok := tr.Mark(m, 0, 0, 0, false, false, time.Unix(1000+400, 0)); ok {
		t.Fatal("want no mark after max staleness")
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `cd backend; go test ./services -run PerpPriceTracker -v`
Expected: FAIL with "undefined: NewPerpPriceTracker"

- [ ] **Step 4: 实现**

`PerpPriceTracker` 内部 `map[perp.MarketID]priceState{index, markedAt, lastMark, lastMarkAt}`，
`Observe` 记录指数价时戳，`Mark` 在指数价新鲜时按 `Market.MarkPrice` 推导并记录，否则在
`maxMarkStaleness` 内回退到 `lastMark`。`Stale` 只看指数价时戳。全部方法加互斥锁。

- [ ] **Step 5: 运行确认通过**

Run: `cd backend; go test ./services -run PerpPriceTracker -v`
Expected: PASS (3 tests)

- [ ] **Step 6: 提交**

```bash
git add backend/perp/price.go backend/services/perp_prices.go backend/services/perp_prices_test.go
git commit -m "feat(perp): price tracker with staleness and mark fallback"
```

---

### Task 6: 资金费结算循环

**Files:**
- Create: `backend/services/perp_funding.go`
- Test: `backend/services/perp_funding_test.go`

**Interfaces:**
- Consumes: `PerpOrderBookService.engine`、`ledger`、`PerpJournal.AppendFunding`。
- Produces:
  - `func (s *PerpOrderBookService) SettleFunding(m perp.MarketID, index, mark perp.Price, rateScaled *big.Int, at int64) (FundingResult, error)`
  - `func (s *PerpOrderBookService) FundingDebtOf(a ledger.AccountID, m perp.MarketID) *uint256.Int`
  - `type FundingResult struct { Legs []FundingLeg; Skipped []FundingSkip }`

- [ ] **Step 1: 写失败测试**

```go
func TestSettleFundingMovesQuoteFromLongToShort(t *testing.T) {
	// Build a service with two accounts holding opposite positions at mark 2000.
	// Long 10 base, short 10 base; 0.1% rate -> long pays 20 quote to short.
	svc, l := newFundingTestService(t)
	// (helper: open long for A and short for B at 2000, size 10e18)
	...
	rate, _ := new(big.Int).SetString("1000000000000000", 10) // 0.001
	res, err := svc.SettleFunding(1, 2000, 2000, rate, 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Legs) != 1 {
		t.Fatalf("want 1 netted leg, got %d", len(res.Legs))
	}
	want, _ := uint256.FromDecimal("20000000000000000000") // 20e18
	if res.Legs[0].Amount.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, res.Legs[0].Amount)
	}
	if l.Balance(longAcct, 1).Cmp(...) ... // assert balances moved
}
```

（测试需先构造一个真实开仓：用 `svc.Place` 让 A 做多、B 做空成交。具体辅助函数写在测试文件内。）

- [ ] **Step 2: 运行确认失败**

Run: `cd backend; go test ./services -run SettleFunding -v`
Expected: FAIL with "s.SettleFunding undefined"

- [ ] **Step 3: 实现**

按 spec §3.4：遍历 `engine` 的仓位快照 → 每边算 `FundingAmount` → 多空净额配对 → 每条 leg 先落
`AppendFunding` 再 `ledger.Apply(TransferOp)`（幂等键 `perp:funding:<market>:<index>:<payer><receiver>`）；
余额不足时改记欠账（`debts` map）并记 `Skipped`。

注意：`engine` 没有导出「遍历所有仓位」的方法。需在 Task 7 一并新增 `Engine.Positions(m MarketID) []Position`（返回副本）。本任务可先用该方法，Task 7 补。

- [ ] **Step 4: 运行确认通过**

Run: `cd backend; go test ./services -run SettleFunding -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add backend/services/perp_funding.go backend/services/perp_funding_test.go
git commit -m "feat(perp): periodic funding settlement with debts"
```

---

### Task 7: Engine.Positions 导出 + 服务装配 + 配置

**Files:**
- Modify: `backend/perp/engine.go`（新增 `Positions`）
- Modify: `backend/services/perp_order_book.go`（`SetPriceSource`、`MarkPrice`、`FundingState`、runner 中拉价）
- Modify: `backend/config/config.go`（新增 `PerpFundingConfig`）
- Modify: `backend/main.go`（装配 `StaticPriceSource`）
- Modify: `README.md`

**Interfaces:**
- Produces:
  - `func (e *Engine) Positions(m MarketID) []Position`（按 Account 升序，稳定顺序）
  - `func (s *PerpOrderBookService) SetPriceSource(p perp.PriceSource)`
  - `func (s *PerpOrderBookService) MarkPrice(m perp.MarketID) (perp.Price, bool)`

- [ ] **Step 1: Engine.Positions 测试**

```go
func TestEnginePositionsSortedByAccount(t *testing.T) {
	// open positions for accounts 3 and 1 in market 1; assert order 1 then 3.
}
```

- [ ] **Step 2: 跑测试确认失败，然后实现并跑通**

`Positions` 遍历 `e.positions[m]`，收集 `clone()`，按 `Account` 字节序排序。

- [ ] **Step 3: 服务装配测试**

`TestPerpServiceWithoutPriceSourceMatches6a`：不调用 `SetPriceSource` 时，`MarkPrice` 返回 `(0,false)`，
`Place` 行为与既有测试一致（既有测试必须全绿）。

- [ ] **Step 4: 配置 + main 装配**

`PERP_INDEX_PRICES` 解析为 `services.StaticPriceSource`，在 `perp` 挂载后注入。格式非法则启动失败。

- [ ] **Step 5: README 追加 6b 章节**

包含：标记价公式、资金费率公式、配置表、残余信任（oracle 信任、运营方兜底不平衡）。

- [ ] **Step 6: 全量验证 + 提交**

```bash
cd backend; go build ./... && go test ./... -count=1
cd ..; npx hardhat test
git add -A; git commit -m "feat(perp): wire mark price and funding into the service and config"
```

---

### Task 8: 回归与收尾

- [ ] **Step 1: Go 全量**

`cd backend; go build ./...; go test ./... -count=1`
Expected: 全绿

- [ ] **Step 2: 合约回归**

`npx hardhat test`
Expected: 全绿（本子项目不改合约）

- [ ] **Step 3: 更新 6a spec 的状态表**

把 6b 行改为「✅ 已实现」，并在 6a 的「残余信任假设」中把「无标记价」一条改为指向 6b。

- [ ] **Step 4: 提交并开 PR**

```bash
git add -A; git commit -m "docs(perp): mark subproject 6b as implemented"
gh pr create --base feat/gateway --head feat/perp-funding --title "feat: mark price and funding rate for perpetuals" --body "..."
```

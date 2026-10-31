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
	MarkConfig   MarkConfig     // mark-price derivation (zero value = defaults)
	RiskConfig   RiskConfig     // maintenance margin and liquidation fee (zero = defaults)
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
	if m.MinQty != nil && m.MaxQty != nil && m.MinQty.Cmp(m.MaxQty) > 0 {
		return fmt.Errorf("perp: market %s: min qty %s exceeds max qty %s", m.Symbol, m.MinQty, m.MaxQty)
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
// the order price with the given leverage: ceil(notional(qty,price) / leverage),
// where notional is the floor quote value. It is what RequiredMargin returns and
// what the service layer pre-freezes. Note this rounds the DIVISION up, not the
// notional: `floor(ceil(qty*price/scale)/leverage)` would under-freeze by one
// quote unit (qty 101, price 100, decimals 2, 10x -> 10 instead of 11).
func (m Market) marginCeil(qty *uint256.Int, price Price, leverage uint32) (*uint256.Int, error) {
	if leverage < 1 {
		return nil, fmt.Errorf("%w: leverage must be >= 1", ErrBadRequest)
	}
	notional, err := m.notionalChecked(qty, price, false)
	if err != nil {
		return nil, err
	}
	return divCeil(notional, uint256.NewInt(uint64(leverage)))
}

// divCeil returns ceil(a/b) for b > 0, with an explicit overflow check so a
// pathological a can never silently wrap into a tiny freeze.
func divCeil(a, b *uint256.Int) (*uint256.Int, error) {
	if b.IsZero() {
		return nil, fmt.Errorf("%w: division by zero", ErrBadRequest)
	}
	sum, overflow := new(uint256.Int).AddOverflow(a, new(uint256.Int).Sub(b, uint256.NewInt(1)))
	if overflow {
		return nil, ErrAmountTooLarge
	}
	return new(uint256.Int).Div(sum, b), nil
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

// marginCeilAmount derives order-level margin from a quote notional cap:
// ceil(quoteAmount / leverage). Used by market orders, which have no price.
func (m Market) marginCeilAmount(quoteAmount *uint256.Int, leverage uint32) (*uint256.Int, error) {
	if leverage < 1 {
		return nil, fmt.Errorf("%w: leverage must be >= 1", ErrBadRequest)
	}
	if quoteAmount == nil {
		return uint256.NewInt(0), nil
	}
	return divCeil(quoteAmount, uint256.NewInt(uint64(leverage)))
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

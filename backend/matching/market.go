// Package matching is a pure, deterministic exchange kernel: it has no I/O,
// never reads the clock, never uses randomness, and never touches the ledger's
// balances. Given the same market configuration and the same instruction
// sequence it must produce byte-identical results, which is what makes crash
// replay safe.
package matching

import (
	"fmt"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Side is the direction of an order.
type Side uint8

const (
	SideBuy Side = iota
	SideSell
)

func (s Side) String() string {
	if s == SideBuy {
		return "buy"
	}
	return "sell"
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
	// TIFGTC rests whatever remains after matching.
	TIFGTC TIF = iota
	// TIFIOC cancels whatever remains after matching; it never rests.
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

// OrderID and Seq are engine-assigned, replay-reproducible counters.
type OrderID uint64

// Seq is the time-priority sequence number (never a timestamp).
type Seq uint64

// Price is quote minimal units per one whole base unit.
type Price uint64

// TradeID is the trade sequence number, from which the ledger idempotency keys
// are derived.
type TradeID uint64

// MarketID identifies a market configuration.
type MarketID uint32

// Market is the immutable configuration of one tradable pair.
type Market struct {
	ID           MarketID
	Symbol       string
	Base, Quote  ledger.AssetID
	BaseDecimals uint8        // notional = qty*price / 10^BaseDecimals
	MinQty       *uint256.Int // optional minimum base quantity
	// MaxQty bounds the base quantity of any accepted order so qty*price can
	// never overflow uint256. Nil means defaultMaxQty.
	MaxQty *uint256.Int
}

// maxBaseDecimals caps the exponent so 10^decimals stays representable in a
// uint256 (with headroom for the +10^d-1 term in the ceil formula).
const maxBaseDecimals = 76

// defaultMaxQty bounds Qty when Market.MaxQty is nil. 2^128 is chosen so that
// qty*price <= 2^128*(2^64-1) < 2^192, comfortably inside uint256: the
// multiplication at the heart of Notional can never wrap for an accepted order.
var defaultMaxQty = new(uint256.Int).Lsh(uint256.NewInt(1), 128)

// maxQty returns the effective base-quantity bound for this market.
func (m Market) maxQty() *uint256.Int {
	if m.MaxQty != nil {
		return m.MaxQty
	}
	return defaultMaxQty
}

// Validate checks the market configuration in isolation. It says nothing about
// order legality (e.g. Market+GTC is rejected by Place, not here).
func (m Market) Validate() error {
	if m.Symbol == "" {
		return fmt.Errorf("matching: market %d: empty symbol", m.ID)
	}
	if m.Base == m.Quote {
		return fmt.Errorf("matching: market %s: base and quote must differ (both %d)", m.Symbol, m.Base)
	}
	if m.BaseDecimals > maxBaseDecimals {
		return fmt.Errorf("matching: market %s: base decimals %d exceeds %d", m.Symbol, m.BaseDecimals, maxBaseDecimals)
	}
	if m.MinQty != nil && m.MinQty.Sign() < 0 {
		return fmt.Errorf("matching: market %s: negative min qty", m.Symbol)
	}
	if m.MaxQty != nil && m.MaxQty.Sign() < 0 {
		return fmt.Errorf("matching: market %s: negative max qty", m.Symbol)
	}
	return nil
}

// scale returns 10^BaseDecimals as a fresh uint256.
func (m Market) scale() *uint256.Int {
	return pow10(m.BaseDecimals)
}

// Notional returns floor(qty*price / 10^BaseDecimals), the quote amount for a
// trade of qty base units at price. It panics on price == 0: a zero price is
// not a valid level (limit orders require price > 0 and book levels are always
// priced), so silently returning 0 would hide a kernel bug. It also panics on
// uint256 overflow: wrapping would silently produce a smaller (or zero)
// notional, and a freeze derived from a wrapped notional cannot cover the fill.
func (m Market) Notional(qty *uint256.Int, p Price) *uint256.Int {
	if p == 0 {
		panic("matching: Notional called with zero price")
	}
	v, err := m.notionalChecked(qty, p, false)
	if err != nil {
		panic("matching: Notional overflow: " + err.Error())
	}
	return v
}

// NotionalCeil returns ceil(qty*price / 10^BaseDecimals), used for buy
// reservations so the freeze always covers the floor-deducted fills. Like
// Notional it panics on price == 0 and on uint256 overflow.
func (m Market) NotionalCeil(qty *uint256.Int, p Price) *uint256.Int {
	if p == 0 {
		panic("matching: NotionalCeil called with zero price")
	}
	v, err := m.notionalChecked(qty, p, true)
	if err != nil {
		panic("matching: NotionalCeil overflow: " + err.Error())
	}
	return v
}

// notionalChecked computes floor(qty*price/10^BaseDecimals), or ceil when ceil
// is true, reporting uint256 overflow instead of wrapping. p == 0 is an error:
// callers must reject a zero price rather than derive a freeze from it. The
// engine always routes through this (via validate/reserveFor), so the panicking
// wrappers above are unreachable from an accepted request and exist only as
// defense in depth.
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

// pow10 returns 10^n. It is only ever called with n <= maxBaseDecimals, and is
// recomputed rather than cached because Market is a value type.
func pow10(n uint8) *uint256.Int {
	out := uint256.NewInt(1)
	ten := uint256.NewInt(10)
	for i := uint8(0); i < n; i++ {
		out.Mul(out, ten)
	}
	return out
}

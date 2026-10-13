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
}

// maxBaseDecimals caps the exponent so 10^decimals stays representable in a
// uint256 (with headroom for the +10^d-1 term in the ceil formula).
const maxBaseDecimals = 76

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
	return nil
}

// scale returns 10^BaseDecimals as a fresh uint256.
func (m Market) scale() *uint256.Int {
	return pow10(m.BaseDecimals)
}

// Notional returns floor(qty*price / 10^BaseDecimals), the quote amount for a
// trade of qty base units at price. It panics on price == 0: a zero price is
// not a valid level (limit orders require price > 0 and book levels are always
// priced), so silently returning 0 would hide a kernel bug.
func (m Market) Notional(qty *uint256.Int, p Price) *uint256.Int {
	if p == 0 {
		panic("matching: Notional called with zero price")
	}
	if qty == nil {
		return uint256.NewInt(0)
	}
	prod := new(uint256.Int).Mul(qty, uint256.NewInt(uint64(p)))
	return new(uint256.Int).Div(prod, m.scale())
}

// NotionalCeil returns ceil(qty*price / 10^BaseDecimals), used for buy
// reservations so the freeze always covers the floor-deducted fills. Like
// Notional it panics on price == 0.
func (m Market) NotionalCeil(qty *uint256.Int, p Price) *uint256.Int {
	if p == 0 {
		panic("matching: NotionalCeil called with zero price")
	}
	if qty == nil {
		return uint256.NewInt(0)
	}
	s := m.scale()
	prod := new(uint256.Int).Mul(qty, uint256.NewInt(uint64(p)))
	prod.Add(prod, new(uint256.Int).Sub(s, uint256.NewInt(1)))
	return new(uint256.Int).Div(prod, s)
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

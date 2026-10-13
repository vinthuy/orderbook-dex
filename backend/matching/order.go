package matching

import (
	"container/list"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Order is one resting or in-flight order. The exported fields are the
// authoritative record; reserved and elem are internal kernel state.
type Order struct {
	ID      OrderID
	Seq     Seq
	Market  MarketID
	Account ledger.AccountID
	Side    Side
	Type    Type
	TIF     TIF
	Price   Price // limit: required (> 0); market: 0 (no price constraint)
	Qty     *uint256.Int
	// MaxNotional is the market-buy spending cap (quote). It may also be set on
	// a limit buy as a sanity bound, but the limit reservation is derived from
	// price, not from this field.
	MaxNotional *uint256.Int
	Filled      *uint256.Int
	Status      Status

	reserved *uint256.Int  // currently frozen: quote for buys, base for sells
	elem     *list.Element // position in its level's FIFO, nil when not resting
}

// Remaining returns Qty - Filled as a fresh copy. Filled never exceeds Qty
// (no input in the remaining amount ever exceeds Qty).
func (o *Order) Remaining() *uint256.Int {
	return new(uint256.Int).Sub(o.Qty, o.Filled)
}

// IsOpen reports whether the order still rests in the book.
func (o *Order) IsOpen() bool {
	return o.Status == StatusNew || o.Status == StatusPartiallyFilled
}

// holdAsset returns the asset the order freezes: quote for buys, base for sells.
func (o *Order) holdAsset(m Market) ledger.AssetID {
	if o.Side == SideBuy {
		return m.Quote
	}
	return m.Base
}

// clone returns a deep copy so callers can never mutate kernel state (in
// particular the *uint256.Int fields) through a returned order.
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

// cloneInt copies a uint256, preserving nil.
func cloneInt(v *uint256.Int) *uint256.Int {
	if v == nil {
		return nil
	}
	return new(uint256.Int).Set(v)
}

// minUint returns min(a, b) as a fresh copy.
func minUint(a, b *uint256.Int) *uint256.Int {
	if a.Cmp(b) <= 0 {
		return new(uint256.Int).Set(a)
	}
	return new(uint256.Int).Set(b)
}

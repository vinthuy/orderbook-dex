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
	EntryPrice Price        // quantity-weighted average entry price
	Margin     *uint256.Int // total frozen margin (quote)
	Leverage   uint32       // user-selected leverage (<= market max)

	// qtyPrice is the cumulative Σ(qty·price) over the fills that opened this
	// position, kept so EntryPrice can be recomputed exactly as
	// floor(qtyPrice/size) on increases without drifting (spec §4.6.2).
	qtyPrice *uint256.Int
}

func (p *Position) clone() *Position {
	if p == nil {
		return nil
	}
	c := *p
	c.Size = cloneInt(p.Size)
	c.Margin = cloneInt(p.Margin)
	c.qtyPrice = cloneInt(p.qtyPrice)
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
	Price   Price        // limit: required (>0); market: 0
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

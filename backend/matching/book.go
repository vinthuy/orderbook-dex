package matching

import (
	"container/list"
	"fmt"
	"sort"

	"github.com/holiman/uint256"
)

// side is one half of a book: price levels ordered best-first (bids descending,
// asks ascending) plus the level lookup. prices and levels must always have the
// same key set, and an empty level is never left behind.
type side struct {
	desc   bool // true = bids (descending price), false = asks (ascending)
	prices []Price
	levels map[Price]*level
}

// level is one price point with its FIFO queue of resting orders.
type level struct {
	price Price
	total *uint256.Int // sum of Remaining() over fifo; invariant-checked
	fifo  *list.List   // of *Order, oldest first
}

func newSide(desc bool) *side {
	return &side{desc: desc, levels: make(map[Price]*level)}
}

// before reports whether price a is better (comes first) than b on this side.
func (s *side) before(a, b Price) bool {
	if s.desc {
		return a > b
	}
	return a < b
}

// insertPrice inserts p keeping prices ordered; the caller guarantees p is new.
func (s *side) insertPrice(p Price) {
	i := sort.Search(len(s.prices), func(i int) bool { return !s.before(s.prices[i], p) })
	s.prices = append(s.prices, 0)
	copy(s.prices[i+1:], s.prices[i:])
	s.prices[i] = p
}

// removePrice removes p, which the caller guarantees is present.
func (s *side) removePrice(p Price) {
	i := sort.Search(len(s.prices), func(i int) bool { return !s.before(s.prices[i], p) })
	if i >= len(s.prices) || s.prices[i] != p {
		panic(fmt.Sprintf("matching: side.removePrice: price %d not found", p))
	}
	s.prices = append(s.prices[:i], s.prices[i+1:]...)
}

// best returns the best-priced level, or nil when the side is empty.
func (s *side) best() *level {
	if len(s.prices) == 0 {
		return nil
	}
	return s.levels[s.prices[0]]
}

// bestOrder returns the oldest order at the best price, or nil when empty.
func (s *side) bestOrder() *Order {
	lvl := s.best()
	if lvl == nil {
		return nil
	}
	return lvl.fifo.Front().Value.(*Order)
}

// book is one market's order book plus the orders currently resting in it.
type book struct {
	m      Market
	bids   *side
	asks   *side
	orders map[OrderID]*Order // resting orders only
}

func newBook(m Market) *book {
	return &book{
		m:      m,
		bids:   newSide(true),
		asks:   newSide(false),
		orders: make(map[OrderID]*Order),
	}
}

func (b *book) sideFor(s Side) *side {
	if s == SideBuy {
		return b.bids
	}
	return b.asks
}

// add inserts an open order at the back of its price level's FIFO.
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

// cancel removes a still-open order from the book entirely, shrinking its
// level by its full remaining amount and dropping the level if it empties.
func (b *book) cancel(o *Order) {
	s := b.sideFor(o.Side)
	lvl := s.levels[o.Price]
	if lvl == nil || o.elem == nil {
		panic(fmt.Sprintf("matching: book.cancel: order %d is not resting", o.ID))
	}
	lvl.total.Sub(lvl.total, o.Remaining())
	lvl.fifo.Remove(o.elem)
	o.elem = nil
	b.dropLevelIfEmpty(s, lvl)
	delete(b.orders, o.ID)
}

// reduce applies a fill of qty to a resting order: it advances Filled and
// shrinks the level total by the same qty, so total stays the sum of the
// remaining amounts. A fully-filled order leaves the FIFO.
func (b *book) reduce(o *Order, qty *uint256.Int) {
	s := b.sideFor(o.Side)
	lvl := s.levels[o.Price]
	if lvl == nil || o.elem == nil {
		panic(fmt.Sprintf("matching: book.reduce: order %d is not resting", o.ID))
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

// dropLevelIfEmpty removes an emptied level from both maps; leaving it behind
// would break the "prices and levels keys agree" and "no empty level"
// invariants that the tests guard.
func (b *book) dropLevelIfEmpty(s *side, lvl *level) {
	if lvl.fifo.Len() != 0 {
		return
	}
	delete(s.levels, lvl.price)
	s.removePrice(lvl.price)
}

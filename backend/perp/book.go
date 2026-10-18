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

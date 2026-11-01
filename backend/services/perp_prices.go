package services

import (
	"sync"
	"time"

	"web3-amm-dex-backend/perp"
)

// Defaults for price freshness (spec §3.1).
const (
	DefaultMaxPriceAge     = time.Minute
	DefaultMaxMarkStaleAge = 5 * time.Minute
)

// priceState is the per-market price history the tracker keeps. Times are
// supplied by the caller so the tracker never reads the clock itself.
type priceState struct {
	index      perp.Price
	indexAt    time.Time
	hasIndex   bool
	lastMark   perp.Price
	lastMarkAt time.Time
	hasMark    bool
}

// PerpPriceTracker decides whether a market has a usable mark price right now.
// It is deliberately separate from the kernel: the kernel stays deterministic and
// the tracker holds the only wall-clock-dependent state in the pricing path.
//
// Rules (spec §3.1):
//   - an index price older than maxPriceAge is stale;
//   - a stale index falls back to the last good mark, for at most maxMarkStale;
//   - past maxMarkStale there is no mark at all (funding skips the round).
type PerpPriceTracker struct {
	maxPriceAge     time.Duration
	maxMarkStaleAge time.Duration

	mu     sync.Mutex
	states map[perp.MarketID]*priceState
}

// NewPerpPriceTracker builds a tracker. Non-positive durations fall back to the
// package defaults.
func NewPerpPriceTracker(maxPriceAge, maxMarkStaleAge time.Duration) *PerpPriceTracker {
	if maxPriceAge <= 0 {
		maxPriceAge = DefaultMaxPriceAge
	}
	if maxMarkStaleAge <= 0 {
		maxMarkStaleAge = DefaultMaxMarkStaleAge
	}
	return &PerpPriceTracker{
		maxPriceAge:     maxPriceAge,
		maxMarkStaleAge: maxMarkStaleAge,
		states:          make(map[perp.MarketID]*priceState),
	}
}

func (t *PerpPriceTracker) state(m perp.MarketID) *priceState {
	st := t.states[m]
	if st == nil {
		st = &priceState{}
		t.states[m] = st
	}
	return st
}

// Observe records a fresh index price for a market.
func (t *PerpPriceTracker) Observe(m perp.MarketID, index perp.Price, now time.Time) {
	if index == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(m)
	st.index = index
	st.indexAt = now
	st.hasIndex = true
}

// Stale reports whether the market's index price is missing or older than
// maxPriceAge.
func (t *PerpPriceTracker) Stale(m perp.MarketID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[m]
	return st == nil || !st.hasIndex || now.Sub(st.indexAt) > t.maxPriceAge
}

// Mark derives and records the mark price for a market from the given index and
// book. It returns false when no usable price exists at all:
//
//   - a fresh index derives a new mark via Market.MarkPrice;
//   - a stale index reuses the last mark while it is younger than maxMarkStale;
//   - otherwise there is no mark.
func (t *PerpPriceTracker) Mark(m perp.Market, index perp.Price, bestBid, bestAsk perp.Price, hasBid, hasAsk bool, now time.Time) (perp.Price, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(m.ID)

	fresh := index != 0
	if fresh {
		mark, err := m.MarkPrice(index, bestBid, bestAsk, hasBid, hasAsk)
		if err == nil {
			st.lastMark = mark
			st.lastMarkAt = now
			st.hasMark = true
			return mark, true
		}
		// Fall through to the stored mark: a derivation failure must not make
		// the market unpriced.
	}

	if st.hasMark && now.Sub(st.lastMarkAt) <= t.maxMarkStaleAge {
		return st.lastMark, true
	}
	return 0, false
}

// LastMark returns the most recent mark without deriving a new one.
func (t *PerpPriceTracker) LastMark(m perp.MarketID) (perp.Price, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[m]
	if st == nil || !st.hasMark {
		return 0, false
	}
	return st.lastMark, true
}

// IndexPrice returns the last observed index price and whether one is fresh.
func (t *PerpPriceTracker) IndexPrice(m perp.MarketID) (perp.Price, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[m]
	if st == nil || !st.hasIndex {
		return 0, false
	}
	return st.index, true
}

// restore reinstates a journaled mark exactly, bypassing derivation. Replay
// must reproduce history, not recompute it from whatever the book looks like
// after the fact.
func (t *PerpPriceTracker) restore(m perp.MarketID, mark perp.Price, at time.Time) {
	if mark == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(m)
	st.lastMark = mark
	st.lastMarkAt = at
	st.hasMark = true
}

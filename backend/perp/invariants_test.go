package perp

import (
	"math/rand"
	"testing"

	"github.com/holiman/uint256"
)

// TestMarginConservation guards spec §4.6.4:
//
//	Σ(order-level freeze) == Σ(Releases) + Σ(resting reserved) + Σ(position Margin)
//
// It runs a sequence that opens, partially reduces, flips, cancels and STP's,
// using non-exact quantities so the ceil-vs-floor residual is non-zero, and it
// re-checks the identity after every step. Only still-open (resting) orders are
// counted, so a residual leaked onto a closed order breaks the identity.
func TestMarginConservation(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	var totalFrozen, totalReleases uint64

	assert := func(step string) {
		t.Helper()
		resting := uint64(0)
		for _, o := range e.all {
			if o.IsOpen() && o.reserved != nil {
				resting += o.reserved.Uint64()
			}
		}
		pos := uint64(0)
		for _, byAcct := range e.positions {
			for _, p := range byAcct {
				pos += p.Margin.Uint64()
			}
		}
		if totalFrozen != totalReleases+resting+pos {
			t.Fatalf("conservation @%s: frozen=%d releases=%d resting=%d pos=%d",
				step, totalFrozen, totalReleases, resting, pos)
		}
	}

	place := func(req PlaceRequest) Result {
		t.Helper()
		rm, err := e.RequiredMargin(req)
		if err != nil {
			t.Fatalf("RequiredMargin: %v", err)
		}
		totalFrozen += rm.Uint64()
		res, err := e.Place(req)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		for _, r := range res.Releases {
			totalReleases += r.Amount.Uint64()
		}
		assert("place")
		return res
	}
	cancel := func(a byte, oid OrderID) {
		t.Helper()
		res, err := e.Cancel(1, acct(a), oid)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		for _, r := range res.Releases {
			totalReleases += r.Amount.Uint64()
		}
		assert("cancel")
	}

	// (1) resting short that will later be partially filled (creates residual).
	restID := place(shortReq(acct(1), 100, 101, 10)).Order.ID
	// (2) resting long (liquidity for the reduce/flip below).
	place(longReq(acct(2), 90, 200, 10))
	// (3) open: acct3 long crosses acct1's short, leaving acct1 partially filled.
	place(longReq(acct(3), 100, 100, 10))
	// (4) partial reduce: acct3 short crosses acct2's bid, closing 40 of its long.
	place(shortReq(acct(3), 90, 40, 10))
	// (5) flip: acct3 short more than its remaining long -> closes 60 and opens 40 short.
	place(shortReq(acct(3), 90, 100, 10))
	// (6) cancel acct1's remaining resting order (releases its residual).
	cancel(1, restID)
	// (7) STP: acct2 short crosses its own resting bid -> maker cancelled + residual released.
	place(shortReq(acct(2), 90, 10, 10))
}

// TestDeterministicReplay guards spec §4.6.6: two engines fed the identical
// instruction stream produce byte-identical trades, releases, PnL and final
// positions (including Margin and Leverage).
func TestDeterministicReplay(t *testing.T) {
	type state struct {
		e        *Engine
		trades   []Trade
		releases []MarginRelease
		pnl      []PnLSettlement
	}
	build := func() state {
		e, _ := NewEngine([]Market{mkMarket()})
		rng := rand.New(rand.NewSource(42))
		s := state{e: e}
		for i := 0; i < 200; i++ {
			side := SideLong
			if rng.Intn(2) == 1 {
				side = SideShort
			}
			a := acct(byte(1 + rng.Intn(5)))
			price := Price(90 + rng.Intn(21))
			qty := uint64(1 + rng.Intn(50))
			req := PlaceRequest{Market: 1, Account: a, Side: side, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: 10}
			res, _ := e.Place(req)
			s.trades = append(s.trades, res.Trades...)
			s.releases = append(s.releases, res.Releases...)
			s.pnl = append(s.pnl, res.PnL...)
			if rng.Intn(4) == 0 {
				// Cancel a resting order; pick the lowest open OrderID so the
				// choice is deterministic (map iteration order is randomized).
				var victim OrderID
				for _, o := range e.all {
					if o.IsOpen() && (victim == 0 || o.ID < victim) {
						victim = o.ID
					}
				}
				if victim != 0 {
					cres, _ := e.Cancel(1, e.all[victim].Account, victim)
					s.releases = append(s.releases, cres.Releases...)
				}
			}
		}
		return s
	}
	s1, s2 := build(), build()

	// Positions: identical set (both directions) and identical values.
	if len(s1.e.positions) != len(s2.e.positions) {
		t.Fatalf("market count diverged: %d vs %d", len(s1.e.positions), len(s2.e.positions))
	}
	for m, byAcct1 := range s1.e.positions {
		byAcct2 := s2.e.positions[m]
		if byAcct2 == nil || len(byAcct1) != len(byAcct2) {
			t.Fatalf("market %d position count diverged: %d vs %d", m, len(byAcct1), len(byAcct2))
		}
		for a, p := range byAcct1 {
			q, ok := byAcct2[a]
			if !ok {
				t.Fatalf("position %x missing in replay", a)
			}
			if q.Side != p.Side || q.Size.Cmp(p.Size) != 0 || q.EntryPrice != p.EntryPrice ||
				q.Margin.Cmp(p.Margin) != 0 || q.Leverage != p.Leverage {
				t.Fatalf("position %x diverged: %+v vs %+v", a, p, q)
			}
		}
	}

	if len(s1.trades) != len(s2.trades) {
		t.Fatalf("trade count diverged: %d vs %d", len(s1.trades), len(s2.trades))
	}
	for i := range s1.trades {
		a, b := s1.trades[i], s2.trades[i]
		if a.ID != b.ID || a.Market != b.Market || a.Price != b.Price ||
			a.Qty.Cmp(b.Qty) != 0 || a.Notional.Cmp(b.Notional) != 0 ||
			a.MakerOrder != b.MakerOrder || a.TakerOrder != b.TakerOrder ||
			a.Maker != b.Maker || a.Taker != b.Taker || a.MakerSide != b.MakerSide {
			t.Fatalf("trade %d diverged: %+v vs %+v", i, a, b)
		}
	}

	if len(s1.releases) != len(s2.releases) {
		t.Fatalf("release count diverged: %d vs %d", len(s1.releases), len(s2.releases))
	}
	for i := range s1.releases {
		a, b := s1.releases[i], s2.releases[i]
		if a.Account != b.Account || a.Amount.Cmp(b.Amount) != 0 {
			t.Fatalf("release %d diverged: %+v vs %+v", i, a, b)
		}
	}

	if len(s1.pnl) != len(s2.pnl) {
		t.Fatalf("pnl count diverged: %d vs %d", len(s1.pnl), len(s2.pnl))
	}
	for i := range s1.pnl {
		a, b := s1.pnl[i], s2.pnl[i]
		if a.Market != b.Market || a.TradeID != b.TradeID || a.Account != b.Account || a.Profit.Cmp(b.Profit) != 0 {
			t.Fatalf("pnl %d diverged: %+v vs %+v", i, a, b)
		}
	}
}

// TestEntryPriceWeightedAverage guards spec §4.6.2: EntryPrice is the
// quantity-weighted average floor(Σ(qty·price)/Σqty), within ±1 quote unit.
func TestEntryPriceWeightedAverage(t *testing.T) {
	m := Market{ID: 1, Symbol: "PRICE", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10}
	e, _ := NewEngine([]Market{m})

	restShort := func(a byte, price Price, qty uint64) {
		t.Helper()
		if _, err := e.Place(PlaceRequest{Market: 1, Account: acct(a), Side: SideShort, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: 10}); err != nil {
			t.Fatalf("rest short: %v", err)
		}
	}
	long := func(qty uint64) {
		t.Helper()
		if _, err := e.Place(PlaceRequest{Market: 1, Account: acct(1), Side: SideLong, Type: TypeLimit, TIF: TIFGTC, Price: 30, Qty: uint256.NewInt(qty), Leverage: 10}); err != nil {
			t.Fatalf("long: %v", err)
		}
	}

	// Resting shorts at 10/20/30 so the taker fills at three different prices.
	restShort(2, 10, 3)
	restShort(3, 20, 5)
	restShort(4, 30, 7)
	long(3) // fills 3 @ 10
	long(5) // fills 5 @ 20
	long(7) // fills 7 @ 30

	p, ok := e.Position(1, acct(1))
	if !ok {
		t.Fatal("no position")
	}
	// Σ(qty·price) = 3·10 + 5·20 + 7·30 = 340; Σqty = 15; floor(340/15) = 22.
	want := Price(340 / 15)
	if d := int64(p.EntryPrice) - int64(want); d < -1 || d > 1 {
		t.Fatalf("EntryPrice = %d, want %d (±1)", p.EntryPrice, want)
	}
}

// TestRestingReservedCoversRemaining guards spec §4.6.3: a resting order's
// reserved remainder always covers the floor margin of its remaining quantity.
func TestRestingReservedCoversRemaining(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 101, 10)) // freeze ceil(101/10)=11
	_, _ = e.Place(longReq(acct(2), 100, 50, 10))   // partially fills acct1 (50 of 101)
	_, _ = e.Place(longReq(acct(3), 90, 30, 10))    // rests fully (freeze ceil(27/10)=3)

	m := mkMarket()
	for _, o := range e.books[1].orders {
		need, err := m.marginFloor(o.Remaining(), o.Price, o.Leverage)
		if err != nil {
			t.Fatalf("marginFloor: %v", err)
		}
		if o.reserved == nil || o.reserved.Cmp(need) < 0 {
			t.Fatalf("order %d reserved %s < floor remaining %s", o.ID, o.reserved, need)
		}
	}
}

// TestTradeZeroSum guards spec §4.6.5: every trade moves Size between maker and
// taker in equal magnitude, so the total signed position size is conserved at
// zero.
func TestTradeZeroSum(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10)) // rests
	_, _ = e.Place(longReq(acct(2), 100, 100, 10))  // acct1 short100, acct2 long100
	_, _ = e.Place(longReq(acct(3), 100, 150, 10))  // acct3 rests long150
	_, _ = e.Place(shortReq(acct(2), 100, 150, 10)) // acct2 closes 100 long, flips 50 short

	var signed int64
	for _, byAcct := range e.positions {
		for _, p := range byAcct {
			s := int64(p.Size.Uint64())
			if p.Side == SideShort {
				s = -s
			}
			signed += s
		}
	}
	if signed != 0 {
		t.Fatalf("total signed position size = %d, want 0", signed)
	}
}

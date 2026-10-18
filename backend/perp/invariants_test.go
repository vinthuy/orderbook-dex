package perp

import (
	"math/rand"
	"testing"

	"github.com/holiman/uint256"
)

func TestMarginConservation(t *testing.T) {
	// 保证金全账恒等式（spec §4.6.4）：
	// Σ(订单级冻结) == Σ(所有 Releases) + Σ(在簿挂单 reserved 残额) + Σ(持仓 Margin)
	e, _ := NewEngine([]Market{mkMarket()})

	var totalFrozen, totalReleases uint64
	place := func(req PlaceRequest) Result {
		t.Helper()
		reqMargin, err := e.RequiredMargin(req)
		if err != nil {
			t.Fatalf("RequiredMargin: %v", err)
		}
		totalFrozen += reqMargin.Uint64()
		res, err := e.Place(req)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		for _, r := range res.Releases {
			totalReleases += r.Amount.Uint64()
		}
		return res
	}

	// Open and fully close a position, then leave one resting order.
	place(shortReq(acct(1), 100, 100, 10))
	place(longReq(acct(2), 100, 100, 10))
	// A resting order that does not cross leaves reserved margin.
	place(longReq(acct(3), 50, 100, 10))

	restingReserved := uint64(0)
	for _, o := range e.all {
		if o.reserved != nil {
			restingReserved += o.reserved.Uint64()
		}
	}
	posMargin := uint64(0)
	for _, byAcct := range e.positions {
		for _, p := range byAcct {
			posMargin += p.Margin.Uint64()
		}
	}

	if totalFrozen != totalReleases+restingReserved+posMargin {
		t.Fatalf("margin conservation: frozen=%d releases=%d resting=%d pos=%d",
			totalFrozen, totalReleases, restingReserved, posMargin)
	}
}

func TestDeterministicReplay(t *testing.T) {
	build := func() *Engine {
		e, _ := NewEngine([]Market{mkMarket()})
		rng := rand.New(rand.NewSource(42))
		for i := 0; i < 200; i++ {
			side := SideLong
			if rng.Intn(2) == 1 {
				side = SideShort
			}
			acct := acct(byte(1 + rng.Intn(5)))
			price := Price(90 + rng.Intn(21))
			qty := uint64(1 + rng.Intn(50))
			req := PlaceRequest{Market: 1, Account: acct, Side: side, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: 10}
			e.Place(req)
			if rng.Intn(4) == 0 {
				// occasionally cancel a resting order; pick the lowest open
				// OrderID so the choice is deterministic (map iteration order is
				// randomized and would make the replay diverge).
				var victim OrderID
				for _, o := range e.all {
					if o.IsOpen() && (victim == 0 || o.ID < victim) {
						victim = o.ID
					}
				}
				if victim != 0 {
					e.Cancel(1, e.all[victim].Account, victim)
				}
			}
		}
		return e
	}
	e1, e2 := build(), build()
	// Compare all positions.
	for m, byAcct := range e1.positions {
		for a, p := range byAcct {
			q, ok := e2.positions[m][a]
			if !ok {
				t.Fatalf("position %x missing in replay", a)
			}
			if q.Side != p.Side || q.Size.Cmp(p.Size) != 0 || q.EntryPrice != p.EntryPrice {
				t.Fatalf("position %x diverged: %+v vs %+v", a, p, q)
			}
		}
	}
}

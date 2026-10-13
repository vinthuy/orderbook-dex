package matching

import (
	"fmt"
	"testing"
)

func mkOrder(id OrderID, seq Seq, side Side, price Price, qty uint64) *Order {
	return &Order{ID: id, Seq: seq, Side: side, Price: price, Qty: amt(qty), Filled: amt(0), Status: StatusNew}
}

func TestSidePriceOrdering(t *testing.T) {
	m := testMarket()
	b := newBook(m)

	// Insert out of order; bids must end up descending, asks ascending.
	for i, p := range []Price{200, 100, 300, 250} {
		b.add(mkOrder(OrderID(i+1), Seq(i+1), SideBuy, p, 10))
		b.add(mkOrder(OrderID(100+i), Seq(100+i), SideSell, p, 10))
	}

	wantBids := []Price{300, 250, 200, 100}
	for i, want := range wantBids {
		if b.bids.prices[i] != want {
			t.Fatalf("bids[%d] = %d, want %d (full: %v)", i, b.bids.prices[i], want, b.bids.prices)
		}
	}
	wantAsks := []Price{100, 200, 250, 300}
	for i, want := range wantAsks {
		if b.asks.prices[i] != want {
			t.Fatalf("asks[%d] = %d, want %d (full: %v)", i, b.asks.prices[i], want, b.asks.prices)
		}
	}
}

func TestLevelFIFOAndTotal(t *testing.T) {
	m := testMarket()
	b := newBook(m)

	o1 := mkOrder(1, 1, SideSell, 250, 10)
	o2 := mkOrder(2, 2, SideSell, 250, 20)
	o3 := mkOrder(3, 3, SideSell, 250, 30)
	b.add(o1)
	b.add(o2)
	b.add(o3)

	lvl := b.asks.levels[250]
	if lvl == nil {
		t.Fatal("level 250 missing")
	}
	if got := lvl.total.Uint64(); got != 60 {
		t.Fatalf("level total = %d, want 60", got)
	}
	got := []OrderID{}
	for e := lvl.fifo.Front(); e != nil; e = e.Next() {
		got = append(got, e.Value.(*Order).ID)
	}
	want := []OrderID{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("fifo = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fifo = %v, want %v", got, want)
		}
	}
}

func TestCancelMaintainsTotalsAndDropsEmptyLevel(t *testing.T) {
	m := testMarket()
	b := newBook(m)

	o1 := mkOrder(1, 1, SideSell, 250, 10)
	o2 := mkOrder(2, 2, SideSell, 250, 20)
	o3 := mkOrder(3, 3, SideSell, 250, 30)
	b.add(o1)
	b.add(o2)
	b.add(o3)
	lvl := b.asks.levels[250]

	// Middle.
	b.cancel(o2)
	o2.Status = StatusCancelled
	if got := lvl.total.Uint64(); got != 40 {
		t.Fatalf("total after middle cancel = %d, want 40", got)
	}
	// Head.
	b.cancel(o1)
	o1.Status = StatusCancelled
	if got := lvl.total.Uint64(); got != 30 {
		t.Fatalf("total after head cancel = %d, want 30", got)
	}
	// Tail empties the level; both maps must drop it.
	b.cancel(o3)
	o3.Status = StatusCancelled
	if _, ok := b.asks.levels[250]; ok {
		t.Fatal("empty level still in levels map")
	}
	if len(b.asks.prices) != 0 {
		t.Fatalf("prices = %v, want empty", b.asks.prices)
	}
	if len(b.orders) != 0 {
		t.Fatalf("book.orders = %d, want 0", len(b.orders))
	}
}

func TestReduceMaintainsTotalAndDropsEmptyLevel(t *testing.T) {
	m := testMarket()
	b := newBook(m)

	o := mkOrder(1, 1, SideBuy, 250, 100)
	b.add(o)
	lvl := b.bids.levels[250]

	b.reduce(o, amt(40))
	o.Status = StatusPartiallyFilled
	if got := lvl.total.Uint64(); got != 60 {
		t.Fatalf("total after partial reduce = %d, want 60", got)
	}
	if got := o.Filled.Uint64(); got != 40 {
		t.Fatalf("Filled = %d, want 40", got)
	}
	if lvl.fifo.Len() != 1 {
		t.Fatalf("fifo len = %d, want 1", lvl.fifo.Len())
	}

	b.reduce(o, amt(60))
	o.Status = StatusFilled
	if _, ok := b.bids.levels[250]; ok {
		t.Fatal("fully-filled order left its empty level behind")
	}
	if len(b.bids.prices) != 0 {
		t.Fatalf("prices = %v, want empty", b.bids.prices)
	}
	if _, ok := b.orders[o.ID]; ok {
		t.Fatal("fully-filled order still registered in book.orders")
	}
}

// TestBookRandomConsistency fuzzes add/cancel/reduce and re-checks the price
// ordering, key-set agreement, FIFO Seq order and level totals after every
// operation.
func TestBookRandomConsistency(t *testing.T) {
	for _, seed := range []uint64{5, 17, 250} {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			m := testMarket()
			b := newBook(m)
			r := newLCG(seed)

			var resting []*Order
			var nextID OrderID
			var nextSeq Seq

			check := func() {
				checkSideInvariants(t, m, b.bids, "bids")
				checkSideInvariants(t, m, b.asks, "asks")
				if len(b.orders) != len(resting) {
					t.Fatalf("book.orders %d != tracked resting %d", len(b.orders), len(resting))
				}
			}

			for i := 0; i < 600; i++ {
				if len(resting) == 0 || r.intn(3) != 0 {
					nextID++
					nextSeq++
					o := mkOrder(nextID, nextSeq, Side(r.intn(2)), Price(100+r.intn(5)*10), uint64(1+r.intn(50)))
					b.add(o)
					resting = append(resting, o)
				} else {
					idx := r.intn(len(resting))
					o := resting[idx]
					if r.intn(2) == 0 {
						b.cancel(o)
						o.Status = StatusCancelled
						resting = append(resting[:idx], resting[idx+1:]...)
					} else {
						qty := uint64(1 + r.intn(int(o.Remaining().Uint64())))
						b.reduce(o, amt(qty))
						if o.Remaining().IsZero() {
							o.Status = StatusFilled
							resting = append(resting[:idx], resting[idx+1:]...)
						} else {
							o.Status = StatusPartiallyFilled
						}
					}
				}
				check()
			}
		})
	}
}

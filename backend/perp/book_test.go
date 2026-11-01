package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func mkOrder(t *testing.T, side Side, price Price, qty uint64) *Order {
	t.Helper()
	o := &Order{ID: OrderID(qty + uint64(price)*1000), Side: side, Price: price, Qty: uint256.NewInt(qty), Filled: uint256.NewInt(0), Status: StatusNew}
	return o
}

func TestBookAddBestOrder(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	b.add(mkOrder(t, SideLong, 10, 5))
	b.add(mkOrder(t, SideLong, 12, 3))
	if got := b.bids.best().price; got != 12 {
		t.Fatalf("best bid = %d, want 12", got)
	}
	if got := b.bids.bestOrder().Price; got != 12 {
		t.Fatalf("best order price = %d, want 12", got)
	}
}

func TestBookPriceTimePriority(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	o1 := mkOrder(t, SideLong, 10, 5)
	o2 := mkOrder(t, SideLong, 10, 7)
	b.add(o1)
	b.add(o2)
	if got := b.bids.bestOrder(); got != o1 {
		t.Fatalf("best order should be o1 (earlier), got seq %d", got.ID)
	}
}

func TestBookCancelRemovesLevel(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	o := mkOrder(t, SideShort, 10, 5)
	b.add(o)
	b.cancel(o)
	if b.asks.best() != nil {
		t.Fatal("asks should be empty after cancel")
	}
	if len(b.orders) != 0 {
		t.Fatal("orders map should be empty")
	}
}

func TestBookReduceFilledLeavesBook(t *testing.T) {
	b := newBook(Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, MaxLeverage: 10})
	o := mkOrder(t, SideLong, 10, 5)
	b.add(o)
	b.reduce(o, uint256.NewInt(5))
	if !o.Remaining().IsZero() {
		t.Fatal("order should be fully filled")
	}
	if b.bids.best() != nil {
		t.Fatal("level should be dropped after full fill")
	}
	if _, ok := b.orders[o.ID]; ok {
		t.Fatal("order should be removed from map")
	}
}

var _ = ledger.AssetID(1)

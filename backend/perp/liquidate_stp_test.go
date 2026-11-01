package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// TestLiquidateSelfTradeReleasesMargin pins that a liquidation that cancels the
// account's own crossing order also releases that order's frozen margin. Without
// it the order leaves the book while Holds keeps counting its freeze forever.
func TestLiquidateSelfTradeReleasesMargin(t *testing.T) {
	m := riskMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	victim := ledger.AccountID([20]byte{1})
	counter := ledger.AccountID([20]byte{2})

	// The victim goes long 1000 @ 100 against a resting short.
	if _, err := e.Place(PlaceRequest{Market: 1, Account: counter, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: victim, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}

	// The victim also rests a BID. That bid sits on the book the liquidation must
	// sell into, so the self-trade-prevention branch fires.
	bid, err := e.Place(PlaceRequest{Market: 1, Account: victim, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 99, Qty: uint256.NewInt(500), Leverage: 10})
	if err != nil {
		t.Fatalf("place own bid: %v", err)
	}
	if bid.Order.Status != StatusNew {
		t.Fatalf("own bid should rest, got %s", bid.Order.Status)
	}

	marginBefore := e.OpenMargins()
	total := func(mm map[ledger.HoldKey]*uint256.Int) *uint256.Int {
		sum := uint256.NewInt(0)
		for _, v := range mm {
			sum.Add(sum, v)
		}
		return sum
	}
	before := total(marginBefore)

	res, err := e.Liquidate(1, victim, nil)
	if err != nil {
		t.Fatalf("Liquidate: %v", err)
	}

	// The victim's own resting bid must be gone.
	if o, ok := e.Order(bid.Order.ID); ok && o.IsOpen() {
		t.Fatalf("own crossing bid %d should have been cancelled, status=%s", o.ID, o.Status)
	}

	// And its freeze must be released, not leaked.
	after := total(e.OpenMargins())
	if after.Cmp(before) >= 0 {
		t.Fatalf("margin not released: before=%s after=%s", before, after)
	}
	found := false
	for _, r := range res.Releases {
		if r.Account == victim && r.Amount != nil && !r.Amount.IsZero() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no release reported for the cancelled self order; releases=%+v", res.Releases)
	}
}

// TestLiquidateSelfTradeDoesNotPanic drives the same path a few times to make sure
// the cancel/release sequence cannot re-enter a removed price level.
func TestLiquidateSelfTradeDoesNotPanic(t *testing.T) {
	m := riskMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	victim := ledger.AccountID([20]byte{1})
	counter := ledger.AccountID([20]byte{2})

	if _, err := e.Place(PlaceRequest{Market: 1, Account: counter, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: victim, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	// Several own bids at DIFFERENT prices, each on its own level.
	for _, p := range []Price{99, 98, 97} {
		if _, err := e.Place(PlaceRequest{Market: 1, Account: victim, Side: SideLong,
			Type: TypeLimit, TIF: TIFGTC, Price: p, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := e.Liquidate(1, victim, nil); err != nil {
		t.Fatalf("Liquidate: %v", err)
	}
	// The empty-ask side means nothing fills; the loop must still terminate and
	// leave the book in a consistent state.
	if _, ok := e.Position(1, victim); !ok {
		t.Fatal("position should survive an unfillable liquidation")
	}
}

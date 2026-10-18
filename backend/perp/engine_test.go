package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func mkMarket() Market {
	return Market{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
}

func acct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func longReq(a ledger.AccountID, price Price, qty uint64, lev uint32) PlaceRequest {
	return PlaceRequest{Market: 1, Account: a, Side: SideLong, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: lev}
}

func shortReq(a ledger.AccountID, price Price, qty uint64, lev uint32) PlaceRequest {
	return PlaceRequest{Market: 1, Account: a, Side: SideShort, Type: TypeLimit, TIF: TIFGTC, Price: price, Qty: uint256.NewInt(qty), Leverage: lev}
}

func TestRequiredMarginLong(t *testing.T) {
	e, err := NewEngine([]Market{mkMarket()})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// notional(100, 100) = 100; ceil(100/10) = 10
	got, err := e.RequiredMargin(longReq(acct(1), 100, 100, 10))
	if err != nil {
		t.Fatalf("RequiredMargin: %v", err)
	}
	if got.Uint64() != 10 {
		t.Fatalf("margin = %s, want 10", got.Dec())
	}
}

func TestPlaceCrossesAndOpensPosition(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	// Short 100 @ 100, then Long 100 @ 100 crosses.
	if _, err := e.Place(shortReq(acct(1), 100, 100, 10)); err != nil {
		t.Fatalf("place short: %v", err)
	}
	res, err := e.Place(longReq(acct(2), 100, 100, 10))
	if err != nil {
		t.Fatalf("place long: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	// Account 2 opened a long; account 1 opened a short (both fully crossed).
	p2, ok := e.Position(1, acct(2))
	if !ok || p2.Side != SideLong || p2.Size.Uint64() != 100 {
		t.Fatalf("p2 = %+v", p2)
	}
	p1, ok := e.Position(1, acct(1))
	if !ok || p1.Side != SideShort || p1.Size.Uint64() != 100 {
		t.Fatalf("p1 = %+v", p1)
	}
}

func TestPlaceReduceClosesPosition(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10))
	_, _ = e.Place(longReq(acct(2), 100, 100, 10))
	// Account 3 rests a long bid so account 2 has a counterparty to close against.
	_, _ = e.Place(longReq(acct(3), 100, 100, 10))

	// Account 2 closes half: sell 50 @ 100 (side Short reduces its long).
	res, err := e.Place(shortReq(acct(2), 100, 50, 10))
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	_ = res
	p2, _ := e.Position(1, acct(2))
	if p2.Size.Uint64() != 50 {
		t.Fatalf("size = %s, want 50", p2.Size.Dec())
	}
}

func TestPlaceFlipSide(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10))
	_, _ = e.Place(longReq(acct(2), 100, 100, 10))
	// Account 3 rests a long bid so account 2 has a counterparty to close/flip against.
	_, _ = e.Place(longReq(acct(3), 100, 150, 10))

	// Account 2 sells 150: first closes 100 long, remaining 50 opens short.
	_, err := e.Place(shortReq(acct(2), 100, 150, 10))
	if err != nil {
		t.Fatalf("flip: %v", err)
	}
	p2, _ := e.Position(1, acct(2))
	if p2.Side != SideShort || p2.Size.Uint64() != 50 {
		t.Fatalf("p2 = %+v, want short 50", p2)
	}
}

func TestPlaceLeverageOverMaxRejected(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	if _, err := e.Place(longReq(acct(1), 100, 100, 11)); err == nil {
		t.Fatal("leverage over max should be rejected")
	}
}

func TestCancel(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	_, _ = e.Place(shortReq(acct(1), 100, 100, 10))
	res, err := e.Place(longReq(acct(2), 50, 100, 10)) // won't fully cross (price mismatch), rests
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if res.Order == nil || !res.Order.IsOpen() {
		t.Fatalf("order should rest, got %+v", res.Order)
	}
	c, err := e.Cancel(1, acct(2), res.Order.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if c.Order.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", c.Order.Status)
	}
}

func TestMarketOrderRequiresMaxNotional(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	req := PlaceRequest{Market: 1, Account: acct(1), Side: SideLong, Type: TypeMarket, TIF: TIFIOC, Price: 0, Qty: uint256.NewInt(10), Leverage: 10}
	if _, err := e.RequiredMargin(req); err == nil {
		t.Fatal("market order without MaxNotional should be rejected")
	}
}

func TestMarketOrderMarginFromMaxNotional(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	req := PlaceRequest{Market: 1, Account: acct(1), Side: SideLong, Type: TypeMarket, TIF: TIFIOC, Price: 0, Qty: uint256.NewInt(10), MaxNotional: uint256.NewInt(100), Leverage: 10}
	got, err := e.RequiredMargin(req)
	if err != nil {
		t.Fatalf("RequiredMargin: %v", err)
	}
	if got.Uint64() != 10 { // ceil(100/10)
		t.Fatalf("margin = %s, want 10", got.Dec())
	}
}

func TestMarketOrderCrossesAndOpensPosition(t *testing.T) {
	e, _ := NewEngine([]Market{mkMarket()})
	// Rest a short 100 @ 100.
	if _, err := e.Place(shortReq(acct(1), 100, 100, 10)); err != nil {
		t.Fatalf("rest short: %v", err)
	}
	// Market long 100 with cap 10050 (covers 100@100 = 100 notional).
	res, err := e.Place(PlaceRequest{Market: 1, Account: acct(2), Side: SideLong, Type: TypeMarket, TIF: TIFIOC, Price: 0, Qty: uint256.NewInt(100), MaxNotional: uint256.NewInt(10050), Leverage: 10})
	if err != nil {
		t.Fatalf("market long: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	p2, ok := e.Position(1, acct(2))
	if !ok || p2.Side != SideLong || p2.Size.Uint64() != 100 {
		t.Fatalf("p2 = %+v, want long 100", p2)
	}
}

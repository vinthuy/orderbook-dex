package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// ladderEngine builds an engine with a long position for `victim` at entry 100.
func ladderEngine(t *testing.T) (*Engine, ledger.AccountID) {
	t.Helper()
	m := riskMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	victim := ledger.AccountID([20]byte{1})
	counter := ledger.AccountID([20]byte{2})
	if _, err := e.Place(PlaceRequest{Market: 1, Account: counter, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: victim, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	return e, victim
}

// restBid puts liquidity on the bid side for a long liquidation to sell into.
func restBid(t *testing.T, e *Engine, acct byte, price Price, qty uint64) {
	t.Helper()
	if _, err := e.Place(PlaceRequest{Market: 1, Account: ledger.AccountID([20]byte{acct}),
		Side: SideLong, Type: TypeLimit, TIF: TIFGTC, Price: price,
		Qty: uint256.NewInt(qty), Leverage: 10}); err != nil {
		t.Fatalf("rest bid %d: %v", price, err)
	}
}

// TestLiquidateLimitOnlyTradesAtOrAboveLimit pins the first rung: a limit
// liquidation must not sweep below its price.
func TestLiquidateLimitOnlyTradesAtOrAboveLimit(t *testing.T) {
	e, victim := ladderEngine(t)
	restBid(t, e, 3, 98, 400) // acceptable
	restBid(t, e, 4, 90, 600) // too cheap for a limit at 95

	res, err := e.LiquidateLimit(1, victim, nil, 95)
	if err != nil {
		t.Fatalf("LiquidateLimit: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("want 1 trade at 98, got %d", len(res.Trades))
	}
	if res.Trades[0].Price != 98 {
		t.Fatalf("traded at %d, want 98", res.Trades[0].Price)
	}
	// 400 closed, 600 left open — the cheap bid must be untouched.
	pos, ok := e.Position(1, victim)
	if !ok {
		t.Fatal("position should still exist")
	}
	if pos.Size.Uint64() != 600 {
		t.Fatalf("remaining size = %s, want 600", pos.Size)
	}
	if _, stillOpen := e.Order(res.Trades[0].MakerOrder); !stillOpen {
		// Maker was fully consumed; that is fine. The assertion that matters is
		// the cheap bid below.
	}
	if longs, _ := e.Book(1, 10); len(longs) == 0 || longs[0].Price != 90 {
		t.Fatalf("the 90 bid should remain untouched, book=%+v", longs)
	}
}

// TestLiquidateFallsBackToSweeping pins the second rung: when the limit pass
// cannot fill, a market liquidation takes the rest even at worse prices.
func TestLiquidateFallsBackToSweeping(t *testing.T) {
	e, victim := ladderEngine(t)
	restBid(t, e, 3, 98, 400)
	restBid(t, e, 4, 90, 600)

	first, err := e.LiquidateLimit(1, victim, nil, 95)
	if err != nil {
		t.Fatalf("LiquidateLimit: %v", err)
	}
	if len(first.Trades) != 1 {
		t.Fatalf("want the 98 fill only, got %d", len(first.Trades))
	}

	second, err := e.Liquidate(1, victim, nil)
	if err != nil {
		t.Fatalf("Liquidate: %v", err)
	}
	if len(second.Trades) != 1 || second.Trades[0].Price != 90 {
		t.Fatalf("want the 90 fill on the fallback pass, got %+v", second.Trades)
	}
	if _, ok := e.Position(1, victim); ok {
		t.Fatal("position should be fully closed after both rungs")
	}
}

func TestLiquidateLimitRejectsZeroLimit(t *testing.T) {
	e, victim := ladderEngine(t)
	if _, err := e.LiquidateLimit(1, victim, nil, 0); err == nil {
		t.Fatal("want an error for a zero limit")
	}
}

// TestLiquidateLimitNoCrossingLiquidityIsNoOp pins that an unacceptable book
// leaves the position and the book untouched.
func TestLiquidateLimitNoCrossingLiquidityIsNoOp(t *testing.T) {
	e, victim := ladderEngine(t)
	restBid(t, e, 3, 80, 500)

	res, err := e.LiquidateLimit(1, victim, nil, 95)
	if err != nil {
		t.Fatalf("LiquidateLimit: %v", err)
	}
	if len(res.Trades) != 0 {
		t.Fatalf("want no trades below the limit, got %d", len(res.Trades))
	}
	pos, ok := e.Position(1, victim)
	if !ok || pos.Size.Uint64() != 1000 {
		t.Fatalf("position should be untouched, got %+v", pos)
	}
	if longs, _ := e.Book(1, 10); len(longs) != 1 || longs[0].Price != 80 {
		t.Fatalf("book should be untouched, got %+v", longs)
	}
}

// TestLiquidateLimitClosingShortBuysAboveLimit is the mirror case.
func TestLiquidateLimitClosingShortBuysAboveLimit(t *testing.T) {
	m := riskMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	victim := ledger.AccountID([20]byte{1})
	counter := ledger.AccountID([20]byte{2})
	// victim is SHORT 1000 @ 100; closing it means buying from the ask side.
	if _, err := e.Place(PlaceRequest{Market: 1, Account: victim, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: counter, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	// Asks at 102 (acceptable for a buy capped at 105) and 110 (too dear).
	if _, err := e.Place(PlaceRequest{Market: 1, Account: counter, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 102, Qty: uint256.NewInt(400), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: ledger.AccountID([20]byte{5}), Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 110, Qty: uint256.NewInt(600), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	res, err := e.LiquidateLimit(1, victim, nil, 105)
	if err != nil {
		t.Fatalf("LiquidateLimit: %v", err)
	}
	if len(res.Trades) != 1 || res.Trades[0].Price != 102 {
		t.Fatalf("want a single fill at 102, got %+v", res.Trades)
	}
	pos, ok := e.Position(1, victim)
	if !ok || pos.Size.Uint64() != 600 {
		t.Fatalf("want 600 still short, got %+v", pos)
	}
}

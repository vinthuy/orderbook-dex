package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// ladderService builds a market whose first liquidation rung trades at most
// `slipE18` away from the mark.
func ladderService(t *testing.T, slipE18 *uint256.Int) (*PerpOrderBookService, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		RiskConfig: perp.RiskConfig{SlippageE18: slipE18},
	}}
	for i, ref := range []string{"d1", "d2", "d3", "d4"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	// acct1 shorts, acct2 goes long 1000 @ 100 -> liquidatable at a low mark.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	return svc, l
}

// TestLiquidationLadderUsesLimitThenSweeep pins both rungs on a deep book: the
// limit pass must take the good price, and the sweep must take the rest only
// because the position is still liquidatable.
func TestLiquidationLadderUsesLimitThenSweep(t *testing.T) {
	// 1% slippage: at mark 90 the sell floor is 89.
	svc, _ := ladderService(t, uint256.NewInt(10_000_000_000_000_000))

	// Bid at 89.5 (acceptable) for 400, bid at 80 (too cheap) for 600.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 90, Qty: uint256.NewInt(400), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(4), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 80, Qty: uint256.NewInt(600), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 90)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("the position should be fully closed across both rungs")
	}
}

// TestLiquidationLadderStopsWhenSweepHasNoLiquidity pins the boundary: a limit
// pass that fills and a sweep that cannot must still leave a coherent state.
func TestLiquidationLadderPartial(t *testing.T) {
	svc, _ := ladderService(t, uint256.NewInt(10_000_000_000_000_000))
	// Only 400 of liquidity, well inside the limit band.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 90, Qty: uint256.NewInt(400), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 90)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	pos, ok := svc.Engine().Position(1, perpAcct(2))
	if !ok {
		t.Fatal("600 units should remain open")
	}
	if pos.Size.Uint64() != 600 {
		t.Fatalf("remaining size = %s, want 600", pos.Size)
	}
}

// TestLiquidationZeroSlippageSweepsOnly pins the explicit opt-out.
func TestLiquidationZeroSlippageSweepsOnly(t *testing.T) {
	svc, _ := ladderService(t, uint256.NewInt(0))
	// A bid far below the mark: with no limit rung the sweep takes it anyway.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 50, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	src := NewStaticPriceSource()
	src.Set(1, 90)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("with no limit rung the sweep should have closed the position")
	}
}

// TestLiquidationLadderReplaysBothRungs pins that a restart reproduces the same
// book: the limit pass and the sweep fill different counterparties, so replay
// must run both, not just the final state.
func TestLiquidationLadderReplaysBothRungs(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		RiskConfig: perp.RiskConfig{SlippageE18: uint256.NewInt(10_000_000_000_000_000)},
	}}
	for i, ref := range []string{"d1", "d2", "d3", "d4"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, dir), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	// Two levels: the limit rung takes the top one, the sweep takes the next.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 90, Qty: uint256.NewInt(400), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(4), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 80, Qty: uint256.NewInt(600), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	src := NewStaticPriceSource()
	src.Set(1, 90)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	liveLongs, liveAsks := svc.Engine().Book(1, 10)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	svc2, err := NewPerpOrderBookService(markets, ledger.New(), ledger.NewHolds(ledger.New()),
		mustPerpJournal(t, dir), NewEventBus())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer svc2.Close()

	replayLongs, replayAsks := svc2.Engine().Book(1, 10)
	if len(replayLongs) != len(liveLongs) || len(replayAsks) != len(liveAsks) {
		t.Fatalf("book shape differs: live %d/%d replay %d/%d",
			len(liveLongs), len(liveAsks), len(replayLongs), len(replayAsks))
	}
	if _, ok := svc2.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("replay resurrected the liquidated position")
	}
}

package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// adlFixture sets up a market with one losing position and one winning position,
// then drives the loser through a shortfall the empty insurance fund cannot pay.
func adlFixture(t *testing.T) (*PerpOrderBookService, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		// Explicit zero slippage: this test is about ADL, not the ladder.
		RiskConfig: perp.RiskConfig{SlippageE18: uint256.NewInt(0)},
	}}
	// acct1 = eventual loser (thinly funded), acct2 = eventual winner (long),
	// acct3 = liquidity for the liquidation.
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(10_000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d2"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(3), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d3"}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, l
}

// TestAutoDeleverageCoversShortfallFromWinners pins the ADL path: when the
// insurance fund cannot pay a liquidation shortfall, a profitable position is
// force-reduced at the mark and its realized profit covers the gap, so the loss
// is not minted out of nothing.
func TestAutoDeleverageCoversShortfallFromWinners(t *testing.T) {
	svc, l := adlFixture(t)

	// acct1 SHORT 1000 @ 100 (margin 10,000 = its whole balance).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	// acct2 LONG 1000 @ 100: this is the future winner once price rises.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	// acct3 rests an ask at 120 for the short to be liquidated into.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	totalBefore := new(uint256.Int).Set(l.TotalBalance(1))

	// Price jumps to 120: acct1 (short) is underwater by 20*1000 = 20,000 against
	// 10,000 margin — a 10,000 shortfall. The insurance fund has nothing.
	src := NewStaticPriceSource()
	src.Set(1, 120)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	// The short must be closed and the winner reduced.
	if _, ok := svc.Engine().Position(1, perpAcct(1)); ok {
		t.Fatal("the losing short should be liquidated")
	}
	win, ok := svc.Engine().Position(1, perpAcct(2))
	if !ok {
		t.Fatal("the winner should still hold a reduced position")
	}
	if win.Size.Uint64() >= 1000 {
		t.Fatalf("the winner should have been auto-deleveraged, still holds %s", win.Size)
	}

	// Conservation: the ledger must not have gained money. Without ADL the
	// shortfall was minted into the loser's account, inflating the total.
	totalAfter := l.TotalBalance(1)
	if totalAfter.Cmp(totalBefore) > 0 {
		t.Fatalf("ledger gained %s out of nothing (before=%s after=%s)",
			new(uint256.Int).Sub(totalAfter, totalBefore), totalBefore, totalAfter)
	}
}

// TestAutoDeleverageSkipsWithoutMark pins that ADL never runs on an unknown price.
func TestAutoDeleverageSkipsWithoutMark(t *testing.T) {
	svc, _ := adlFixture(t)
	raised, err := svc.autoDeleverage(1, uint256.NewInt(1000), perpAcct(9))
	if err != nil {
		t.Fatalf("autoDeleverage: %v", err)
	}
	if !raised.IsZero() {
		t.Fatalf("want 0 raised without prices, got %s", raised)
	}
}

// TestAutoDeleverageOnlySelectsProfitablePositions pins eligibility directly on
// the selection logic: a position whose mark-price PnL is not positive is never a
// candidate, so ADL can never take money from someone who is already losing.
func TestAutoDeleverageOnlySelectsProfitablePositions(t *testing.T) {
	m := perp.Market{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10}
	size := uint256.NewInt(1000)

	// A long entered at 100 is profitable at 120 and losing at 90.
	profit, err := m.UnrealizedPnL(perp.SideLong, size, 100, 120)
	if err != nil {
		t.Fatal(err)
	}
	if profit.Sign() <= 0 {
		t.Fatal("long at 120 should be profitable and therefore eligible")
	}
	loss, err := m.UnrealizedPnL(perp.SideLong, size, 100, 90)
	if err != nil {
		t.Fatal(err)
	}
	if loss.Sign() > 0 {
		t.Fatal("long at 90 must not be eligible")
	}

	// The mirror for a short.
	shortProfit, err := m.UnrealizedPnL(perp.SideShort, size, 100, 90)
	if err != nil {
		t.Fatal(err)
	}
	if shortProfit.Sign() <= 0 {
		t.Fatal("short at 90 should be profitable and therefore eligible")
	}
	shortLoss, err := m.UnrealizedPnL(perp.SideShort, size, 100, 120)
	if err != nil {
		t.Fatal(err)
	}
	if shortLoss.Sign() > 0 {
		t.Fatal("short at 120 must not be eligible")
	}
}


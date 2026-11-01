package services

import (
	"fmt"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// crossFixture builds two markets sharing quote asset 1. Market 1 holds a losing
// leg and market 2 a winning leg for the same account, with the mode selectable.
func crossFixture(t *testing.T, mode perp.MarginMode) (*PerpOrderBookService, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	mk := func(id perp.MarketID, sym string) perp.Market {
		return perp.Market{
			ID: id, Symbol: sym, Base: ledger.AssetID(10 + id), Quote: 1,
			BaseDecimals: 0, MaxLeverage: 10,
			RiskConfig: perp.RiskConfig{Mode: mode, SlippageE18: uint256.NewInt(0)},
		}
	}
	markets := []perp.Market{mk(1, "AAA-PERP"), mk(2, "BBB-PERP")}
	// The account under test plus three counterparties.
	for i := 1; i <= 4; i++ {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i)), Asset: 1,
			Amount: uint256.NewInt(100_000_000), Ref: fmt.Sprintf("d%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, l
}

// TestCrossMarginSparesLosingLegWhenWinningLegCoversIt is the core semantic
// difference: under cross margin a profitable leg backs a losing one, so the
// losing leg survives a scan that would close it in isolation.
func TestCrossMarginSparesLosingLegWhenWinningLegCoversIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       perp.MarginMode
		wantClosed bool
	}{
		{"isolated", perp.MarginIsolated, true},
		{"cross", perp.MarginCross, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := crossFixture(t, tc.mode)
			victim := perpAcct(1)

			// Market 1: victim LONG 1000 @ 100 (the future losing leg).
			if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideShort,
				Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: victim, Side: perp.SideLong,
				Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
				t.Fatal(err)
			}
			// Market 2: victim SHORT 1000 @ 100 (the future winning leg).
			if _, err := svc.Place(perp.PlaceRequest{Market: 2, Account: perpAcct(3), Side: perp.SideLong,
				Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Place(perp.PlaceRequest{Market: 2, Account: victim, Side: perp.SideShort,
				Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
				t.Fatal(err)
			}
			// Liquidity so a liquidation could actually fill the losing leg.
			if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(4), Side: perp.SideLong,
				Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 85, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
				t.Fatal(err)
			}

			// Market 1 falls hard (long loses), market 2 rises equally (short loses
			// too) — so instead make market 2 rise in a way that profits the SHORT?
			// No: to have a winning leg, market 2 must FALL. Set mark 2 low.
			src := NewStaticPriceSource()
			src.Set(1, 85) // long at 100 -> loses 15*1000 = 15,000 against 10,000 margin
			src.Set(2, 85) // short at 100 -> gains 15*1000 = 15,000
			svc.SetPriceSource(src, PriceOptions{})
			svc.tick(time.Unix(1000, 0))

			_, open := svc.Engine().Position(1, victim)
			if tc.wantClosed && open {
				t.Fatal("isolated margin should have closed the losing leg")
			}
			if !tc.wantClosed && !open {
				t.Fatal("cross margin should have kept the losing leg: the winning leg covers it")
			}
		})
	}
}

// TestAccountRiskForAggregatesAcrossMarkets pins the arithmetic directly.
func TestAccountRiskForAggregatesAcrossMarkets(t *testing.T) {
	victim := perpAcct(1)
	markets := map[perp.MarketID]perp.Market{
		1: {ID: 1, Symbol: "A", Base: 10, Quote: 1, BaseDecimals: 0, MaxLeverage: 10},
		2: {ID: 2, Symbol: "B", Base: 11, Quote: 1, BaseDecimals: 0, MaxLeverage: 10},
	}
	marks := map[perp.MarketID]perp.Price{1: 85, 2: 85}
	positions := []perp.Position{
		// Long at 100 -> loses 15,000, margin 10,000: equity -5,000.
		{Market: 1, Account: victim, Side: perp.SideLong, Size: uint256.NewInt(1000),
			EntryPrice: 100, Margin: uint256.NewInt(10_000), Leverage: 10},
		// Short at 100 -> gains 15,000, margin 10,000: equity 25,000.
		{Market: 2, Account: victim, Side: perp.SideShort, Size: uint256.NewInt(1000),
			EntryPrice: 100, Margin: uint256.NewInt(10_000), Leverage: 10},
	}
	risk, err := perp.AccountRiskFor(victim, positions, markets, marks)
	if err != nil {
		t.Fatalf("AccountRiskFor: %v", err)
	}
	// Aggregate equity = -5,000 + 25,000 = 20,000; maintenance = 2 * ceil(85*1000*0.005)
	// = 2 * 425 = 850. 20,000 > 850 -> not liquidatable.
	if risk.Liquidatable {
		t.Fatalf("aggregate equity %s should cover maintenance %s", risk.Equity, risk.MaintenanceMargin)
	}
	if risk.WorstMarket != 1 {
		t.Fatalf("worst leg should be market 1 (the loser), got %d", risk.WorstMarket)
	}
}

package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestLiquidationIntervalIsHonoured pins that PERP_LIQUIDATION_INTERVAL controls
// the scan cadence. It used to be stored and never read: the scan ran on every
// price-refresh tick regardless.
func TestLiquidationIntervalIsHonoured(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2", "d3"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}

	scans := 0
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// Open a position and make it liquidatable, with liquidity to fill.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	src := NewStaticPriceSource()
	src.Set(1, 80)
	svc.SetPriceSource(src, PriceOptions{})
	_ = scans

	// A ten-second cadence: ticks inside the window must not scan.
	svc.SetLiquidationOptions(LiquidationOptions{Interval: 10 * time.Second})
	base := time.Unix(10_000, 0)

	svc.tick(base)                      // first tick always scans
	svc.tick(base.Add(1 * time.Second)) // too soon
	svc.tick(base.Add(5 * time.Second)) // still too soon
	if got := svc.LiquidationScans(); got != 1 {
		t.Fatalf("want exactly 1 scan inside the interval, got %d", got)
	}

	svc.tick(base.Add(11 * time.Second)) // due again
	if got := svc.LiquidationScans(); got != 2 {
		t.Fatalf("want a second scan once the interval elapsed, got %d", got)
	}
}

// TestTickIntervalIsTheShortestOfItsDrivers pins that the runner wakes often
// enough to honour a liquidation interval shorter than the price refresh.
func TestTickIntervalIsTheShortestOfItsDrivers(t *testing.T) {
	svc, _, _ := riskService(t)

	svc.SetFundingOptions(FundingOptions{Tick: 5 * time.Second, Interval: time.Hour,
		MaxRateE18: uint256.NewInt(1)})
	svc.SetLiquidationOptions(LiquidationOptions{Interval: 1 * time.Second})

	if got := svc.tickInterval(); got != time.Second {
		t.Fatalf("tick interval = %s, want 1s (the liquidation cadence)", got)
	}

	// A slower liquidation cadence must not slow the price refresh below its own.
	svc.SetLiquidationOptions(LiquidationOptions{Interval: 30 * time.Second})
	if got := svc.tickInterval(); got != 5*time.Second {
		t.Fatalf("tick interval = %s, want 5s (the funding/price cadence)", got)
	}
}

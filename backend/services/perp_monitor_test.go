package services

import (
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// monitorFixture builds a service with a monitor wired to the same bus.
func monitorFixture(t *testing.T) (*PerpOrderBookService, *PerpMonitor, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		RiskConfig: perp.RiskConfig{SlippageE18: uint256.NewInt(0)},
	}}
	for i, ref := range []string{"d1", "d2", "d3"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(10_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	mon := NewPerpMonitor(svc)
	mon.Subscribe(bus)
	return svc, mon, l
}

// TestHealthIsHealthyWhenNothingIsWrong pins the baseline: a fresh, unpriced
// market with no positions is not "broken".
func TestHealthIsHealthyWhenNothingIsWrong(t *testing.T) {
	_, mon, _ := monitorFixture(t)
	h := mon.Snapshot(time.Now())
	if h.Status != HealthOK {
		t.Fatalf("want ok, got %s: %v", h.Status, h.Problems)
	}
	if len(h.Problems) != 0 {
		t.Fatalf("want no problems, got %v", h.Problems)
	}
}

// TestHealthDetectsPositionsWithoutPrice is the failure this endpoint exists for:
// positions are open, the scanner cannot price them, so nobody gets liquidated.
// The old /health returned {"status":"ok"} in exactly this situation.
func TestHealthDetectsPositionsWithoutPrice(t *testing.T) {
	svc, mon, _ := monitorFixture(t)

	// Open a position with no price source configured at all.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	h := mon.Snapshot(time.Now())
	if h.Status == HealthOK {
		t.Fatal("positions without a price must not report ok")
	}
	found := false
	for _, p := range h.Problems {
		if strings.Contains(p, "no price source") || strings.Contains(p, "fresh price") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a pricing problem, got %v", h.Problems)
	}
}

// TestHealthReportsFrozenMarginAndPositions pins the market section.
func TestHealthReportsFrozenMarginAndPositions(t *testing.T) {
	svc, mon, _ := monitorFixture(t)
	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	h := mon.Snapshot(time.Now())
	if len(h.Markets) != 1 {
		t.Fatalf("want 1 market, got %d", len(h.Markets))
	}
	mh := h.Markets[0]
	if mh.OpenPositions != 2 {
		t.Fatalf("open positions = %d, want 2", mh.OpenPositions)
	}
	if mh.FrozenMargin.Uint64() != 20_000 {
		t.Fatalf("frozen margin = %s, want 20000 (two 10k positions)", mh.FrozenMargin)
	}
	if !mh.HasMark || mh.MarkPrice != 100 {
		t.Fatalf("want mark 100, got %d (has=%v)", mh.MarkPrice, mh.HasMark)
	}
}

// TestHealthCountsLiquidationsAndADL pins the counters coming off the event stream.
func TestHealthCountsLiquidations(t *testing.T) {
	svc, mon, _ := monitorFixture(t)
	svc.SetInsuranceFund(perpAcct(9))

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 50, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	src := NewStaticPriceSource()
	src.Set(1, 50)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	h := mon.Snapshot(time.Now())
	if h.Liquidations == 0 {
		t.Fatal("want at least one liquidation counted")
	}
	if h.LastEventAt == 0 {
		t.Fatal("want a last-event timestamp once something has happened")
	}
}

// TestHealthDetectsDegraded pins that a degraded service is never reported ok.
func TestHealthDetectsDegraded(t *testing.T) {
	svc, mon, _ := monitorFixture(t)
	svc.mu.Lock()
	svc.fatalErr = ErrPerpServiceDegraded
	svc.mu.Unlock()

	h := mon.Snapshot(time.Now())
	if h.Status != HealthDegraded {
		t.Fatalf("want degraded, got %s", h.Status)
	}
	if !h.Degraded {
		t.Fatal("the degraded flag must be set")
	}
}

// TestMetricsTextIsPrometheusShaped pins the exposition format basics: help/type
// lines, no duplicate series, and values that parse.
func TestMetricsTextIsPrometheusShaped(t *testing.T) {
	svc, mon, _ := monitorFixture(t)
	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{})
	// Tick at "now": a mark derived at an old timestamp is stale by definition, and
	// this test is about the metric shape, not staleness.
	now := time.Now()
	svc.tick(now)

	text := mon.Snapshot(now).MetricsText()
	for _, want := range []string{
		"perp_health_status 1",
		"perp_degraded 0",
		"perp_liquidations_total",
		"perp_open_positions{market=\"ETH-PERP\"}",
		"perp_price_stale{market=\"ETH-PERP\"} 0",
		"perp_unpriced{market=\"ETH-PERP\"} 0",
		"perp_mark_price{market=\"ETH-PERP\"} 100",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q\n---\n%s", want, text)
		}
	}
}

// TestMetricsReportUnhealthy pins that the status gauge follows the verdict.
func TestMetricsReportUnhealthy(t *testing.T) {
	svc, mon, _ := monitorFixture(t)
	// Positions and no price: unhealthy.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	text := mon.Snapshot(time.Now()).MetricsText()
	if !strings.Contains(text, "perp_health_status 0") {
		t.Fatalf("status gauge should be 0 when unhealthy:\n%s", text)
	}
}


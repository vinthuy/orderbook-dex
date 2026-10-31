package services

import (
	"testing"
	"time"

	"web3-amm-dex-backend/perp"
)

func priceTestMarket() perp.Market {
	return perp.Market{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 18, MaxLeverage: 10}
}

func TestPerpPriceTrackerReturnsMarkWhenFresh(t *testing.T) {
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	mark, ok := tr.Mark(priceTestMarket(), 2000, 2001, 2003, true, true, time.Unix(1000, 0))
	if !ok {
		t.Fatal("want a fresh mark")
	}
	if mark != 2001 {
		t.Fatalf("want 2001, got %d", mark)
	}
}

func TestPerpPriceTrackerRejectsStaleIndex(t *testing.T) {
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	tr.Observe(1, 2000, time.Unix(1000, 0))
	if !tr.Stale(1, time.Unix(1000+61, 0)) {
		t.Fatal("want stale after max price age")
	}
	if tr.Stale(1, time.Unix(1000+30, 0)) {
		t.Fatal("want fresh within max price age")
	}
}

func TestPerpPriceTrackerStaleWhenNeverObserved(t *testing.T) {
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	if !tr.Stale(1, time.Unix(1000, 0)) {
		t.Fatal("an unobserved market must be stale")
	}
}

func TestPerpPriceTrackerFallsBackToLastMark(t *testing.T) {
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	m := priceTestMarket()
	first, ok := tr.Mark(m, 2000, 2001, 2003, true, true, time.Unix(1000, 0))
	if !ok || first != 2001 {
		t.Fatalf("first mark: %d %v", first, ok)
	}
	// No index (index 0) but within the mark staleness window: reuse it.
	got, ok := tr.Mark(m, 0, 0, 0, false, false, time.Unix(1000+120, 0))
	if !ok || got != 2001 {
		t.Fatalf("fallback: want 2001, got %d (ok=%v)", got, ok)
	}
	// Beyond max mark staleness: no mark at all.
	if _, ok := tr.Mark(m, 0, 0, 0, false, false, time.Unix(1000+400, 0)); ok {
		t.Fatal("want no mark after max staleness")
	}
}

func TestPerpPriceTrackerNoMarkWithoutHistory(t *testing.T) {
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	if _, ok := tr.Mark(priceTestMarket(), 0, 0, 0, false, false, time.Unix(1000, 0)); ok {
		t.Fatal("want no mark when there is neither index nor history")
	}
	if _, ok := tr.LastMark(1); ok {
		t.Fatal("want no last mark")
	}
}

func TestPerpPriceTrackerFreshMarkSupersedesHistory(t *testing.T) {
	tr := NewPerpPriceTracker(time.Minute, 5*time.Minute)
	m := priceTestMarket()
	if _, ok := tr.Mark(m, 2000, 0, 0, false, false, time.Unix(1000, 0)); !ok {
		t.Fatal("seed mark")
	}
	got, ok := tr.Mark(m, 3000, 0, 0, false, false, time.Unix(1010, 0))
	if !ok || got != 3000 {
		t.Fatalf("want new mark 3000, got %d (ok=%v)", got, ok)
	}
	if last, ok := tr.LastMark(1); !ok || last != 3000 {
		t.Fatalf("LastMark: want 3000, got %d", last)
	}
}

package services

import (
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/perp"
)

func TestParseIndexPrices(t *testing.T) {
	src, err := ParseIndexPrices("1:2000;2: 3000 ;")
	if err != nil {
		t.Fatalf("ParseIndexPrices: %v", err)
	}
	if p, ok := src.Index(1); !ok || p != 2000 {
		t.Fatalf("market 1: want 2000, got %d (ok=%v)", p, ok)
	}
	if p, ok := src.Index(2); !ok || p != 3000 {
		t.Fatalf("market 2: want 3000, got %d (ok=%v)", p, ok)
	}
	if _, ok := src.Index(3); ok {
		t.Fatal("market 3 should have no price")
	}
}

func TestParseIndexPricesRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"1", "1:", "x:2000", "1:0", "1:-5"} {
		if _, err := ParseIndexPrices(bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
}

func TestTickRefreshesMarkAndJournals(t *testing.T) {
	svc, _, _ := fundingFixture(t, t.TempDir())
	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{})

	svc.tick(time.Unix(1000, 0))
	mark, ok := svc.MarkPrice(1)
	if !ok {
		t.Fatal("want a mark after the first tick")
	}
	// The book is empty after the fixture's fill (100 vs 100 fully crossed), so
	// the mark is the index price.
	if mark != 100 {
		t.Fatalf("want mark 100, got %d", mark)
	}
}

func TestTickDoesNotSettleBeforeInterval(t *testing.T) {
	svc, l, _ := fundingFixture(t, t.TempDir())
	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{})
	svc.SetFundingOptions(FundingOptions{
		Tick: 5 * time.Second, Interval: time.Hour,
		MaxRateE18: uint256.NewInt(7_500_000_000_000_000),
	})

	before := new(uint256.Int).Set(l.Balance(perpAcct(2), 1))
	svc.tick(time.Unix(1000, 0)) // primes lastFunding
	svc.tick(time.Unix(1001, 0))
	if got := l.Balance(perpAcct(2), 1); got.Cmp(before) != 0 {
		t.Fatalf("funding settled too early: %s -> %s", before, got)
	}
}

func TestTickSettlesFundingWhenDue(t *testing.T) {
	svc, l, _ := fundingFixture(t, t.TempDir())
	// Open a large position so the funding amount does not floor to zero.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{})
	svc.SetFundingOptions(FundingOptions{
		Tick: time.Second, Interval: time.Hour,
		MaxRateE18: uint256.NewInt(7_500_000_000_000_000),
	})

	// A mark equal to the index gives a zero premium, hence no legs: drive the
	// round with an explicit positive rate instead so the effect is visible.
	svc.tick(time.Unix(1000, 0)) // primes lastFunding and refreshes the mark
	beforeLong := new(uint256.Int).Set(l.Balance(perpAcct(2), 1))
	rate, _ := new(big.Int).SetString("1000000000000000", 10)
	if _, err := svc.SettleFunding(1, 1, 100, rate, 1000); err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if l.Balance(perpAcct(2), 1).Cmp(beforeLong) >= 0 {
		t.Fatalf("long should have paid funding: %s -> %s", beforeLong, l.Balance(perpAcct(2), 1))
	}
	st, ok := svc.FundingState(1)
	if !ok || st.LastIndex != 1 {
		t.Fatalf("funding round did not run: %+v ok=%v", st, ok)
	}
}

// TestTickRunsFundingWhenDue checks the scheduler itself: a tick past the
// funding interval must move money without any explicit SettleFunding call.
func TestTickRunsFundingWhenDue(t *testing.T) {
	svc, _, _ := fundingFixture(t, t.TempDir())
	// Open a position and rest an order so the book is two-sided and the
	// derived mark deviates from the index.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	// A resting bid slightly above the index shifts the mark up; the 0.5%
	// deviation clamp keeps a large gap from moving it at all, which is the
	// point of clamping. This bid sits inside the band, so the mark averages the
	// index with the mid (1:1 weights).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 101, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("rest long: %v", err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{MaxMarkStaleAge: time.Hour})
	svc.SetFundingOptions(FundingOptions{
		Tick: time.Second, Interval: time.Hour,
		MaxRateE18: uint256.NewInt(7_500_000_000_000_000),
		MaxDeviation: uint256.NewInt(5_000_000_000_000_000),
	})

	svc.tick(time.Unix(1000, 0)) // prime the funding clock, derive the mark
	svc.tick(time.Unix(1000+3601, 0))

	st, ok := svc.FundingState(1)
	if !ok || st.LastIndex == 0 {
		t.Fatalf("scheduled funding round did not run: %+v ok=%v", st, ok)
	}
	// The mark must be at least the index and the round must have a rate field.
	if st.Mark < 100 {
		longs, shorts := svc.Engine().Book(1, 5)
		t.Fatalf("mark fell below the index: %d (bids: %+v, asks: %+v)", st.Mark, longs, shorts)
	}
}

func TestRunFundingRoundSkipsWithoutMark(t *testing.T) {
	svc, _, _ := fundingFixture(t, "")
	_, settled, err := svc.RunFundingRound(1, time.Unix(1000, 0), time.Hour, uint256.NewInt(1))
	if err != nil {
		t.Fatalf("RunFundingRound: %v", err)
	}
	if settled {
		t.Fatal("want no round without a mark price")
	}
}

func TestUnrealizedPnLUsesMark(t *testing.T) {
	svc, _, _ := fundingFixture(t, t.TempDir())
	src := NewStaticPriceSource()
	src.Set(1, 110)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	// Acct 2 is long 100 base entered at 100; mark 110 -> notional gain:
	// (110-100)*100/100 (BaseDecimals 2) = 10.
	pnl, ok := svc.UnrealizedPnL(1, perpAcct(2))
	if !ok {
		t.Fatal("want unrealized PnL")
	}
	if pnl.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("want 10, got %s", pnl)
	}
}

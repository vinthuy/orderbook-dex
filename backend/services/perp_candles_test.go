package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/perp"
)

func trade(price perp.Price, qty uint64) perp.Trade {
	return perp.Trade{ID: 1, Market: 1, Price: price, Qty: uint256.NewInt(qty)}
}

func TestCandleStoreAggregatesOHLCV(t *testing.T) {
	c := NewCandleStore()
	base := time.Unix(1_700_000_000, 0)

	c.Record(trade(100, 10), base)
	c.Record(trade(105, 20), base.Add(10*time.Second))
	c.Record(trade(95, 30), base.Add(20*time.Second))
	c.Record(trade(102, 40), base.Add(30*time.Second))

	series := c.Series(1, Interval1m, 0)
	if len(series) != 1 {
		t.Fatalf("want 1 bar, got %d", len(series))
	}
	b := series[0]
	if b.Open != 100 || b.High != 105 || b.Low != 95 || b.Close != 102 {
		t.Fatalf("OHLC = %d/%d/%d/%d, want 100/105/95/102", b.Open, b.High, b.Low, b.Close)
	}
	if b.Volume.Uint64() != 100 {
		t.Fatalf("volume = %s, want 100", b.Volume)
	}
	if b.TradeCount != 4 {
		t.Fatalf("trade count = %d, want 4", b.TradeCount)
	}
}

func TestCandleStoreSplitsOnBucketBoundary(t *testing.T) {
	c := NewCandleStore()
	// 1m buckets: 1_700_000_040 is in the bucket starting at 1_700_000_040 - 40.
	c.Record(trade(100, 1), time.Unix(1_700_000_010, 0))
	c.Record(trade(200, 1), time.Unix(1_700_000_070, 0))

	series := c.Series(1, Interval1m, 0)
	if len(series) != 2 {
		t.Fatalf("want 2 bars, got %d", len(series))
	}
	if series[0].Close != 100 || series[1].Open != 200 {
		t.Fatalf("bars not split correctly: %+v", series)
	}
	if series[1].OpenTime-series[0].OpenTime != 60 {
		t.Fatalf("bars should be one minute apart, got %d", series[1].OpenTime-series[0].OpenTime)
	}
}

func TestCandleStoreAllIntervalsAgree(t *testing.T) {
	c := NewCandleStore()
	base := time.Unix(1_700_000_000, 0)
	c.Record(trade(100, 5), base)
	c.Record(trade(110, 5), base.Add(30*time.Second))

	for _, iv := range []Interval{Interval1m, Interval5m, Interval15m, Interval1h, Interval4h, Interval1d} {
		series := c.Series(1, iv, 0)
		if len(series) != 1 {
			t.Fatalf("%s: want 1 bar, got %d", iv, len(series))
		}
		if series[0].High != 110 || series[0].Volume.Uint64() != 10 {
			t.Fatalf("%s: bad aggregate %+v", iv, series[0])
		}
		// Bucket start must be aligned to the interval.
		step := int64(time.Duration(iv) / time.Second)
		if series[0].OpenTime%step != 0 {
			t.Fatalf("%s: open time %d is not aligned to %d", iv, series[0].OpenTime, step)
		}
	}
}

func TestCandleStoreLimitReturnsNewest(t *testing.T) {
	c := NewCandleStore()
	for i := 0; i < 5; i++ {
		c.Record(trade(perp.Price(100+i), 1), time.Unix(1_700_000_000+int64(i)*60, 0))
	}
	series := c.Series(1, Interval1m, 2)
	if len(series) != 2 {
		t.Fatalf("want 2 bars, got %d", len(series))
	}
	if series[0].Close != 103 || series[1].Close != 104 {
		t.Fatalf("limit should return the newest bars, got %d then %d", series[0].Close, series[1].Close)
	}
}

func TestCandleStoreIgnoresZeroQty(t *testing.T) {
	c := NewCandleStore()
	c.Record(trade(100, 0), time.Unix(1_700_000_000, 0))
	if got := c.Series(1, Interval1m, 0); len(got) != 0 {
		t.Fatalf("a zero-qty trade must not create a bar, got %+v", got)
	}
}

// TestCandleStoreHandlesOutOfOrderTrades pins that a late trade inside the current
// bar folds in rather than creating a duplicate bucket.
func TestCandleStoreHandlesOutOfOrderTrades(t *testing.T) {
	c := NewCandleStore()
	c.Record(trade(100, 10), time.Unix(1_700_000_000, 0))
	c.Record(trade(105, 10), time.Unix(1_700_000_030, 0))
	// A trade from earlier in the same minute arrives late.
	c.Record(trade(90, 10), time.Unix(1_700_000_010, 0))

	series := c.Series(1, Interval1m, 0)
	if len(series) != 1 {
		t.Fatalf("out-of-order trade must not create a second bar, got %d", len(series))
	}
	if series[0].Low != 90 {
		t.Fatalf("low = %d, want 90 after the late trade", series[0].Low)
	}
	if series[0].Volume.Uint64() != 30 {
		t.Fatalf("volume = %s, want 30", series[0].Volume)
	}
}

// TestCandleStoreReturnsCopies pins that a caller cannot mutate store state.
func TestCandleStoreReturnsCopies(t *testing.T) {
	c := NewCandleStore()
	c.Record(trade(100, 10), time.Unix(1_700_000_000, 0))
	got := c.Series(1, Interval1m, 0)
	got[0].Volume.SetUint64(999)
	if again := c.Series(1, Interval1m, 0); again[0].Volume.Uint64() != 10 {
		t.Fatalf("store leaked internal state: volume %s", again[0].Volume)
	}
}

func TestCandleStoreCapsRetention(t *testing.T) {
	c := NewCandleStore()
	for i := 0; i < maxCandlesPerMarket+50; i++ {
		c.Record(trade(perp.Price(100+i%10), 1), time.Unix(int64(1_700_000_000+i*60), 0))
	}
	series := c.Series(1, Interval1m, 0)
	if len(series) != maxCandlesPerMarket {
		t.Fatalf("want %d bars retained, got %d", maxCandlesPerMarket, len(series))
	}
}

func TestParseInterval(t *testing.T) {
	for in, want := range map[string]Interval{
		"1m": Interval1m, "5m": Interval5m, "15m": Interval15m,
		"1h": Interval1h, "4h": Interval4h, "1d": Interval1d,
	} {
		got, err := ParseInterval(in)
		if err != nil || got != want {
			t.Fatalf("ParseInterval(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseInterval("3m"); err == nil {
		t.Fatal("want an error for an unsupported interval")
	}
}

package services

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/perp"
)

// Interval is a candle period. Only fixed, whole-second periods are supported:
// anything else would make bucketing depend on the caller's clock.
type Interval time.Duration

const (
	Interval1m  Interval = Interval(time.Minute)
	Interval5m  Interval = Interval(5 * time.Minute)
	Interval15m Interval = Interval(15 * time.Minute)
	Interval1h  Interval = Interval(time.Hour)
	Interval4h  Interval = Interval(4 * time.Hour)
	Interval1d  Interval = Interval(24 * time.Hour)
)

// String returns the compact name used in the API ("1m", "1h", ...).
func (i Interval) String() string {
	switch i {
	case Interval1m:
		return "1m"
	case Interval5m:
		return "5m"
	case Interval15m:
		return "15m"
	case Interval1h:
		return "1h"
	case Interval4h:
		return "4h"
	case Interval1d:
		return "1d"
	default:
		return time.Duration(i).String()
	}
}

// ParseInterval maps an API string onto an Interval.
func ParseInterval(s string) (Interval, error) {
	switch s {
	case "1m":
		return Interval1m, nil
	case "5m":
		return Interval5m, nil
	case "15m":
		return Interval15m, nil
	case "1h":
		return Interval1h, nil
	case "4h":
		return Interval4h, nil
	case "1d":
		return Interval1d, nil
	default:
		return 0, errors.New("services: unknown candle interval (want 1m, 5m, 15m, 1h, 4h or 1d)")
	}
}

// Candle is one OHLCV bar. Prices and volumes are strings so the JSON layer does
// not lose precision on large quantities.
type Candle struct {
	Market     perp.MarketID
	Interval   Interval
	OpenTime   int64 // unix seconds, aligned to the interval
	Open       perp.Price
	High       perp.Price
	Low        perp.Price
	Close      perp.Price
	Volume     *uint256.Int // base units traded in the bar
	TradeCount int
}

// bucketStart aligns t down to the start of its interval.
//
// Alignment is computed on unix seconds so a bar boundary is the same for every
// observer regardless of local timezone.
func bucketStart(t time.Time, iv Interval) int64 {
	step := int64(time.Duration(iv) / time.Second)
	if step <= 0 {
		return t.Unix()
	}
	u := t.Unix()
	if u >= 0 {
		return u - u%step
	}
	// For pre-epoch times, floor rather than truncate so buckets stay monotonic.
	return u - ((u % step) + step) % step
}

// maxCandlesPerMarket bounds memory: 5000 bars covers 1m bars for ~3.5 days and
// 1h bars for over 200 days, which is more history than a v1 client can chart.
const maxCandlesPerMarket = 5000

// CandleStore aggregates trades into OHLCV bars per (market, interval).
//
// It is fed from the same TopicPerpTrade stream the WebSocket hub consumes, so a
// candle is always derived from the exact trades clients see — no separate
// reconstruction path that could disagree with the book.
type CandleStore struct {
	mu      sync.Mutex
	candles map[perp.MarketID]map[Interval][]Candle
	// lastPrice per market, used so an empty bucket still has a reference for
	// carry-forward when a client asks for a continuous chart.
	lastPrice map[perp.MarketID]perp.Price
}

// NewCandleStore builds an empty store.
func NewCandleStore() *CandleStore {
	return &CandleStore{
		candles:   make(map[perp.MarketID]map[Interval][]Candle),
		lastPrice: make(map[perp.MarketID]perp.Price),
	}
}

// Subscribe wires the store to the trade topic.
func (c *CandleStore) Subscribe(bus *EventBus) {
	bus.Subscribe(TopicPerpTrade, func(e Event) error {
		if t, ok := e.Data.(perp.Trade); ok {
			c.Record(t, e.Timestamp)
		}
		return nil
	})
}

// Record folds one trade into every supported interval. at is the trade time;
// callers pass the event timestamp so the bar boundary is the real trade time,
// not the moment the aggregator happened to run.
func (c *CandleStore) Record(t perp.Trade, at time.Time) {
	if t.Qty == nil || t.Qty.IsZero() || t.Price == 0 {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, iv := range []Interval{Interval1m, Interval5m, Interval15m, Interval1h, Interval4h, Interval1d} {
		byIv := c.candles[t.Market]
		if byIv == nil {
			byIv = make(map[Interval][]Candle)
			c.candles[t.Market] = byIv
		}
		series := byIv[iv]
		start := bucketStart(at, iv)

		last := len(series) - 1
		if last >= 0 && series[last].OpenTime == start {
			cur := &series[last]
			if t.Price > cur.High {
				cur.High = t.Price
			}
			if t.Price < cur.Low {
				cur.Low = t.Price
			}
			cur.Close = t.Price
			cur.Volume.Add(cur.Volume, t.Qty)
			cur.TradeCount++
		} else if last >= 0 && series[last].OpenTime > start {
			// Out-of-order trade inside an existing bar: fold it in rather than
			// creating a duplicate bucket, so the series stays sorted.
			cur := &series[last]
			if t.Price > cur.High {
				cur.High = t.Price
			}
			if t.Price < cur.Low {
				cur.Low = t.Price
			}
			cur.Volume.Add(cur.Volume, t.Qty)
			cur.TradeCount++
		} else {
			series = append(series, Candle{
				Market: t.Market, Interval: iv, OpenTime: start,
				Open: t.Price, High: t.Price, Low: t.Price, Close: t.Price,
				Volume: new(uint256.Int).Set(t.Qty), TradeCount: 1,
			})
			if len(series) > maxCandlesPerMarket {
				series = series[len(series)-maxCandlesPerMarket:]
			}
		}
		byIv[iv] = series
	}
	c.lastPrice[t.Market] = t.Price
}

// Series returns up to limit candles for a market/interval, newest last. A limit
// of zero or less returns everything retained.
func (c *CandleStore) Series(m perp.MarketID, iv Interval, limit int) []Candle {
	c.mu.Lock()
	defer c.mu.Unlock()
	byIv := c.candles[m]
	if byIv == nil {
		return nil
	}
	series := byIv[iv]
	if limit > 0 && len(series) > limit {
		series = series[len(series)-limit:]
	}
	out := make([]Candle, len(series))
	copy(out, series)
	// Copy the volume pointers so a caller cannot mutate store state.
	for i := range out {
		out[i].Volume = new(uint256.Int).Set(out[i].Volume)
	}
	return out
}

// LastPrice returns the most recent traded price for a market.
func (c *CandleStore) LastPrice(m perp.MarketID) (perp.Price, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.lastPrice[m]
	return p, ok
}

// Markets returns the markets that have at least one candle, sorted.
func (c *CandleStore) Markets() []perp.MarketID {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]perp.MarketID, 0, len(c.candles))
	for m := range c.candles {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

package services

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/perp"
)

// StaticPriceSource is a fixed index price map, for tests and for local runs
// where no venue feed is wired up. It is an operator-trusted input: production
// must swap in a median-of-venues source (spec §6.1).
type StaticPriceSource struct {
	mu     sync.RWMutex
	prices map[perp.MarketID]perp.Price
}

// NewStaticPriceSource builds an empty source.
func NewStaticPriceSource() *StaticPriceSource {
	return &StaticPriceSource{prices: make(map[perp.MarketID]perp.Price)}
}

// Set records a price for a market.
func (s *StaticPriceSource) Set(m perp.MarketID, p perp.Price) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prices[m] = p
}

// Index implements perp.PriceSource.
func (s *StaticPriceSource) Index(m perp.MarketID) (perp.Price, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.prices[m]
	return p, ok && p != 0
}

// ParseIndexPrices parses "ID:PRICE;ID:PRICE". A malformed entry is an error: an
// operator typo must not silently leave a market without a price.
func ParseIndexPrices(s string) (*StaticPriceSource, error) {
	src := NewStaticPriceSource()
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		if len(fields) != 2 {
			return nil, fmt.Errorf("services: index price %q: want ID:PRICE", part)
		}
		id, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: index price %q: bad market id: %w", part, err)
		}
		price, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil || price == 0 {
			return nil, fmt.Errorf("services: index price %q: bad price (must be a positive integer)", part)
		}
		src.Set(perp.MarketID(id), perp.Price(price))
	}
	return src, nil
}

// FundingOptions configures the periodic funding scheduler.
type FundingOptions struct {
	Tick         time.Duration
	Interval     time.Duration
	MaxRateE18   *uint256.Int
	MaxDeviation *uint256.Int
}

// nextFundingIndex returns the round index to use after the given prior index.
// Round indexes are 1-based, so a zero prior means "no round yet".
func nextFundingIndex(prior uint64) uint64 { return prior + 1 }

// RunFundingRound computes the rate from the current mark and index and settles
// one funding round (spec §3.3-3.4). It reports false when the market has no mark
// yet, in which case nothing is settled (the round is skipped, not deferred).
func (s *PerpOrderBookService) RunFundingRound(m perp.MarketID, now time.Time, interval time.Duration, maxRateE18 *uint256.Int) (FundingResult, bool, error) {
	s.mu.Lock()
	_, ok := s.markets[m]
	tracker := s.tracker
	prior := s.funding[m].LastIndex
	s.mu.Unlock()
	if !ok {
		return FundingResult{}, false, perp.ErrUnknownMarket
	}
	if tracker == nil {
		return FundingResult{}, false, nil
	}
	mark, ok := tracker.LastMark(m)
	if !ok {
		return FundingResult{}, false, nil
	}
	index, ok := tracker.IndexPrice(m)
	if !ok {
		return FundingResult{}, false, nil
	}
	rate, err := perp.FundingRateFromMark(index, mark, uint64(interval/time.Second), maxRateE18)
	if err != nil {
		return FundingResult{}, false, err
	}
	res, err := s.SettleFunding(m, nextFundingIndex(prior), mark, rate, now.Unix())
	if err != nil {
		return FundingResult{}, false, err
	}
	return res, true, nil
}

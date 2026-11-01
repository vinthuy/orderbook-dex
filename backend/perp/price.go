package perp

import (
	"fmt"

	"github.com/holiman/uint256"
)

// scaleE18 is the fixed-point scale for rates and deviation bounds.
const scaleE18 uint64 = 1_000_000_000_000_000_000

// Defaults for mark-price derivation (spec §3.2).
var (
	defaultMaxDeviationE18 = uint256.NewInt(5_000_000_000_000_000) // 0.5%
	defaultMarkWeight      = uint32(1)
)

// PriceSource supplies an external index price per market. It is injected by
// the service layer; the kernel itself never calls it, so determinism holds.
type PriceSource interface {
	// Index returns the index price for the market and whether a fresh price
	// is available at all.
	Index(m MarketID) (Price, bool)
}

// MarkConfig configures how a market derives its mark price from the index
// price and the perpetual book (spec §3.2). The zero value means defaults.
type MarkConfig struct {
	// MaxDeviationE18 bounds |bookMid/index - 1|, scaled by 1e18.
	MaxDeviationE18 *uint256.Int
	// IndexWeight and BookWeight are the weights of the index price and the
	// (clamped) book mid in the mark price. Both zero means 1:1.
	IndexWeight uint32
	BookWeight  uint32
}

func (c MarkConfig) maxDeviation() *uint256.Int {
	if c.MaxDeviationE18 == nil || c.MaxDeviationE18.IsZero() {
		return new(uint256.Int).Set(defaultMaxDeviationE18)
	}
	return c.MaxDeviationE18
}

func (c MarkConfig) weights() (uint32, uint32) {
	if c.IndexWeight == 0 && c.BookWeight == 0 {
		return defaultMarkWeight, defaultMarkWeight
	}
	if c.IndexWeight == 0 {
		return defaultMarkWeight, c.BookWeight
	}
	if c.BookWeight == 0 {
		return c.IndexWeight, defaultMarkWeight
	}
	return c.IndexWeight, c.BookWeight
}

// MarkPrice derives the mark price from the index price and the best bid/ask
// (spec §3.2):
//
//	mid     = (bestBid + bestAsk) / 2   (one-sided uses that side; no book -> index)
//	clamped = clamp(mid, index*(1-dev), index*(1+dev))
//	mark    = (index*wi + clamped*wb) / (wi + wb)
//
// It is a pure function: no clock, no I/O, no randomness. Clamping the book
// contribution is what makes the mark resistant to a single large order
// yanking a thin book. It returns an error only for a zero index price; a
// caller with no price at all must treat that as "no mark" rather than calling
// this with index 0.
func (m Market) MarkPrice(index Price, bestBid, bestAsk Price, hasBid, hasAsk bool) (Price, error) {
	if index == 0 {
		return 0, fmt.Errorf("%w: mark price requires a positive index price", ErrBadRequest)
	}
	idx := uint256.NewInt(uint64(index))

	var mid *uint256.Int
	switch {
	case hasBid && hasAsk:
		// The sum of two uint64 prices fits comfortably in a uint256.
		sum := new(uint256.Int).Add(uint256.NewInt(uint64(bestBid)), uint256.NewInt(uint64(bestAsk)))
		mid = sum.Div(sum, uint256.NewInt(2))
	case hasBid:
		mid = uint256.NewInt(uint64(bestBid))
	case hasAsk:
		mid = uint256.NewInt(uint64(bestAsk))
	default:
		return index, nil
	}

	dev := m.MarkConfig.maxDeviation()
	e18 := uint256.NewInt(scaleE18)

	upper, overflow := new(uint256.Int).MulOverflow(idx, new(uint256.Int).Add(e18, dev))
	if overflow {
		return 0, ErrAmountTooLarge
	}
	upper.Div(upper, e18)

	// A deviation >= 100% would make the lower bound negative; floor it at 0.
	var devSub *uint256.Int
	if dev.Cmp(e18) >= 0 {
		devSub = uint256.NewInt(0)
	} else {
		devSub = new(uint256.Int).Sub(e18, dev)
	}
	lower, overflow := new(uint256.Int).MulOverflow(idx, devSub)
	if overflow {
		return 0, ErrAmountTooLarge
	}
	lower.Div(lower, e18)

	clamped := mid
	if clamped.Cmp(upper) > 0 {
		clamped = upper
	} else if clamped.Cmp(lower) < 0 {
		clamped = lower
	}

	wi, wb := m.MarkConfig.weights()
	wsum := uint64(wi) + uint64(wb)

	num, overflow := new(uint256.Int).MulOverflow(idx, uint256.NewInt(uint64(wi)))
	if overflow {
		return 0, ErrAmountTooLarge
	}
	bookNum, overflow := new(uint256.Int).MulOverflow(clamped, uint256.NewInt(uint64(wb)))
	if overflow {
		return 0, ErrAmountTooLarge
	}
	num.Add(num, bookNum)
	num.Div(num, uint256.NewInt(wsum))
	if num.IsZero() || !num.IsUint64() {
		return 0, ErrAmountTooLarge
	}
	return Price(num.Uint64()), nil
}

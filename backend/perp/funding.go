package perp

import (
	"fmt"
	"math/big"

	"github.com/holiman/uint256"
)

// FundingRateFromMark computes the periodic funding rate from the premium
// between mark and index (spec §3.3):
//
//	premium = (mark - index) / index
//	rate    = clamp(premium / intervalHours, -maxRate, +maxRate)
//
// The result is SIGNED and scaled by 1e18; positive means longs pay shorts.
// intervalSeconds must be > 0 and is floored to whole hours (at least one), so
// an 8-hour interval yields one eighth of the premium as the hourly rate.
func FundingRateFromMark(index, mark Price, intervalSeconds uint64, maxRateE18 *uint256.Int) (*big.Int, error) {
	if index == 0 {
		return nil, fmt.Errorf("%w: funding rate requires a positive index price", ErrBadRequest)
	}
	if intervalSeconds == 0 {
		return nil, fmt.Errorf("%w: funding interval must be > 0", ErrBadRequest)
	}

	// premium = (mark - index) * 1e18 / index, signed.
	diff := new(big.Int).Sub(new(big.Int).SetUint64(uint64(mark)), new(big.Int).SetUint64(uint64(index)))
	premium := diff.Mul(diff, new(big.Int).SetUint64(scaleE18))
	premium.Div(premium, new(big.Int).SetUint64(uint64(index)))

	hours := intervalSeconds / 3600
	if hours == 0 {
		hours = 1
	}
	rate := premium.Div(premium, new(big.Int).SetUint64(hours))

	if maxRateE18 != nil {
		max := maxRateE18.ToBig()
		neg := new(big.Int).Neg(new(big.Int).Set(max))
		if rate.Cmp(max) > 0 {
			rate.Set(max)
		} else if rate.Cmp(neg) < 0 {
			rate.Set(neg)
		}
	}
	return rate, nil
}

// FundingAmount returns the absolute funding payment for a position of size
// base units at mark price, given a signed rate scaled by 1e18 (spec §3.4):
//
//	amount = floor(notional(size, mark) * |rate| / 1e18)
//
// The sign of the rate is the caller's to interpret (positive means longs pay);
// this returns a magnitude so a payer can be debited and a receiver credited.
func (m Market) FundingAmount(size *uint256.Int, mark Price, rateScaled *big.Int) (*uint256.Int, error) {
	if rateScaled == nil || rateScaled.Sign() == 0 || size == nil || size.IsZero() {
		return uint256.NewInt(0), nil
	}
	if rateScaled.Sign() < 0 {
		rateScaled = new(big.Int).Neg(rateScaled)
	}
	notional, err := m.notionalChecked(size, mark, false)
	if err != nil {
		return nil, err
	}
	rateU, overflow := uint256.FromBig(rateScaled)
	if overflow {
		return nil, ErrAmountTooLarge
	}
	prod, overflow := new(uint256.Int).MulOverflow(notional, rateU)
	if overflow {
		return nil, ErrAmountTooLarge
	}
	return prod.Div(prod, uint256.NewInt(scaleE18)), nil
}

package perp

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"
)

func TestFundingRatePositivePremium(t *testing.T) {
	// index 2000, mark 2002, 1h interval -> premium 0.001 -> rate 0.001
	rate, err := FundingRateFromMark(2000, 2002, 3600, uint256.NewInt(75_000_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want, _ := new(big.Int).SetString("1000000000000000", 10) // 0.001
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateNegativePremium(t *testing.T) {
	rate, err := FundingRateFromMark(2000, 1998, 3600, uint256.NewInt(75_000_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want, _ := new(big.Int).SetString("-1000000000000000", 10)
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateClampsToMax(t *testing.T) {
	// 10% premium with a 0.75% cap -> 0.75%
	rate, err := FundingRateFromMark(2000, 2200, 3600, uint256.NewInt(7_500_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want := uint256.NewInt(7_500_000_000_000_000).ToBig()
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateClampsNegativeToMax(t *testing.T) {
	rate, err := FundingRateFromMark(2000, 1800, 3600, uint256.NewInt(7_500_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want := new(big.Int).Neg(uint256.NewInt(7_500_000_000_000_000).ToBig())
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateScalesWithInterval(t *testing.T) {
	// 0.1% premium over 8h -> 0.0125% per hour
	rate, err := FundingRateFromMark(2000, 2002, 8*3600, uint256.NewInt(7_500_000_000_000_000))
	if err != nil {
		t.Fatalf("FundingRateFromMark: %v", err)
	}
	want, _ := new(big.Int).SetString("125000000000000", 10) // 0.000125
	if rate.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, rate)
	}
}

func TestFundingRateZeroIndexRejected(t *testing.T) {
	if _, err := FundingRateFromMark(0, 2000, 3600, uint256.NewInt(1)); err == nil {
		t.Fatal("want error for zero index")
	}
}

func TestFundingRateZeroIntervalRejected(t *testing.T) {
	if _, err := FundingRateFromMark(2000, 2001, 0, uint256.NewInt(1)); err == nil {
		t.Fatal("want error for zero interval")
	}
}

func TestFundingAmountPositive(t *testing.T) {
	m := testMarkMarket()
	// 10 base at 18 decimals, mark 2000 -> notional 20000e18, rate 0.001
	size, _ := uint256.FromDecimal("10000000000000000000")
	rate, _ := new(big.Int).SetString("1000000000000000", 10)
	got, err := m.FundingAmount(size, 2000, rate)
	if err != nil {
		t.Fatalf("FundingAmount: %v", err)
	}
	want, _ := uint256.FromDecimal("20") // 20000 * 0.001
	if got.Cmp(want) != 0 {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestFundingAmountNegativeRateIsAbsolute(t *testing.T) {
	m := testMarkMarket()
	size, _ := uint256.FromDecimal("10000000000000000000")
	rate, _ := new(big.Int).SetString("-1000000000000000", 10)
	got, err := m.FundingAmount(size, 2000, rate)
	if err != nil {
		t.Fatalf("FundingAmount: %v", err)
	}
	want, _ := uint256.FromDecimal("20")
	if got.Cmp(want) != 0 {
		t.Fatalf("want absolute %s, got %s", want, got)
	}
}

func TestFundingAmountZeroSizeIsZero(t *testing.T) {
	m := testMarkMarket()
	got, err := m.FundingAmount(uint256.NewInt(0), 2000, new(big.Int))
	if err != nil {
		t.Fatalf("FundingAmount: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("want 0, got %s", got)
	}
}

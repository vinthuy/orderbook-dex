package perp

import (
	"testing"

	"github.com/holiman/uint256"
)

func testMarkMarket() Market {
	return Market{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 18, MaxLeverage: 10,
		MarkConfig: MarkConfig{
			MaxDeviationE18: uint256.NewInt(5_000_000_000_000_000), // 0.5%
			IndexWeight:     1,
			BookWeight:      1,
		},
	}
}

func TestMarkPriceEmptyBookEqualsIndex(t *testing.T) {
	m := testMarkMarket()
	got, err := m.MarkPrice(2000, 0, 0, false, false)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2000 {
		t.Fatalf("want index 2000, got %d", got)
	}
}

func TestMarkPriceTwoSidedIsAverageOfIndexAndMid(t *testing.T) {
	m := testMarkMarket()
	// index 2000, mid 2002 -> (2000+2002)/2 = 2001
	got, err := m.MarkPrice(2000, 2001, 2003, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2001 {
		t.Fatalf("want 2001, got %d", got)
	}
}

func TestMarkPriceClampsBookDeviation(t *testing.T) {
	m := testMarkMarket()
	// mid 3000 is 50% above index 2000 -> clamped to 2010, averaged to 2005
	got, err := m.MarkPrice(2000, 2999, 3001, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2005 {
		t.Fatalf("want 2005 (average of 2000 and clamped 2010), got %d", got)
	}
}

func TestMarkPriceOneSidedUsesThatSide(t *testing.T) {
	m := testMarkMarket()
	got, err := m.MarkPrice(2000, 2004, 0, true, false)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	// clamped mid 2004, average 2002
	if got != 2002 {
		t.Fatalf("want 2002, got %d", got)
	}
}

func TestMarkPriceRejectsZeroIndex(t *testing.T) {
	m := testMarkMarket()
	if _, err := m.MarkPrice(0, 2000, 2001, true, true); err == nil {
		t.Fatal("want error for zero index")
	}
}

func TestMarkPriceClampsDownward(t *testing.T) {
	m := testMarkMarket()
	// mid 1000 is 50% below index 2000 -> clamped to 1990, average 1995
	got, err := m.MarkPrice(2000, 999, 1001, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 1995 {
		t.Fatalf("want 1995, got %d", got)
	}
}

func TestMarkPriceDefaultConfigApplies(t *testing.T) {
	m := testMarkMarket()
	m.MarkConfig = MarkConfig{} // all zero -> defaults
	got, err := m.MarkPrice(2000, 2001, 2003, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2001 {
		t.Fatalf("want 2001 with default config, got %d", got)
	}
}

func TestMarkPriceZeroDeviationPinsToIndex(t *testing.T) {
	m := testMarkMarket()
	// A zero MaxDeviation means "use the default", never "allow anything".
	m.MarkConfig = MarkConfig{IndexWeight: 1, BookWeight: 1}
	got, err := m.MarkPrice(2000, 4000, 4002, true, true)
	if err != nil {
		t.Fatalf("MarkPrice: %v", err)
	}
	if got != 2005 {
		t.Fatalf("want 2005 (clamped to 1.005x index), got %d", got)
	}
}

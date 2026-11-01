package perp

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"
)

func TestUnrealizedPnL(t *testing.T) {
	m := testMarkMarket()
	size, _ := uint256.FromDecimal("10000000000000000000") // 10 base

	// long entry 2000, mark 2100 -> +1000 quote
	got, err := m.UnrealizedPnL(SideLong, size, 2000, 2100)
	if err != nil {
		t.Fatalf("UnrealizedPnL: %v", err)
	}
	want, _ := new(big.Int).SetString("1000", 10)
	if got.Cmp(want) != 0 {
		t.Fatalf("long: want %s, got %s", want, got)
	}

	// short entry 2000, mark 2100 -> -1000 quote
	got, err = m.UnrealizedPnL(SideShort, size, 2000, 2100)
	if err != nil {
		t.Fatalf("UnrealizedPnL: %v", err)
	}
	want = new(big.Int).Neg(want)
	if got.Cmp(want) != 0 {
		t.Fatalf("short: want %s, got %s", want, got)
	}

	// zero size -> zero
	got, err = m.UnrealizedPnL(SideLong, uint256.NewInt(0), 2000, 2100)
	if err != nil {
		t.Fatalf("UnrealizedPnL: %v", err)
	}
	if got.Sign() != 0 {
		t.Fatalf("zero size: want 0, got %s", got)
	}
}

func TestUnrealizedPnLRequiresPositiveMark(t *testing.T) {
	m := testMarkMarket()
	if _, err := m.UnrealizedPnL(SideLong, uint256.NewInt(1), 2000, 0); err == nil {
		t.Fatal("want error for zero mark")
	}
}

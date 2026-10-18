package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func TestMarketValidate(t *testing.T) {
	ok := Market{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid market rejected: %v", err)
	}
	bad := ok
	bad.Base = bad.Quote
	if err := bad.Validate(); err == nil {
		t.Fatal("base==quote should be rejected")
	}
	bad2 := ok
	bad2.MaxLeverage = 0
	if err := bad2.Validate(); err == nil {
		t.Fatal("zero leverage should be rejected")
	}
}

func TestNotionalFloor(t *testing.T) {
	m := Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
	// notional = floor(qty*price/10^2)
	if got := m.Notional(uint256.NewInt(100), 1); got.Uint64() != 1 {
		t.Fatalf("Notional(100,1) = %s, want 1 (100*1/100)", got.Dec())
	}
	if got := m.NotionalCeil(uint256.NewInt(1), 1); got.Uint64() != 1 {
		t.Fatalf("NotionalCeil(1,1) = %s, want 1 (ceil(1*1/100))", got.Dec())
	}
}

func TestMarginFor(t *testing.T) {
	m := Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}
	// notional(100,100) = 100*100/100 = 100; ceil(100/10) = 10
	got, err := m.marginCeil(uint256.NewInt(100), 100, 10)
	if err != nil {
		t.Fatalf("marginCeil: %v", err)
	}
	if got.Uint64() != 10 {
		t.Fatalf("marginCeil = %s, want 10", got.Dec())
	}
	// floor(notional(100,100)/10) = floor(100/10) = 10
	got2, err := m.marginFloor(uint256.NewInt(100), 100, 10)
	if err != nil {
		t.Fatalf("marginFloor: %v", err)
	}
	if got2.Uint64() != 10 {
		t.Fatalf("marginFloor = %s, want 10", got2.Dec())
	}
}

func TestAccountIDUnchanged(t *testing.T) {
	_ = ledger.AssetID(1)
}

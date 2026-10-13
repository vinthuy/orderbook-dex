package matching

import (
	"errors"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// --- shared test helpers (same package, used across the _test.go files) ---

func tacct(b byte) ledger.AccountID {
	var a ledger.AccountID
	for i := range a {
		a[i] = b
	}
	return a
}

func amt(n uint64) *uint256.Int { return uint256.NewInt(n) }

// dec builds a uint256 from a decimal string (test convenience).
func dec(s string) *uint256.Int {
	v, err := uint256.FromDecimal(s)
	if err != nil {
		panic(err)
	}
	return v
}

// testMarket is the arithmetic-friendly market: 2 base decimals, so scale=100.
func testMarket() Market {
	return Market{ID: 1, Symbol: "ETH/USDT", Base: 2, Quote: 1, BaseDecimals: 2}
}

func newTestEngine(t *testing.T, markets ...Market) *Engine {
	t.Helper()
	e, err := NewEngine(markets)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func assertPanics(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected panic, got none", name)
		}
	}()
	f()
}

// --- Market.Validate ---

func TestMarketValidate(t *testing.T) {
	base := Market{ID: 1, Symbol: "ETH/USDT", Base: 2, Quote: 1, BaseDecimals: 18}
	tests := []struct {
		name    string
		mutate  func(m *Market)
		wantErr bool
	}{
		{"valid", func(m *Market) {}, false},
		{"valid min qty", func(m *Market) { m.MinQty = amt(0) }, false},
		{"empty symbol", func(m *Market) { m.Symbol = "" }, true},
		{"base equals quote", func(m *Market) { m.Quote = m.Base }, true},
		{"decimals 76 ok", func(m *Market) { m.BaseDecimals = 76 }, false},
		{"decimals 77 too big", func(m *Market) { m.BaseDecimals = 77 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base
			tt.mutate(&m)
			err := m.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate(%+v) = nil, want error", m)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", m, err)
			}
		})
	}
}

// TestValidateErrorNamesMarket pins that failures carry the market symbol, so
// operators can tell which configured market is broken.
func TestValidateErrorNamesMarket(t *testing.T) {
	m := Market{ID: 7, Symbol: "BAD/PAIR", Base: 1, Quote: 1, BaseDecimals: 2}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if want := "BAD/PAIR"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name market %q", err.Error(), want)
	}
}

// --- Notional / NotionalCeil ---

func TestNotionalFloorAndCeil(t *testing.T) {
	m := testMarket() // scale = 100
	tests := []struct {
		name      string
		qty       uint64
		price     Price
		wantFloor uint64
		wantCeil  uint64
	}{
		{"exact division", 100, 250, 250, 250},
		{"remainder one", 7, 101, 7, 8},
		{"zero qty", 0, 101, 0, 0},
		{"rounds up from zero", 1, 1, 0, 1},
		{"large exact", 1000, 300, 3000, 3000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.Notional(amt(tt.qty), tt.price); got.Uint64() != tt.wantFloor {
				t.Fatalf("Notional(%d,%d) = %s, want %d", tt.qty, tt.price, got, tt.wantFloor)
			}
			if got := m.NotionalCeil(amt(tt.qty), tt.price); got.Uint64() != tt.wantCeil {
				t.Fatalf("NotionalCeil(%d,%d) = %s, want %d", tt.qty, tt.price, got, tt.wantCeil)
			}
		})
	}
}

// TestNotionalLargeDecimals checks the scale at the 18/76 boundary: with 18
// decimals a 1-unit trade rounds down to zero but still reserves one ceil unit,
// and 76 decimals is large yet representative.
func TestNotionalLargeDecimals(t *testing.T) {
	m18 := Market{ID: 1, Symbol: "X/Y", Base: 2, Quote: 1, BaseDecimals: 18}
	if got := m18.Notional(amt(1), 1); !got.IsZero() {
		t.Fatalf("18dp Notional(1,1) = %s, want 0", got)
	}
	if got := m18.NotionalCeil(amt(1), 1); got.Uint64() != 1 {
		t.Fatalf("18dp NotionalCeil(1,1) = %s, want 1", got)
	}
	want := dec("1000000000000000000")
	if got := m18.Notional(dec("1000000000000000000"), Price(dec("1000000000000000000").Uint64())); got.Cmp(want) != 0 {
		t.Fatalf("18dp Notional(1e18, 1e18) = %s, want %s", got, want)
	}

	m76 := Market{ID: 1, Symbol: "X/Y", Base: 2, Quote: 1, BaseDecimals: 76}
	if got := m76.Notional(amt(1), 1); !got.IsZero() {
		t.Fatalf("76dp Notional(1,1) = %s, want 0", got)
	}
	if got := m76.NotionalCeil(amt(1), 1); got.Uint64() != 1 {
		t.Fatalf("76dp NotionalCeil(1,1) = %s, want 1", got)
	}
}

// TestNotionalZeroPricePanics pins the loud behaviour: a zero price is a kernel
// bug, so it must panic rather than silently return 0.
func TestNotionalZeroPricePanics(t *testing.T) {
	m := testMarket()
	assertPanics(t, "Notional", func() { m.Notional(amt(1), 0) })
	assertPanics(t, "NotionalCeil", func() { m.NotionalCeil(amt(1), 0) })
}

// TestNotionalDoesNotAliasInputs guards the pointer-copy rule at the math
// boundary: the inputs must survive a call unchanged.
func TestNotionalDoesNotAliasInputs(t *testing.T) {
	m := testMarket()
	qty := amt(7)
	m.Notional(qty, 101)
	m.NotionalCeil(qty, 101)
	if qty.Uint64() != 7 {
		t.Fatalf("qty mutated by Notional: %s", qty)
	}
}

// --- NewEngine market-set validation ---

func TestNewEngineRejectsBadMarketSets(t *testing.T) {
	valid := testMarket()
	other := Market{ID: 2, Symbol: "BTC/USDT", Base: 3, Quote: 1, BaseDecimals: 18}

	dupID := other
	dupID.ID = valid.ID
	dupSymbol := other
	dupSymbol.Symbol = valid.Symbol

	tests := []struct {
		name    string
		markets []Market
	}{
		{"duplicate id", []Market{valid, dupID}},
		{"duplicate symbol", []Market{valid, dupSymbol}},
		{"invalid market", []Market{{ID: 3, Symbol: "", Base: 1, Quote: 2}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewEngine(tt.markets); err == nil {
				t.Fatal("NewEngine = nil error, want failure")
			}
		})
	}

	e, err := NewEngine([]Market{valid, other})
	if err != nil {
		t.Fatalf("valid set: %v", err)
	}
	if len(e.Markets()) != 2 {
		t.Fatalf("Markets len = %d, want 2", len(e.Markets()))
	}
}

// TestMarketsReturnsDetachedCopy ensures mutating the returned slice or its
// MinQty cannot corrupt engine state.
func TestMarketsReturnsDetachedCopy(t *testing.T) {
	m := testMarket()
	m.MinQty = amt(5)
	e := newTestEngine(t, m)

	got := e.Markets()
	got[0].Symbol = "MUTATED"
	got[0].MinQty.SetUint64(9999)

	if e.Markets()[0].Symbol != "ETH/USDT" {
		t.Fatal("Symbol mutated through returned slice")
	}
	if e.Markets()[0].MinQty.Uint64() != 5 {
		t.Fatal("MinQty mutated through returned slice")
	}
}

func TestErrBadRequestIsSentinel(t *testing.T) {
	e := newTestEngine(t, testMarket())
	_, err := e.Place(PlaceRequest{Market: 1, Qty: amt(1), Type: TypeLimit, Price: 0})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v, want ErrBadRequest", err)
	}
}

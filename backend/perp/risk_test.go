package perp

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func riskMarket() Market {
	m := testMarkMarket()
	m.BaseDecimals = 0 // notional == size*price, easier to reason about
	return m
}

func TestMaintenanceMargin(t *testing.T) {
	m := riskMarket() // maintenance 0.5%
	got, err := m.MaintenanceMargin(uint256.NewInt(1000), 100)
	if err != nil {
		t.Fatalf("MaintenanceMargin: %v", err)
	}
	// notional 100000 * 0.005 = 500
	if got.Uint64() != 500 {
		t.Fatalf("want 500, got %s", got)
	}
}

func TestMaintenanceMarginRoundsUp(t *testing.T) {
	m := riskMarket()
	// notional 1 * 0.005 = 0.005 -> ceil to 1
	got, err := m.MaintenanceMargin(uint256.NewInt(1), 1)
	if err != nil {
		t.Fatalf("MaintenanceMargin: %v", err)
	}
	if got.Uint64() != 1 {
		t.Fatalf("want ceiling 1, got %s", got)
	}
}

func TestMaintenanceMarginZeroSize(t *testing.T) {
	m := riskMarket()
	got, err := m.MaintenanceMargin(uint256.NewInt(0), 100)
	if err != nil {
		t.Fatalf("MaintenanceMargin: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("want 0, got %s", got)
	}
}

func TestMaintenanceMarginRejectsZeroMark(t *testing.T) {
	m := riskMarket()
	if _, err := m.MaintenanceMargin(uint256.NewInt(1000), 0); err == nil {
		t.Fatal("want error for zero mark")
	}
}

func TestEquity(t *testing.T) {
	m := riskMarket()
	size := uint256.NewInt(1000)

	// Long entry 100, mark 110: PnL = (110-100)*1000 = 10000, margin 5000.
	eq, err := m.Equity(SideLong, size, 100, 110, uint256.NewInt(5000))
	if err != nil {
		t.Fatalf("Equity: %v", err)
	}
	if eq.Cmp(big.NewInt(15000)) != 0 {
		t.Fatalf("want 15000, got %s", eq)
	}

	// Long entry 100, mark 90: PnL = -10000, margin 5000 -> equity -5000.
	eq, err = m.Equity(SideLong, size, 100, 90, uint256.NewInt(5000))
	if err != nil {
		t.Fatalf("Equity: %v", err)
	}
	if eq.Cmp(big.NewInt(-5000)) != 0 {
		t.Fatalf("want -5000, got %s", eq)
	}
}

func TestLiquidatable(t *testing.T) {
	m := riskMarket()

	// Long 1000 @ 100, margin 5000 (5% of notional), maintenance 500.
	// At mark 100: equity 5000 >= 500 -> healthy.
	p := &Position{Market: 1, Side: SideLong, Size: uint256.NewInt(1000),
		EntryPrice: 100, Margin: uint256.NewInt(5000), Leverage: 10}
	bad, err := m.Liquidatable(p, 100)
	if err != nil {
		t.Fatalf("Liquidatable: %v", err)
	}
	if bad {
		t.Fatal("a healthy position must not be liquidatable")
	}

	// At mark 95: PnL -5000, equity 0 < maintenance 475 -> liquidatable.
	bad, err = m.Liquidatable(p, 95)
	if err != nil {
		t.Fatalf("Liquidatable: %v", err)
	}
	if !bad {
		t.Fatal("an underwater position must be liquidatable")
	}
}

func TestLiquidatableRequiresMark(t *testing.T) {
	m := riskMarket()
	p := &Position{Market: 1, Side: SideLong, Size: uint256.NewInt(1000),
		EntryPrice: 100, Margin: uint256.NewInt(5000), Leverage: 10}
	if _, err := m.Liquidatable(p, 0); err == nil {
		t.Fatal("want error without a mark price")
	}
}

func TestLiquidateClosesPosition(t *testing.T) {
	m := riskMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	long := ledger.AccountID([20]byte{1})
	short := ledger.AccountID([20]byte{2})

	// Short 1000 rests at 100; long 1000 trades against it.
	if _, err := e.Place(PlaceRequest{Market: 1, Account: short, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: long, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	if _, ok := e.Position(1, long); !ok {
		t.Fatal("want a long position")
	}

	// The short has no resting order left; the long liquidation sells into the
	// book, so first rest a bid from a third party for it to hit.
	buyer := ledger.AccountID([20]byte{3})
	if _, err := e.Place(PlaceRequest{Market: 1, Account: buyer, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 95, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place bid: %v", err)
	}

	res, err := e.Liquidate(1, long, nil)
	if err != nil {
		t.Fatalf("Liquidate: %v", err)
	}
	if len(res.Trades) == 0 {
		t.Fatal("want a liquidation trade")
	}
	if _, ok := e.Position(1, long); ok {
		t.Fatal("long position should be closed")
	}
	if res.Order.Status != StatusFilled {
		t.Fatalf("want filled liquidation, got %s", res.Order.Status)
	}
	if len(res.PnL) == 0 {
		t.Fatal("want a PnL settlement for the closed long")
	}
}

func TestLiquidateWithoutCounterpartyLeavesPosition(t *testing.T) {
	m := riskMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	long := ledger.AccountID([20]byte{1})
	short := ledger.AccountID([20]byte{2})
	if _, err := e.Place(PlaceRequest{Market: 1, Account: short, Side: SideShort,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := e.Place(PlaceRequest{Market: 1, Account: long, Side: SideLong,
		Type: TypeLimit, TIF: TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	if _, err := e.Liquidate(1, long, nil); err != nil {
		t.Fatalf("Liquidate: %v", err)
	}
	if _, ok := e.Position(1, long); !ok {
		t.Fatal("position must survive an unfillable liquidation")
	}
}

func TestLiquidateUnknownMarket(t *testing.T) {
	e, _ := NewEngine([]Market{riskMarket()})
	if _, err := e.Liquidate(99, ledger.AccountID([20]byte{1}), nil); err == nil {
		t.Fatal("want error for unknown market")
	}
}


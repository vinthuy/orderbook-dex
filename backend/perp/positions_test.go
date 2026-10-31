package perp

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func TestEnginePositionsSortedByAccount(t *testing.T) {
	m := testMarkMarket()
	m.MinQty = nil
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	acct1 := ledger.AccountID([20]byte{1})
	acct3 := ledger.AccountID([20]byte{3})

	// acct3 sells, acct1 buys against it.
	if _, err := e.Place(PlaceRequest{
		Market: 1, Account: acct3, Side: SideShort, Type: TypeLimit, TIF: TIFGTC,
		Price: 2000, Qty: uint256.NewInt(100), Leverage: 10,
	}); err != nil {
		t.Fatalf("Place maker: %v", err)
	}
	if _, err := e.Place(PlaceRequest{
		Market: 1, Account: acct1, Side: SideLong, Type: TypeLimit, TIF: TIFGTC,
		Price: 2000, Qty: uint256.NewInt(100), Leverage: 10,
	}); err != nil {
		t.Fatalf("Place taker: %v", err)
	}

	got := e.Positions(1)
	if len(got) != 2 {
		t.Fatalf("want 2 positions, got %d", len(got))
	}
	if got[0].Account != acct1 || got[1].Account != acct3 {
		t.Fatalf("want accounts sorted 1,3; got %x, %x", got[0].Account, got[1].Account)
	}
	if got[0].Side != SideLong || got[1].Side != SideShort {
		t.Fatalf("unexpected sides")
	}
}

func TestEnginePositionsReturnsCopies(t *testing.T) {
	m := testMarkMarket()
	e, err := NewEngine([]Market{m})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	acct := ledger.AccountID([20]byte{9})
	counter := ledger.AccountID([20]byte{10})
	if _, err := e.Place(PlaceRequest{
		Market: 1, Account: counter, Side: SideShort, Type: TypeLimit, TIF: TIFGTC,
		Price: 2000, Qty: uint256.NewInt(100), Leverage: 10,
	}); err != nil {
		t.Fatalf("Place maker: %v", err)
	}
	if _, err := e.Place(PlaceRequest{
		Market: 1, Account: acct, Side: SideLong, Type: TypeLimit, TIF: TIFGTC,
		Price: 2000, Qty: uint256.NewInt(100), Leverage: 10,
	}); err != nil {
		t.Fatalf("Place: %v", err)
	}
	got := e.Positions(1)
	var longIdx = -1
	for i := range got {
		if got[i].Account == acct {
			longIdx = i
		}
	}
	if longIdx < 0 {
		t.Fatalf("no position for account 9")
	}
	got[longIdx].Size.SetUint64(1)
	for _, p := range e.Positions(1) {
		if p.Account == acct && p.Size.Uint64() != 100 {
			t.Fatalf("Positions leaked internal state: size %s", p.Size)
		}
	}
}

func TestEnginePositionsUnknownMarket(t *testing.T) {
	e, _ := NewEngine([]Market{testMarkMarket()})
	if got := e.Positions(99); got != nil {
		t.Fatalf("want nil for unknown market, got %v", got)
	}
}

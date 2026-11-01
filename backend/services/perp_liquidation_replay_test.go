package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestLiquidationIsJournaledAndReplays checks whether a forced liquidation is
// journaled. If it is not, reopening the service on the same WAL resurrects the
// liquidated position — a fund-safety bug, because the trader would then hold a
// position the ledger already settled.
func TestLiquidationIsJournaledAndReplays(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2", "d3"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}

	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, dir), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 88, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 88)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("precondition: the long should have been liquidated")
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen on the same journal directory with fresh ledger/holds objects, as a
	// restart would.
	l2 := ledger.New()
	holds2 := ledger.NewHolds(l2)
	for i, ref := range []string{"d1", "d2", "d3"} {
		if err := l2.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc2, err := NewPerpOrderBookService(markets, l2, holds2, mustPerpJournal(t, dir), NewEventBus())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer svc2.Close()

	if p, ok := svc2.Engine().Position(1, perpAcct(2)); ok {
		t.Fatalf("liquidation was not journaled: replay resurrected a %s position of %s units",
			p.Side, p.Size)
	}
}


package services

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

func perpMarkets() []perp.Market {
	return []perp.Market{{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}}
}

func perpAcct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func mustPerpJournal(t *testing.T, dir string) *PerpJournal {
	t.Helper()
	j, err := NewPerpJournal(dir)
	if err != nil {
		t.Fatalf("NewPerpJournal: %v", err)
	}
	// Release the WAL handle before t.TempDir's RemoveAll runs (Windows refuses
	// to unlink an open file).
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestPerpOpenAndCloseSettlesPnL(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	// Fund three accounts with quote (asset 1).
	fund := func(a ledger.AccountID, ref string) {
		t.Helper()
		if err := l.Apply(ledger.DepositOp{Account: a, Asset: 1, Amount: uint256.NewInt(100000), Ref: ref}); err != nil {
			t.Fatalf("fund %s: %v", ref, err)
		}
	}
	fund(perpAcct(1), "d1")
	fund(perpAcct(2), "d2")
	fund(perpAcct(3), "d3")

	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustPerpJournal(t, ""), bus)
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	defer svc.Close()

	// Account 1 opens short 100 @ 100 (freezes margin 10).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open short: %v", err)
	}
	// Account 2 opens long 100 @ 100 (crosses).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open long: %v", err)
	}

	// Account 2 closes long 100 @ 120 (profit = notional(100,120)-notional(100,100)
	// = 120-100 = 20). The closing order is a SELL, so it crosses resting bids:
	// account 3 must rest a LONG at 120. (A resting short would sit on the ask
	// side and never cross; the plan's original test had the wrong side here.)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("rest long: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("close long: %v", err)
	}

	// Account 2's quote balance should reflect +20 profit (and margin release).
	bal := l.Balance(perpAcct(2), 1).Uint64()
	// started 100000, opened long (margin 10 frozen, not deducted), closed for +20.
	// The realized profit 20 is credited; margin was frozen then released.
	if bal != 100020 {
		t.Fatalf("account 2 balance = %d, want 100020", bal)
	}
	if held := holds.Held(perpAcct(2), 1); !held.IsZero() {
		t.Fatalf("account 2 held = %s, want 0 (margin must be fully released)", held.Dec())
	}
	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("account 2 still has a position, want closed")
	}
}

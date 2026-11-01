package services

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestFundingDebtSurvivesRestart pins that an unpaid funding debt is not forgiven
// by a restart. Debts live only in memory otherwise, and legs alone do not say
// what a broke payer still owes.
func TestFundingDebtSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	mk := func() (*PerpOrderBookService, *ledger.Ledger, *ledger.Holds) {
		l := ledger.New()
		h := ledger.NewHolds(l)
		markets := []perp.Market{{
			ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		}}
		// The long gets exactly enough to open, leaving no available balance.
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d1"}); err != nil {
			t.Fatal(err)
		}
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(10_000), Ref: "d2"}); err != nil {
			t.Fatal(err)
		}
		s, err := NewPerpOrderBookService(markets, l, h, mustPerpJournal(t, dir), NewEventBus())
		if err != nil {
			t.Fatalf("service: %v", err)
		}
		return s, l, h
	}

	svc, _, _ := mk()
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	// 10% rate: the long owes 1000 but has nothing available (all its balance is
	// frozen as margin), so the whole amount becomes debt.
	rate, _ := new(big.Int).SetString("100000000000000000", 10)
	res, err := svc.SettleFunding(1, 1, 100, rate, 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Skips) != 1 {
		t.Fatalf("want 1 skip, got %d", len(res.Skips))
	}
	before := svc.FundingDebtOf(perpAcct(2), 1)
	if before.IsZero() {
		t.Fatal("want a recorded debt")
	}
	t.Logf("debt before restart: %s", before)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	svc2, _, _ := mk()
	defer svc2.Close()
	after := svc2.FundingDebtOf(perpAcct(2), 1)
	if after.Cmp(before) != 0 {
		t.Fatalf("debt lost across restart: before=%s after=%s", before, after)
	}
}


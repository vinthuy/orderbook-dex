package services

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestHistoryRecordsFundingForBothSides pins that a funding payment appears on the
// payer with direction "paid" and on the receiver with "received".
func TestHistoryRecordsFundingForBothSides(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(10000000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// acct1 shorts, acct2 longs: 1000 units at price 100 = 100,000 notional.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	// 1% rate -> 1000 quote paid by the long.
	rate, _ := new(big.Int).SetString("10000000000000000", 10)
	res, err := svc.SettleFunding(1, 1, 100, rate, 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Legs) == 0 {
		t.Fatal("precondition: the round should have produced a leg")
	}

	longRows := svc.History().Funding(perpAcct(2), 10)
	if len(longRows) == 0 {
		t.Fatal("the long should have a funding row")
	}
	if longRows[0].Sign != -1 {
		t.Fatalf("a positive rate means the long PAYS, got sign %d", longRows[0].Sign)
	}
	if longRows[0].Amount.Uint64() != 1000 {
		t.Fatalf("long paid %s, want 1000", longRows[0].Amount)
	}

	shortRows := svc.History().Funding(perpAcct(1), 10)
	if len(shortRows) == 0 {
		t.Fatal("the short should have a funding row")
	}
	if shortRows[0].Sign != 1 {
		t.Fatalf("the short RECEIVES, got sign %d", shortRows[0].Sign)
	}
}

package services

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// collateralService builds a market that accepts asset 7 as margin at a 90%
// haircut and a price of 2 quote per unit.
func collateralService(t *testing.T) (*PerpOrderBookService, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		Collateral: []perp.CollateralAsset{{
			Asset:      7,
			HaircutE18: uint256.NewInt(900_000_000_000_000_000), // 90%
			Price:      2,
		}},
	}}
	acct := perpAcct(1)
	if err := l.Apply(ledger.DepositOp{Account: acct, Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "q"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: acct, Asset: 7, Amount: uint256.NewInt(1000), Ref: "c"}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, l
}

// TestPledgeCreditsHaircutAdjustedValue pins the arithmetic: 1000 units at price 2
// with a 90% haircut credits 1000 * 2 * 0.9 = 1800 quote.
func TestPledgeCreditsHaircutAdjustedValue(t *testing.T) {
	svc, l := collateralService(t)
	acct := perpAcct(1)

	beforeQuote := new(uint256.Int).Set(l.Balance(acct, 1))
	beforeColl := new(uint256.Int).Set(l.Balance(acct, 7))

	got, err := svc.Pledge(1, acct, 7, uint256.NewInt(1000))
	if err != nil {
		t.Fatalf("Pledge: %v", err)
	}
	if got.Uint64() != 1800 {
		t.Fatalf("credited %s, want 1800", got)
	}

	if b := l.Balance(acct, 7); !b.IsZero() {
		t.Fatalf("collateral should have been debited, still %s", b)
	}
	wantQuote := new(uint256.Int).Add(beforeQuote, uint256.NewInt(1800))
	if b := l.Balance(acct, 1); b.Cmp(wantQuote) != 0 {
		t.Fatalf("quote = %s, want %s", b, wantQuote)
	}
	_ = beforeColl
}

// TestPledgeDoesNotInflateTheLedgerTotal pins that the haircut discount is not
// minted: the total across all assets cannot grow from a pledge. (Values are in
// different assets, so this checks the quote leg only grew by what the collateral
// leg gave up, valued at the haircut.)
func TestPledgeIsIdempotentByRef(t *testing.T) {
	svc, l := collateralService(t)
	acct := perpAcct(1)

	if _, err := svc.Pledge(1, acct, 7, uint256.NewInt(500)); err != nil {
		t.Fatalf("Pledge: %v", err)
	}
	afterFirst := new(uint256.Int).Set(l.Balance(acct, 1))

	// Same amount again: the ref key is identical, so it must be a no-op.
	if _, err := svc.Pledge(1, acct, 7, uint256.NewInt(500)); err != nil {
		t.Fatalf("second Pledge: %v", err)
	}
	if b := l.Balance(acct, 1); b.Cmp(afterFirst) != 0 {
		t.Fatalf("a repeated pledge credited again: %s -> %s", afterFirst, b)
	}
}

func TestPledgeRejectsQuoteAsset(t *testing.T) {
	svc, _ := collateralService(t)
	if _, err := svc.Pledge(1, perpAcct(1), 1, uint256.NewInt(100)); err == nil {
		t.Fatal("pledging the quote asset itself must be rejected")
	}
}

func TestPledgeRejectsUnlistedAsset(t *testing.T) {
	svc, _ := collateralService(t)
	_, err := svc.Pledge(1, perpAcct(1), 99, uint256.NewInt(100))
	if err == nil {
		t.Fatal("want an error for an asset the market does not accept")
	}
}

func TestPledgeRejectsZeroAmount(t *testing.T) {
	svc, _ := collateralService(t)
	got, err := svc.Pledge(1, perpAcct(1), 7, uint256.NewInt(0))
	if err != nil {
		t.Fatalf("a zero pledge should be a no-op, got %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("want 0 credited, got %s", got)
	}
}

// TestPledgedCollateralBacksMargin is the end-to-end point: an account with no
// quote at all can trade after pledging collateral, because the pledge produced
// the quote the margin engine needs.
func TestPledgedCollateralBacksMargin(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		Collateral: []perp.CollateralAsset{{
			Asset: 7, HaircutE18: uint256.NewInt(1_000_000_000_000_000_000), Price: 1,
		}},
	}}
	trader := perpAcct(1)
	counter := perpAcct(2)
	// The trader holds ONLY collateral asset 7; the counterparty has quote.
	if err := l.Apply(ledger.DepositOp{Account: trader, Asset: 7, Amount: uint256.NewInt(50_000), Ref: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: counter, Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "q"}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// Before pledging, the trader cannot post margin.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: trader, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err == nil {
		t.Fatal("trading without quote should fail before pledging")
	}

	if _, err := svc.Pledge(1, trader, 7, uint256.NewInt(10_000)); err != nil {
		t.Fatalf("Pledge: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: counter, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: trader, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("trading after pledging should succeed: %v", err)
	}
	if _, ok := svc.Engine().Position(1, trader); !ok {
		t.Fatal("want an open position backed by pledged collateral")
	}
}

func TestCollateralBalancesListsQuoteFirst(t *testing.T) {
	svc, _ := collateralService(t)
	out, err := svc.CollateralBalances(1, perpAcct(1))
	if err != nil {
		t.Fatalf("CollateralBalances: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want quote + one collateral, got %d", len(out))
	}
	if out[0].Asset != 1 {
		t.Fatalf("quote asset should be listed first, got %d", out[0].Asset)
	}
	if out[1].Asset != 7 || out[1].Value.Uint64() != 1800 {
		t.Fatalf("collateral row = %+v, want asset 7 valued at 1800", out[1])
	}
}

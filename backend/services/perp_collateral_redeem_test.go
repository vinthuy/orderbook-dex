package services

import (
	"errors"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestRedeemIsTheInverseOfPledge pins the round trip: pledge X collateral at a
// given price and haircut, then redeem the quote it credited, and get the same X
// back. Without this the pledge was a one-way door.
func TestRedeemIsTheInverseOfPledge(t *testing.T) {
	svc, l := collateralService(t)
	acct := perpAcct(1)

	// Asset 7: price 2, haircut 90%. 1000 units credit 1800 quote.
	credited, err := svc.Pledge(1, acct, 7, uint256.NewInt(1000))
	if err != nil {
		t.Fatalf("Pledge: %v", err)
	}
	if credited.Uint64() != 1800 {
		t.Fatalf("credited %s, want 1800", credited)
	}

	got, err := svc.Redeem(1, acct, 7, credited)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.Uint64() != 1000 {
		t.Fatalf("redeemed %s units, want the original 1000", got)
	}
	if b := l.Balance(acct, 7); b.Uint64() != 1000 {
		t.Fatalf("collateral balance = %s, want the original 1000", b)
	}
}

// TestRedeemRefusesFrozenMargin is the guard that matters: quote backing an open
// position is not available to convert, or the position could not cover a loss.
func TestRedeemRefusesFrozenMargin(t *testing.T) {
	svc, _ := collateralService(t)
	acct := perpAcct(1)

	// Open a position that freezes most of the quote.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: acct, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(5000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	// The account holds 1,000,000 quote; 50,000 is now frozen.
	avail := svc.Holds().Available(acct, 1)
	tooMuch := new(uint256.Int).Add(avail, uint256.NewInt(1))
	if _, err := svc.Redeem(1, acct, 7, tooMuch); !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance above the free balance, got %v", err)
	}

	// Exactly the free balance is allowed.
	if _, err := svc.Redeem(1, acct, 7, avail); err != nil {
		t.Fatalf("redeeming the free balance should succeed: %v", err)
	}
}

// TestRedeemRefusesRoundingToZero pins that a dust redemption cannot silently
// vaporise quote.
func TestRedeemRefusesRoundingToZero(t *testing.T) {
	svc, _ := collateralService(t)
	// 1 quote at price 2, haircut 0.9 -> 1e18 / (2 * 0.9e18) = 0 collateral.
	if _, err := svc.Redeem(1, perpAcct(1), 7, uint256.NewInt(1)); err == nil {
		t.Fatal("a redemption that rounds to zero collateral must be rejected, not silently burned")
	}
}

func TestRedeemRejectsQuoteAsset(t *testing.T) {
	svc, _ := collateralService(t)
	if _, err := svc.Redeem(1, perpAcct(1), 1, uint256.NewInt(100)); err == nil {
		t.Fatal("redeeming the quote asset into itself must be rejected")
	}
}

func TestRedeemRejectsUnlistedAsset(t *testing.T) {
	svc, _ := collateralService(t)
	if _, err := svc.Redeem(1, perpAcct(1), 99, uint256.NewInt(100)); err == nil {
		t.Fatal("want an error for an asset the market does not accept")
	}
}

func TestRedeemZeroIsNoOp(t *testing.T) {
	svc, _ := collateralService(t)
	got, err := svc.Redeem(1, perpAcct(1), 7, uint256.NewInt(0))
	if err != nil {
		t.Fatalf("a zero redemption should be a no-op, got %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("want 0, got %s", got)
	}
}

// TestRedeemIsIdempotentByRef pins that a retried redemption cannot double-spend.
func TestRedeemIsIdempotentByRef(t *testing.T) {
	svc, l := collateralService(t)
	acct := perpAcct(1)

	if _, err := svc.Pledge(1, acct, 7, uint256.NewInt(1000)); err != nil {
		t.Fatal(err)
	}
	beforeQuote := new(uint256.Int).Set(l.Balance(acct, 1))

	if _, err := svc.Redeem(1, acct, 7, uint256.NewInt(900)); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	afterFirst := new(uint256.Int).Set(l.Balance(acct, 1))

	// Same amount again: identical ref, must be a no-op.
	if _, err := svc.Redeem(1, acct, 7, uint256.NewInt(900)); err != nil {
		t.Fatalf("second Redeem: %v", err)
	}
	if b := l.Balance(acct, 1); b.Cmp(afterFirst) != 0 {
		t.Fatalf("a repeated redemption spent again: %s -> %s", afterFirst, b)
	}
	if afterFirst.Cmp(beforeQuote) >= 0 {
		t.Fatalf("the first redemption should have spent quote: %s -> %s", beforeQuote, afterFirst)
	}
}

// TestRedeemAtCurrentTerms pins the honest semantics: redeeming after the haircut
// changes returns collateral at the NEW rate, because the market values the asset
// as it is now.
func TestRedeemAtCurrentTerms(t *testing.T) {
	svc, l := collateralService(t)
	acct := perpAcct(1)

	if _, err := svc.Pledge(1, acct, 7, uint256.NewInt(1000)); err != nil {
		t.Fatal(err)
	}
	// Drop the haircut to 45%: the same quote now buys twice the collateral.
	svc.mu.Lock()
	mk := svc.markets[1]
	mk.Collateral[0].HaircutE18 = uint256.NewInt(450_000_000_000_000_000)
	svc.markets[1] = mk
	svc.mu.Unlock()

	got, err := svc.Redeem(1, acct, 7, uint256.NewInt(1800))
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	// 1800 quote / (price 2 * 0.45) = 2000 units.
	if got.Uint64() != 2000 {
		t.Fatalf("redeemed %s, want 2000 at the new haircut", got)
	}
	_ = l
}

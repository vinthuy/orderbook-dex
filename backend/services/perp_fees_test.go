package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// feeService builds a one-market service with maker/taker fees configured.
func feeService(t *testing.T, makerE18, takerE18 uint64) (*PerpOrderBookService, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		FeeRates: perp.FeeRates{
			MakerE18: uint256.NewInt(makerE18),
			TakerE18: uint256.NewInt(takerE18),
		},
	}}
	for i, ref := range []string{"d1", "d2"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, l
}

// TestFeesAreChargedToBothSidesAndConserved pins that a fill charges the maker and
// the taker at their own rate and that the ledger total does not change: a fee is
// a transfer to the fee account, never a burn or a mint.
func TestFeesAreChargedToBothSidesAndConserved(t *testing.T) {
	// 0.02% maker, 0.05% taker, on notional 1000 * 100 = 100,000.
	// maker fee = floor(100000 * 2e14 / 1e18) = 20
	// taker fee = floor(100000 * 5e14 / 1e18) = 50
	svc, l := feeService(t, 200_000_000_000_000, 500_000_000_000_000)

	totalBefore := new(uint256.Int).Set(l.TotalBalance(1))

	// acct1 rests a short (becomes the maker), acct2 crosses it (the taker).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	feeAcct := svc.FeeAccount()
	got := l.Balance(feeAcct, 1)
	if got.Uint64() != 70 {
		t.Fatalf("fee account holds %s, want 70 (20 maker + 50 taker)", got)
	}

	totalAfter := l.TotalBalance(1)
	if totalAfter.Cmp(totalBefore) != 0 {
		t.Fatalf("fees changed the ledger total: before=%s after=%s", totalBefore, totalAfter)
	}
}

// TestNoFeesConfiguredChargesNothing pins that the zero-value FeeRates leaves
// behaviour exactly as before the feature.
func TestNoFeesConfiguredChargesNothing(t *testing.T) {
	svc, l := feeService(t, 0, 0)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if got := l.Balance(svc.FeeAccount(), 1); !got.IsZero() {
		t.Fatalf("want no fees, got %s", got)
	}
}

// TestFeeIsCappedByBalance pins that a fee never drives a balance negative: an
// account with nothing left pays nothing rather than accruing debt.
func TestFeeIsCappedByBalance(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		// A punitive 10% taker fee on a dust balance.
		FeeRates: perp.FeeRates{TakerE18: uint256.NewInt(100_000_000_000_000_000)},
	}}
	// Just enough to open: 1000 * 100 / 10 = 10,000 margin, no spare.
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(10_000), Ref: "d2"}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("a fee must never reject an otherwise valid order: %v", err)
	}
	if bal := l.Balance(perpAcct(2), 1); bal.Sign() < 0 {
		t.Fatalf("balance went negative: %s", bal)
	}
}

// TestFeesApplyToLiquidation pins that a forced close pays its fee too.
func TestFeesApplyToLiquidation(t *testing.T) {
	svc, l := feeService(t, 0, 500_000_000_000_000) // taker only
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	before := new(uint256.Int).Set(l.Balance(svc.FeeAccount(), 1))

	// Make acct2 liquidatable and give the liquidation a bid to hit.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 50, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	src := NewStaticPriceSource()
	src.Set(1, 50)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	after := l.Balance(svc.FeeAccount(), 1)
	if after.Cmp(before) <= 0 {
		t.Fatalf("a liquidation should have paid a fee: %s -> %s", before, after)
	}
}

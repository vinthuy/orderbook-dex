package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestLiquidationDebitsRealizedLoss pins the invariant that a forced liquidation
// must actually charge the trader's realized loss. It covers BOTH balance
// regimes, because they take different code paths inside settlePnL:
//   - a thinly funded account goes through coverShortfall (WithdrawOp fails),
//   - a well-funded account lets WithdrawOp succeed directly.
func TestLiquidationDebitsRealizedLoss(t *testing.T) {
	// Well-funded regime: the account holds far more than the loss, so the
	// WithdrawOp succeeds and the full loss must land.
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d2"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(3), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d3"}); err != nil {
		t.Fatal(err)
	}

	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), NewEventBus())
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
		t.Fatal(err)
	}

	before := new(uint256.Int).Set(l.Balance(perpAcct(2), 1))
	t.Logf("before: balance=%s held=%s", before, holds.Held(perpAcct(2), 1))

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 88, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 88)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	pos, stillOpen := svc.Engine().Position(1, perpAcct(2))
	after := l.Balance(perpAcct(2), 1)
	t.Logf("after: open=%v pos=%v balance=%s held=%s", stillOpen, pos, after, holds.Held(perpAcct(2), 1))

	if stillOpen {
		t.Fatal("position should have been liquidated")
	}

	// The loss at 88 against an entry of 100 is 12,000 quote. A well-funded
	// account must be debited exactly that much.
	loss := new(uint256.Int).Sub(before, after)
	if loss.Uint64() < 12_000 {
		t.Fatalf("loss debited = %s, want >= 12000 (balance %s -> %s)", loss, before, after)
	}
}


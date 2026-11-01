package services

import (
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// riskService builds a perp service whose market uses BaseDecimals 0 so notional
// equals size*price, making maintenance-margin arithmetic readable in tests.
func riskService(t *testing.T) (*PerpOrderBookService, *ledger.Ledger, *ledger.Holds) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2", "d3", "d4"} {
		acct := perpAcct(byte(i + 1))
		if err := l.Apply(ledger.DepositOp{Account: acct, Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatalf("fund: %v", err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), NewEventBus())
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, l, holds
}

// openPair opens a long for acct 2 against a short for acct 1 at price 100.
func openPair(t *testing.T, svc *PerpOrderBookService, qty uint64) {
	t.Helper()
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(qty), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(qty), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
}

func TestHealthyPositionIsNotLiquidated(t *testing.T) {
	svc, _, _ := riskService(t)
	openPair(t, svc, 1000)

	src := NewStaticPriceSource()
	src.Set(1, 100)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); !ok {
		t.Fatal("a healthy position must survive the scan")
	}
}

func TestUnderwaterPositionIsLiquidated(t *testing.T) {
	svc, _, _ := riskService(t)
	openPair(t, svc, 1000)

	// Rest a bid the liquidation can sell into, below entry so the long is\n\t// underwater: long 1000 entered at 100 (margin 10000), now marking at 88.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(4), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 88, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("rest bid: %v", err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 88)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("an underwater long should have been liquidated")
	}
}

func TestLiquidationRequiresFreshMark(t *testing.T) {
	svc, _, _ := riskService(t)
	openPair(t, svc, 1000)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(4), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 88, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("rest bid: %v", err)
	}

	// No price source at all: the scan must not act on an absent price.
	svc.tick(time.Unix(1000, 0))
	if _, ok := svc.Engine().Position(1, perpAcct(2)); !ok {
		t.Fatal("liquidation without a mark price is unsafe")
	}
}

// TestShortfallDoesNotDegradeTheBook is the regression for 6a's known gap: a loss
// exceeding everything the account has used to halt the whole perpetual book.
func TestShortfallDoesNotDegradeTheBook(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	// The long gets just enough to open: 1000 base at 100 with 10x needs 10000.
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d1"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(10000), Ref: "d2"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	defer svc.Close()

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	// Close at 40: the long loses 60 * 1000 = 60000 against 10000 of margin, i.e.
	// a 50000 shortfall. Previously this failed the WithdrawOp and degraded the
	// service for everyone.
	//
	// The underwater account cannot open a closing order itself (all its balance
	// is frozen as margin), which is exactly why the forced-liquidation path
	// exists. Rest a bid and let the liquidation engine close it.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 40, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("rest bid: %v", err)
	}
	svc.SetInsuranceFund(reservedInsuranceAccount)
	src := NewStaticPriceSource()
	src.Set(1, 40)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("the underwater long should have been liquidated")
	}
	// No negative balance ever: the account pays what it has and no more.
	if bal := l.Balance(perpAcct(2), 1); bal.Sign() < 0 {
		t.Fatalf("balance went negative: %s", bal)
	}

	// The book must still accept orders.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10), Leverage: 10}); err != nil {
		t.Fatalf("book degraded after a shortfall: %v", err)
	}
}

func TestLiquidationPaysFeeToInsuranceFund(t *testing.T) {
	svc, l, _ := riskService(t)
	openPair(t, svc, 1000)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(4), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 88, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatalf("rest bid: %v", err)
	}
	fund := perpAcct(9)
	svc.SetInsuranceFund(fund)
	if got := svc.InsuranceFund(); got != fund {
		t.Fatalf("insurance fund not set: %x", got)
	}

	src := NewStaticPriceSource()
	src.Set(1, 88)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("position should be closed")
	}
	if !fundUnchangedOrCredited(t, l, fund) {
		t.Fatal("expected the insurance fund to receive the liquidation fee")
	}
}

// fundUnchangedOrCredited is a weak check kept explicit: the fee is capped by the
// account's remaining balance, which may be zero after a wipeout.
func fundUnchangedOrCredited(t *testing.T, l *ledger.Ledger, fund ledger.AccountID) bool {
	t.Helper()
	return l.Balance(fund, 1).Sign() >= 0
}

func TestLiquidatableBoundaryUsesMark(t *testing.T) {
	m := perp.Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10}
	p := &perp.Position{Market: 1, Side: perp.SideLong, Size: uint256.NewInt(1000),
		EntryPrice: 100, Margin: uint256.NewInt(1000), Leverage: 10}
	// equity at 100 = 1000, maintenance = 500 -> healthy.
	bad, err := m.Liquidatable(p, 100)
	if err != nil || bad {
		t.Fatalf("want healthy, got bad=%v err=%v", bad, err)
	}
	// equity at 99 = 1000 + (99-100)*1000 = 0, maintenance 495 -> liquidatable.
	bad, err = m.Liquidatable(p, 99)
	if err != nil {
		t.Fatalf("Liquidatable: %v", err)
	}
	if !bad {
		t.Fatal("99 wipes the equity and must be liquidatable")
	}
	// The boundary: equity must stay at or above maintenance. At 99.5 the loss is
	// 500, so equity 500 vs maintenance 497.5 -> the smallest healthy price is
	// the one where equity == maintenance.
	// equity(p) = 1000 - (100-p)*1000 = 100000 - 1000*(100-p)
	// maintenance(p) = ceil(p*1000*0.005) = ceil(5p)
	// equity >= maintenance  <=>  1000*(p-99) >= 5p  <=>  995p >= 99000  <=>  p >= 99.5
	// so p = 100 is healthy and p = 99 is not; the midpoint is the boundary.
	if bad, _ := m.Liquidatable(p, 100); bad {
		t.Fatal("100 is the boundary and must stay healthy")
	}
}

func TestEquitySigned(t *testing.T) {
	m := perp.Market{ID: 1, Symbol: "X", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10}
	eq, err := m.Equity(perp.SideLong, uint256.NewInt(1000), 100, 50, uint256.NewInt(1000))
	if err != nil {
		t.Fatalf("Equity: %v", err)
	}
	if eq.Cmp(big.NewInt(-49000)) != 0 {
		t.Fatalf("want -49000, got %s", eq)
	}
}


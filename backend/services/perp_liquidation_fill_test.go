package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestUnfillableLiquidationChargesNothing pins that a position below maintenance
// on an empty book is not charged, is not journaled, and does not emit a
// liquidation event on every scan tick.
func TestUnfillableLiquidationChargesNothing(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}

	liquidations := 0
	bus.Subscribe(TopicPerpLiquidation, func(Event) error {
		liquidations++
		return nil
	})

	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), bus)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	fund := perpAcct(9)
	svc.SetInsuranceFund(fund)

	// acct1 shorts, acct2 goes long 1000 @ 100. No bids remain to sell into.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 80) // deep below maintenance, so the position is liquidatable
	svc.SetPriceSource(src, PriceOptions{})

	for i := 0; i < 4; i++ {
		svc.tick(time.Unix(int64(1000+i), 0))
	}

	if liquidations != 0 {
		t.Fatalf("want no liquidation events for an unfillable position, got %d", liquidations)
	}
	if bal := l.Balance(fund, 1); !bal.IsZero() {
		t.Fatalf("insurance fund was charged %s for a liquidation that never filled", bal)
	}
	if bal := l.Balance(perpAcct(2), 1); bal.Uint64() != 1_000_000 {
		t.Fatalf("account balance changed (%s) despite no fill", bal)
	}
}

// TestLiquidationReportsClosedSizeNotRequested pins that the event reports what
// actually closed.
func TestLiquidationReportsClosedSizeNotRequested(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2", "d3"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}

	var got []LiquidationEvent
	bus.Subscribe(TopicPerpLiquidation, func(e Event) error {
		if ev, ok := e.Data.(LiquidationEvent); ok {
			got = append(got, ev)
		}
		return nil
	})

	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, t.TempDir()), bus)
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
	// Only 400 of the 1000 can be sold: the book holds a 400-unit bid.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 80, Qty: uint256.NewInt(400), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	src := NewStaticPriceSource()
	src.Set(1, 80)
	svc.SetPriceSource(src, PriceOptions{})
	svc.tick(time.Unix(1000, 0))

	if len(got) == 0 {
		t.Fatal("want a liquidation event")
	}
	ev := got[len(got)-1]
	if ev.Size == nil || ev.Size.Uint64() != 400 {
		t.Fatalf("event size = %v, want the 400 actually closed", ev.Size)
	}
	if !ev.Partial {
		t.Fatal("a partially filled liquidation must report Partial")
	}
	if _, stillOpen := svc.Engine().Position(1, perpAcct(2)); !stillOpen {
		t.Fatal("600 units should remain open")
	}
}

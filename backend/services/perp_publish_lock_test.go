package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestPublishDoesNotHoldLock checks whether an event subscriber that calls back
// into the service deadlocks. EventBus.Publish is synchronous, so publishing while
// s.mu is held makes any subscriber that reads service state a self-deadlock —
// a permanent DoS of the matcher goroutine.
func TestPublishDoesNotHoldLock(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(1_000_000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}

	// A subscriber that reads service state — exactly what an operator alert or a
	// richer WebSocket hub would do.
	reentered := make(chan struct{}, 1)
	var svc *PerpOrderBookService
	bus.Subscribe(TopicPerpPosition, func(Event) error {
		// Any service method takes s.mu.
		_, _ = svc.MarkPrice(perp.MarketID(1))
		select {
		case reentered <- struct{}{}:
		default:
		}
		return nil
	})

	var err error
	svc, err = NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), bus)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	done := make(chan error, 1)
	go func() {
		_, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
			Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10})
		done <- err
	}()

	select {
	case <-done:
		// Returned: publishing does not hold the lock. Correct.
	case <-time.After(3 * time.Second):
		t.Fatal("Place deadlocked: a subscriber that reads service state blocks the matcher")
	}

	select {
	case <-reentered:
	case <-time.After(time.Second):
		t.Fatal("subscriber never ran")
	}
}


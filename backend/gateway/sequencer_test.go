package gateway

import (
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

func testOrderBook(t *testing.T) (*services.OrderBookService, *ledger.Ledger, *services.EventBus) {
	t.Helper()
	markets := []matching.Market{
		{ID: 1, Symbol: "ETH/USDT", Base: 2, Quote: 1, BaseDecimals: 2},
	}
	l := ledger.New()
	holds := ledger.NewHolds(l)
	journal, err := services.NewJournal("")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	bus := services.NewEventBus()
	ob, err := services.NewOrderBookService(markets, l, holds, journal, bus)
	if err != nil {
		t.Fatalf("order book: %v", err)
	}
	t.Cleanup(func() { _ = ob.Stop() })
	return ob, l, bus
}

func TestSequencerAssignsStrictlyIncreasingIDs(t *testing.T) {
	ob, l, _ := testOrderBook(t)
	// Fund quote (asset 1) so the buy can freeze it.
	if err := l.Apply(ledger.DepositOp{Account: acct(1), Asset: 1, Amount: uint256.NewInt(1000), Ref: "dep1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	s := NewSequencer(ob)

	req := matching.PlaceRequest{
		Market: 1, Account: acct(1), Side: matching.SideBuy,
		Type: matching.TypeLimit, TIF: matching.TIFGTC, Price: 1, Qty: uint256.NewInt(1),
	}
	_, id1, err := s.Place(req)
	if err != nil {
		t.Fatalf("place 1: %v", err)
	}
	_, id2, err := s.Place(req)
	if err != nil {
		t.Fatalf("place 2: %v", err)
	}
	if id2 != id1+1 {
		t.Fatalf("request ids %d -> %d, want +1", id1, id2)
	}
}

func TestSequencerSerializesConcurrentPlaces(t *testing.T) {
	ob, l, _ := testOrderBook(t)
	if err := l.Apply(ledger.DepositOp{Account: acct(2), Asset: 1, Amount: uint256.NewInt(1000), Ref: "dep2"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	s := NewSequencer(ob)
	req := matching.PlaceRequest{
		Market: 1, Account: acct(2), Side: matching.SideBuy,
		Type: matching.TypeLimit, TIF: matching.TIFGTC, Price: 1, Qty: uint256.NewInt(1),
	}

	ids := make([]uint64, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, id, err := s.Place(req)
			if err != nil {
				t.Errorf("place: %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for _, id := range ids {
		if id == 0 {
			t.Fatal("a request id was 0 (place failed)")
		}
		if seen[id] {
			t.Fatalf("duplicate request id %d", id)
		}
		seen[id] = true
	}
}

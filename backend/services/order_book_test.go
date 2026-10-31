package services

import (
	"errors"
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
)

// --- helpers ---------------------------------------------------------------

func obMarkets() []matching.Market {
	return []matching.Market{
		{ID: 1, Symbol: "ETH/USDT", Base: 2, Quote: 1, BaseDecimals: 2},
	}
}

func obAcct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func mustDeposit(t *testing.T, l *ledger.Ledger, account ledger.AccountID, asset ledger.AssetID, amount uint64) {
	t.Helper()
	ref := "deposit:" + account.Hex() + ":" + uint256.NewInt(uint64(asset)).Dec()
	err := l.Apply(ledger.DepositOp{Account: account, Asset: asset, Amount: uint256.NewInt(amount), Ref: ref})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
}

func mustJournal(t *testing.T, dir string) *Journal {
	t.Helper()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	// Release the WAL handle before t.TempDir's RemoveAll runs (Windows refuses
	// to unlink an open file).
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func mustNewOrderBook(t *testing.T, markets []matching.Market, l *ledger.Ledger, holds *ledger.Holds, journal *Journal, bus *EventBus) *OrderBookService {
	t.Helper()
	svc, err := NewOrderBookService(markets, l, holds, journal, bus)
	if err != nil {
		t.Fatalf("NewOrderBookService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	return svc
}

func limitReq(account ledger.AccountID, side matching.Side, price uint64, qty uint64) matching.PlaceRequest {
	return matching.PlaceRequest{
		Market:  1,
		Account: account,
		Side:    side,
		Type:    matching.TypeLimit,
		TIF:     matching.TIFGTC,
		Price:   matching.Price(price),
		Qty:     uint256.NewInt(qty),
	}
}

// toggleStore is a ledger.Store whose WAL writes can be made to fail on demand.
// It models the "ledger WAL write error" that must drive the order book into
// fatal degradation (spec §6.5), exercising the real ledger.Apply rollback path.
type toggleStore struct {
	mu      sync.Mutex
	fail    bool
	entries []ledger.WALEntry
}

func (s *toggleStore) setFail(f bool) {
	s.mu.Lock()
	s.fail = f
	s.mu.Unlock()
}

func (s *toggleStore) AppendBatch(e []ledger.WALEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("wal write failed")
	}
	s.entries = append(s.entries, e...)
	return nil
}

func (s *toggleStore) SaveSnapshot([]byte) error                { return nil }
func (s *toggleStore) LoadSnapshot() ([]byte, error)            { return nil, nil }
func (s *toggleStore) Replay(uint64) ([]ledger.WALEntry, error) { return nil, nil }
func (s *toggleStore) Close() error                             { return nil }

// --- Task 9: end-to-end matching + settlement ------------------------------

func TestOrderBookEndToEndTrade(t *testing.T) {
	l := ledger.New()
	seller, buyer := obAcct(1), obAcct(2)
	mustDeposit(t, l, seller, 2, 10) // base
	mustDeposit(t, l, buyer, 1, 100) // quote
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	// Resting sell: freezes 10 base, moves nothing.
	res, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10))
	if err != nil {
		t.Fatalf("place sell: %v", err)
	}
	if res.Order.Status != matching.StatusNew {
		t.Fatalf("resting sell status = %s, want new", res.Order.Status)
	}
	if len(res.Trades) != 0 {
		t.Fatalf("resting sell produced %d trades", len(res.Trades))
	}
	if got := holds.Held(seller, 2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("seller base held = %s, want 10", got)
	}
	if got := holds.Available(seller, 2); got.Sign() != 0 {
		t.Fatalf("seller base available = %s, want 0", got)
	}

	// Matching buy.
	res, err = svc.Place(limitReq(buyer, matching.SideBuy, 300, 10))
	if err != nil {
		t.Fatalf("place buy: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	tr := res.Trades[0]
	if tr.Qty.Cmp(uint256.NewInt(10)) != 0 || tr.Notional.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("trade qty/notional = %s/%s, want 10/30", tr.Qty, tr.Notional)
	}
	if tr.Maker != seller || tr.Taker != buyer || tr.MakerSide != matching.SideSell {
		t.Fatalf("trade parties wrong: maker=%v taker=%v side=%v", tr.Maker, tr.Taker, tr.MakerSide)
	}
	if res.Order.Status != matching.StatusFilled {
		t.Fatalf("taker status = %s, want filled", res.Order.Status)
	}

	// Balances: the two zero-sum legs moved base seller->buyer and quote
	// buyer->seller.
	if got := l.Balance(seller, 2); got.Sign() != 0 {
		t.Fatalf("seller base = %s, want 0", got)
	}
	if got := l.Balance(seller, 1); got.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("seller quote = %s, want 30", got)
	}
	if got := l.Balance(buyer, 2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("buyer base = %s, want 10", got)
	}
	if got := l.Balance(buyer, 1); got.Cmp(uint256.NewInt(70)) != 0 {
		t.Fatalf("buyer quote = %s, want 70", got)
	}

	// Every freeze is released: both orders fully filled, no residual.
	for _, a := range []ledger.AccountID{seller, buyer} {
		for _, asset := range []ledger.AssetID{1, 2} {
			if got := holds.Held(a, asset); got.Sign() != 0 {
				t.Fatalf("hold (%v,%d) = %s, want 0", a, asset, got)
			}
		}
	}

	// Conservation.
	if got := l.TotalBalance(2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("total base = %s, want 10", got)
	}
	if got := l.TotalBalance(1); got.Cmp(uint256.NewInt(100)) != 0 {
		t.Fatalf("total quote = %s, want 100", got)
	}

	// Book emptied.
	bids, asks := svc.Engine().Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("book not empty after a full cross: bids=%v asks=%v", bids, asks)
	}
}

// TestOrderBookAvailableNotInflated pins that a fill's available balances change
// only by the fill itself: the freeze is released, never double-counted. If the
// ledger settle and the freeze release were replaced by a wrong encoding (or a
// double release/debit), these exact values move.
func TestOrderBookAvailableNotInflated(t *testing.T) {
	l := ledger.New()
	seller, buyer := obAcct(1), obAcct(2)
	mustDeposit(t, l, seller, 2, 10)
	mustDeposit(t, l, buyer, 1, 100)
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	if _, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10)); err != nil {
		t.Fatalf("place sell: %v", err)
	}
	if _, err := svc.Place(limitReq(buyer, matching.SideBuy, 300, 10)); err != nil {
		t.Fatalf("place buy: %v", err)
	}

	if got := holds.Available(seller, 2); got.Sign() != 0 {
		t.Fatalf("seller base available = %s, want 0", got)
	}
	if got := holds.Available(seller, 1); got.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("seller quote available = %s, want 30", got)
	}
	if got := holds.Available(buyer, 2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("buyer base available = %s, want 10", got)
	}
	if got := holds.Available(buyer, 1); got.Cmp(uint256.NewInt(70)) != 0 {
		t.Fatalf("buyer quote available = %s, want 70", got)
	}
}

// TestOrderBookSettlesBeforeReleasingFreeze pins the ordering guarantee that the
// sequential end-state tests cannot see: at the seam between "ledger settled"
// and "freeze released" the available balance must be UNDERSTATED, never
// inflated. Reversing the two steps would leave availability temporarily higher
// than its final value, which is the window in which a concurrent withdrawal
// could take funds a transfer is about to spend.
func TestOrderBookSettlesBeforeReleasingFreeze(t *testing.T) {
	l := ledger.New()
	seller, buyer := obAcct(1), obAcct(2)
	mustDeposit(t, l, seller, 2, 100) // base
	mustDeposit(t, l, buyer, 1, 1000) // quote
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	// Maker rests a sell of 10 base at 300: freeze 10 base, available 90.
	if _, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10)); err != nil {
		t.Fatalf("place sell: %v", err)
	}

	var (
		midSellerBase, midSellerBaseBal *uint256.Int
		midBuyerQuote, midBuyerQuoteBal *uint256.Int
		seamCalls                       int
	)
	svc.beforeRelease = func() {
		seamCalls++
		midSellerBase = holds.Available(seller, 2)
		midSellerBaseBal = l.Balance(seller, 2)
		midBuyerQuote = holds.Available(buyer, 1)
		midBuyerQuoteBal = l.Balance(buyer, 1)
	}

	// Taker buys 10 at 300: notional = floor(10*300/100) = 30 quote.
	if _, err := svc.Place(limitReq(buyer, matching.SideBuy, 300, 10)); err != nil {
		t.Fatalf("place buy: %v", err)
	}
	if seamCalls != 1 {
		t.Fatalf("seam observed %d times, want 1", seamCalls)
	}

	finalSellerBase := holds.Available(seller, 2)
	finalBuyerQuote := holds.Available(buyer, 1)

	// The ledger side is already done at the seam...
	if midSellerBaseBal.Cmp(uint256.NewInt(90)) != 0 {
		t.Fatalf("mid seller base balance = %s, want 90 (transfer applied)", midSellerBaseBal)
	}
	if midBuyerQuoteBal.Cmp(uint256.NewInt(970)) != 0 {
		t.Fatalf("mid buyer quote balance = %s, want 970 (transfer applied)", midBuyerQuoteBal)
	}
	// ...while the freeze has not yet been returned, so availability is exactly
	// one fill short of its final value. Asserting `<=` and the exact shortfall
	// is what catches a reversed order.
	if midSellerBase.Cmp(finalSellerBase) >= 0 {
		t.Fatalf("mid seller base available = %s, final = %s: seam must understate, not match or exceed",
			midSellerBase, finalSellerBase)
	}
	if got := new(uint256.Int).Add(midSellerBase, uint256.NewInt(10)); got.Cmp(finalSellerBase) != 0 {
		t.Fatalf("mid seller base available %s + 10 != final %s", midSellerBase, finalSellerBase)
	}
	if midBuyerQuote.Cmp(finalBuyerQuote) >= 0 {
		t.Fatalf("mid buyer quote available = %s, final = %s: seam must understate",
			midBuyerQuote, finalBuyerQuote)
	}
	if got := new(uint256.Int).Add(midBuyerQuote, uint256.NewInt(30)); got.Cmp(finalBuyerQuote) != 0 {
		t.Fatalf("mid buyer quote available %s + 30 != final %s", midBuyerQuote, finalBuyerQuote)
	}

	if finalSellerBase.Cmp(uint256.NewInt(90)) != 0 {
		t.Fatalf("final seller base available = %s, want 90", finalSellerBase)
	}
	if finalBuyerQuote.Cmp(uint256.NewInt(970)) != 0 {
		t.Fatalf("final buyer quote available = %s, want 970", finalBuyerQuote)
	}
}

func TestOrderBookRejectsWithoutFunds(t *testing.T) {
	l := ledger.New()
	seller := obAcct(1)
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	_, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10))
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	bids, asks := svc.Engine().Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("book must be untouched on a rejected order")
	}
	if got := holds.Held(seller, 2); got.Sign() != 0 {
		t.Fatalf("no freeze may survive a rejected order, held = %s", got)
	}
}

func TestOrderBookCancelReleases(t *testing.T) {
	l := ledger.New()
	seller := obAcct(1)
	mustDeposit(t, l, seller, 2, 10)
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	res, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if got := holds.Available(seller, 2); got.Sign() != 0 {
		t.Fatalf("available while resting = %s, want 0", got)
	}

	cres, err := svc.Cancel(1, seller, res.Order.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cres.Order.Status != matching.StatusCancelled {
		t.Fatalf("cancelled order status = %s", cres.Order.Status)
	}
	if got := holds.Held(seller, 2); got.Sign() != 0 {
		t.Fatalf("held after cancel = %s, want 0", got)
	}
	if got := holds.Available(seller, 2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("available after cancel = %s, want 10", got)
	}
	if _, asks := svc.Engine().Book(1, 5); len(asks) != 0 {
		t.Fatalf("ask level survived cancel: %v", asks)
	}
}

func TestOrderBookCancelsOthersOrderRejected(t *testing.T) {
	l := ledger.New()
	seller, attacker := obAcct(1), obAcct(3)
	mustDeposit(t, l, seller, 2, 10)
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	res, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if _, err := svc.Cancel(1, attacker, res.Order.ID); !errors.Is(err, matching.ErrNotOwner) {
		t.Fatalf("cancel by non-owner = %v, want ErrNotOwner", err)
	}
}

func TestOrderBookPublishesEvents(t *testing.T) {
	l := ledger.New()
	seller, buyer := obAcct(1), obAcct(2)
	mustDeposit(t, l, seller, 2, 10)
	mustDeposit(t, l, buyer, 1, 100)
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	var orderEvents, tradeEvents int
	var lastTrade matching.Trade
	bus.Subscribe(TopicOrderBookOrder, func(e Event) error { orderEvents++; return nil })
	bus.Subscribe(TopicOrderBookTrade, func(e Event) error {
		tradeEvents++
		te, ok := e.Data.(TradeEvent)
		if !ok {
			t.Errorf("trade event data = %T, want TradeEvent", e.Data)
			return nil
		}
		lastTrade = te.Trade
		return nil
	})

	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), bus)
	if _, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10)); err != nil {
		t.Fatalf("place sell: %v", err)
	}
	if _, err := svc.Place(limitReq(buyer, matching.SideBuy, 300, 10)); err != nil {
		t.Fatalf("place buy: %v", err)
	}

	if orderEvents != 2 {
		t.Fatalf("order events = %d, want 2", orderEvents)
	}
	if tradeEvents != 1 {
		t.Fatalf("trade events = %d, want 1", tradeEvents)
	}
	if lastTrade.Notional.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("published trade notional = %s, want 30", lastTrade.Notional)
	}
}

// TestOrderBookLifecycleRestart proves Start/Stop are idempotent and a full
// Start->Stop->Start->Stop cycle does not panic (the runner contract).
func TestOrderBookLifecycleRestart(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	if err := svc.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if loops := svc.run.activeLoops(); loops != 1 {
		t.Fatalf("active loops after double Start = %d, want 1", loops)
	}
	if err := svc.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := svc.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err := svc.Stop(); err != nil {
		t.Fatalf("final stop: %v", err)
	}
}

// --- Task 10: crash replay, no double settle, fatal degradation ------------

// TestOrderBookCrashReplayHeals models "instructions on disk, ledger not
// settled": a ledger WAL write fails mid-trade, the service degrades, and a
// fresh service over the same journal catches the settlement up and rebuilds
// the book and freeze state.
func TestOrderBookCrashReplayHeals(t *testing.T) {
	dir := t.TempDir()
	seller, buyer := obAcct(1), obAcct(2)

	store := &toggleStore{}
	l1 := ledger.New().WithStore(store)
	mustDeposit(t, l1, seller, 2, 10)
	mustDeposit(t, l1, buyer, 1, 100)
	holds1 := ledger.NewHolds(l1)
	svc1 := mustNewOrderBook(t, obMarkets(), l1, holds1, mustJournal(t, dir), NewEventBus())

	if _, err := svc1.Place(limitReq(seller, matching.SideSell, 300, 10)); err != nil {
		t.Fatalf("place sell: %v", err)
	}

	// Crash the ledger write for the matching fill.
	store.setFail(true)
	if _, err := svc1.Place(limitReq(buyer, matching.SideBuy, 300, 10)); !errors.Is(err, ErrServiceDegraded) {
		t.Fatalf("matching place under a failing WAL = %v, want ErrServiceDegraded", err)
	}
	// The ledger must be untouched: the fill never settled.
	if got := l1.Balance(buyer, 1); got.Cmp(uint256.NewInt(100)) != 0 {
		t.Fatalf("buyer quote after the crash = %s, want 100 (unsettled)", got)
	}
	if got := l1.Balance(seller, 2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("seller base after the crash = %s, want 10 (unsettled)", got)
	}

	// Restart: fresh ledger with the same deposits, same journal directory.
	l2 := ledger.New()
	mustDeposit(t, l2, seller, 2, 10)
	mustDeposit(t, l2, buyer, 1, 100)
	holds2 := ledger.NewHolds(l2)
	svc2 := mustNewOrderBook(t, obMarkets(), l2, holds2, mustJournal(t, dir), NewEventBus())

	if got := l2.Balance(seller, 2); got.Sign() != 0 {
		t.Fatalf("healed seller base = %s, want 0", got)
	}
	if got := l2.Balance(seller, 1); got.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("healed seller quote = %s, want 30", got)
	}
	if got := l2.Balance(buyer, 2); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("healed buyer base = %s, want 10", got)
	}
	if got := l2.Balance(buyer, 1); got.Cmp(uint256.NewInt(70)) != 0 {
		t.Fatalf("healed buyer quote = %s, want 70", got)
	}
	// Book and freeze state match the healthy end state: fully crossed, no
	// residuals.
	bids, asks := svc2.Engine().Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("healed book not empty: bids=%v asks=%v", bids, asks)
	}
	if len(svc2.Engine().OpenReserves()) != 0 {
		t.Fatalf("healed reserves = %v, want none", svc2.Engine().OpenReserves())
	}
	for _, a := range []ledger.AccountID{seller, buyer} {
		for _, asset := range []ledger.AssetID{1, 2} {
			if got := holds2.Held(a, asset); got.Sign() != 0 {
				t.Fatalf("healed hold (%v,%d) = %s, want 0", a, asset, got)
			}
		}
	}
}

// TestOrderBookReplayDoesNotDoubleSettle replays a complete journal twice: the
// ledger must not move a second time (idempotency is keyed by trade id).
func TestOrderBookReplayDoesNotDoubleSettle(t *testing.T) {
	dir := t.TempDir()
	seller, buyer := obAcct(1), obAcct(2)

	// Produce a complete journal through a healthy service (all settled).
	l0 := ledger.New()
	mustDeposit(t, l0, seller, 2, 10)
	mustDeposit(t, l0, buyer, 1, 100)
	svc0 := mustNewOrderBook(t, obMarkets(), l0, ledger.NewHolds(l0), mustJournal(t, dir), NewEventBus())
	if _, err := svc0.Place(limitReq(seller, matching.SideSell, 300, 10)); err != nil {
		t.Fatalf("place sell: %v", err)
	}
	if _, err := svc0.Place(limitReq(buyer, matching.SideBuy, 300, 10)); err != nil {
		t.Fatalf("place buy: %v", err)
	}

	// Replay the same journal into a fresh ledger twice.
	l := ledger.New()
	mustDeposit(t, l, seller, 2, 10)
	mustDeposit(t, l, buyer, 1, 100)
	for i := 0; i < 2; i++ {
		holds := ledger.NewHolds(l)
		svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, dir), NewEventBus())
		if got := l.Balance(seller, 1); got.Cmp(uint256.NewInt(30)) != 0 {
			t.Fatalf("pass %d: seller quote = %s, want 30", i, got)
		}
		if got := l.Balance(buyer, 1); got.Cmp(uint256.NewInt(70)) != 0 {
			t.Fatalf("pass %d: buyer quote = %s, want 70", i, got)
		}
		if got := l.TotalBalance(2); got.Cmp(uint256.NewInt(10)) != 0 {
			t.Fatalf("pass %d: total base = %s, want 10", i, got)
		}
		_ = svc
	}
}

func TestOrderBookFatalDegradation(t *testing.T) {
	store := &toggleStore{}
	l := ledger.New().WithStore(store)
	seller, buyer := obAcct(1), obAcct(2)
	mustDeposit(t, l, seller, 2, 10)
	mustDeposit(t, l, buyer, 1, 100)
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	var degraded int
	bus.Subscribe(TopicOrderBookDegraded, func(Event) error { degraded++; return nil })

	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), bus)
	if _, err := svc.Place(limitReq(seller, matching.SideSell, 300, 10)); err != nil {
		t.Fatalf("place sell: %v", err)
	}

	store.setFail(true)
	if _, err := svc.Place(limitReq(buyer, matching.SideBuy, 300, 10)); !errors.Is(err, ErrServiceDegraded) {
		t.Fatalf("the failing place = %v, want ErrServiceDegraded", err)
	}
	if degraded == 0 {
		t.Fatal("a degraded event must be published")
	}

	// Every later instruction is refused until restart.
	if _, err := svc.Place(limitReq(seller, matching.SideSell, 300, 1)); !errors.Is(err, ErrServiceDegraded) {
		t.Fatalf("place after degradation = %v, want ErrServiceDegraded", err)
	}
	if _, err := svc.Cancel(1, seller, 1); !errors.Is(err, ErrServiceDegraded) {
		t.Fatalf("cancel after degradation = %v, want ErrServiceDegraded", err)
	}
}

func TestRecoverReemitsTradeEvents(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	journal := mustJournal(t, dir)
	bus := NewEventBus()

	// Fund two accounts for a crossing pair.
	deposit := func(a ledger.AccountID, asset ledger.AssetID, n uint64, ref string) {
		t.Helper()
		if err := l.Apply(ledger.DepositOp{Account: a, Asset: asset, Amount: uint256.NewInt(n), Ref: ref}); err != nil {
			t.Fatalf("deposit: %v", err)
		}
	}
	seller := obAcct(1)
	buyer := obAcct(2)
	deposit(seller, 2, 100, "d1") // base asset 2
	deposit(buyer, 1, 100, "d2")  // quote asset 1

	ob := mustNewOrderBook(t, obMarkets(), l, holds, journal, bus)
	defer ob.Close()

	// Place a sell then a crossing buy so a trade occurs and is journaled.
	if _, err := ob.Place(limitReq(seller, matching.SideSell, 10, 5)); err != nil {
		t.Fatalf("place sell: %v", err)
	}
	if _, err := ob.Place(limitReq(buyer, matching.SideBuy, 10, 5)); err != nil {
		t.Fatalf("place buy: %v", err)
	}

	// Count trade events on a fresh bus while a fresh service replays the WAL.
	count := 0
	bus2 := NewEventBus()
	bus2.Subscribe(TopicOrderBookTrade, func(e Event) error {
		count++
		return nil
	})

	ob2 := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, dir), bus2)
	defer ob2.Close()
	_ = ob2 // recovery runs in the constructor

	if count == 0 {
		t.Fatal("recover should re-emit the journaled trade event")
	}
}

// TestSettlementWithdrawCannotTouchFrozenFunds is the fund-safety test for the
// freeze/withdrawal boundary: a resting order's freeze is not withdrawable, and
// only the available remainder can leave.
func TestSettlementWithdrawCannotTouchFrozenFunds(t *testing.T) {
	l := ledger.New()
	acct := obAcct(9)
	mustDeposit(t, l, acct, 1, 100) // quote
	holds := ledger.NewHolds(l)
	svc := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, ""), NewEventBus())

	// A resting buy freezes ceil(10*300/100) = 30 quote.
	if _, err := svc.Place(limitReq(acct, matching.SideBuy, 300, 10)); err != nil {
		t.Fatalf("place buy: %v", err)
	}
	if got := holds.Held(acct, 1); got.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("held quote = %s, want 30", got)
	}
	if got := holds.Available(acct, 1); got.Cmp(uint256.NewInt(70)) != 0 {
		t.Fatalf("available quote = %s, want 70", got)
	}

	adapter := NewMockBlockchainAdapter()
	settlement, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}
	settlement.SetHoldProvider(holds)

	// Withdrawing the whole ledger balance must fail: 30 of it is frozen.
	_, err = settlement.Withdraw(acct, 1, uint256.NewInt(100), uint256.NewInt(100), 1, "w-frozen")
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("withdrawing frozen funds = %v, want ErrInsufficientBalance", err)
	}
	if len(adapter.Sent) != 0 {
		t.Fatalf("a refused withdrawal must broadcast nothing, sent = %d", len(adapter.Sent))
	}
	if got := l.Balance(acct, 1); got.Cmp(uint256.NewInt(100)) != 0 {
		t.Fatalf("a refused withdrawal must not debit, balance = %s", got)
	}

	// The available 70 may leave; the 30 freeze stays put.
	if _, err := settlement.Withdraw(acct, 1, uint256.NewInt(100), uint256.NewInt(70), 1, "w-avail"); err != nil {
		t.Fatalf("withdraw available: %v", err)
	}
	if got := l.Balance(acct, 1); got.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("balance after withdrawal = %s, want 30", got)
	}
	if got := holds.Held(acct, 1); got.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("freeze after withdrawal = %s, want 30 (untouched)", got)
	}
}

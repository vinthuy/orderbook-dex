package matching

import (
	"errors"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// --- helpers ---

func limitReq(market MarketID, acct ledger.AccountID, side Side, tif TIF, price Price, qty uint64) PlaceRequest {
	return PlaceRequest{Market: market, Account: acct, Side: side, Type: TypeLimit, TIF: tif, Price: price, Qty: amt(qty)}
}

func marketBuyReq(market MarketID, acct ledger.AccountID, qty, maxNotional uint64) PlaceRequest {
	return PlaceRequest{Market: market, Account: acct, Side: SideBuy, Type: TypeMarket, TIF: TIFIOC, Qty: amt(qty), MaxNotional: amt(maxNotional)}
}

func marketSellReq(market MarketID, acct ledger.AccountID, qty uint64) PlaceRequest {
	return PlaceRequest{Market: market, Account: acct, Side: SideSell, Type: TypeMarket, TIF: TIFIOC, Qty: amt(qty)}
}

func placeOK(t *testing.T, e *Engine, req PlaceRequest) Result {
	t.Helper()
	res, err := e.Place(req)
	if err != nil {
		t.Fatalf("Place(%+v) = %v, want success", req, err)
	}
	return res
}

// releaseSum totals the releases for one (account, asset) key.
func releaseSum(rs []HoldRelease, acct ledger.AccountID, asset ledger.AssetID) *uint256.Int {
	sum := uint256.NewInt(0)
	for _, r := range rs {
		if r.Account == acct && r.Asset == asset {
			sum.Add(sum, r.Amount)
		}
	}
	return sum
}

// --- Task 4: limit GTC and partial fills ---

func TestPlaceFullFillFields(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b := tacct(1), tacct(2)

	mk := placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 100))
	if mk.Order.Status != StatusNew {
		t.Fatalf("maker status = %s, want new", mk.Order.Status)
	}
	if len(mk.Releases) != 0 {
		t.Fatalf("resting maker releases = %d, want 0", len(mk.Releases))
	}

	got := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))
	if got.Order.Status != StatusFilled {
		t.Fatalf("taker status = %s, want filled", got.Order.Status)
	}
	if got.Order.Filled.Uint64() != 100 {
		t.Fatalf("taker filled = %s, want 100", got.Order.Filled)
	}
	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(got.Trades))
	}
	tr := got.Trades[0]
	want := Trade{
		ID: 1, Market: 1, Price: 250,
		Qty: amt(100), Notional: amt(250),
		MakerOrder: mk.Order.ID, TakerOrder: got.Order.ID,
		Maker: b, Taker: a, MakerSide: SideSell,
	}
	assertTradeEqual(t, tr, want)

	// Both freezes are fully consumed: taker quote 250, maker base 100.
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 250 {
		t.Fatalf("taker quote released = %s, want 250", s)
	}
	if s := releaseSum(got.Releases, b, 2); s.Uint64() != 100 {
		t.Fatalf("maker base released = %s, want 100", s)
	}
	bids, asks := e.Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("book not empty after full fill: bids=%v asks=%v", bids, asks)
	}
}

func assertTradeEqual(t *testing.T, got, want Trade) {
	t.Helper()
	if got.ID != want.ID || got.Market != want.Market || got.Price != want.Price ||
		got.MakerOrder != want.MakerOrder || got.TakerOrder != want.TakerOrder ||
		got.Maker != want.Maker || got.Taker != want.Taker || got.MakerSide != want.MakerSide {
		t.Fatalf("trade header =\n %+v\nwant\n %+v", got, want)
	}
	if got.Qty.Cmp(want.Qty) != 0 || got.Notional.Cmp(want.Notional) != 0 {
		t.Fatalf("trade amounts qty=%s notional=%s, want qty=%s notional=%s", got.Qty, got.Notional, want.Qty, want.Notional)
	}
}

// TestPartialFillRestsInLevel covers a maker smaller than the incoming order:
// the taker partially fills and its remainder rests at its limit price. A
// second bid at the same price then queues behind it (Seq order), showing the
// remainder was appended at the back of the level's FIFO.
func TestPartialFillRestsInLevel(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b, c := tacct(1), tacct(2), tacct(3)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 50)) // maker ask id1
	got := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))
	if got.Order.Status != StatusPartiallyFilled {
		t.Fatalf("status = %s, want partially_filled", got.Order.Status)
	}
	if got.Order.Filled.Uint64() != 50 {
		t.Fatalf("filled = %s, want 50", got.Order.Filled)
	}

	// The maker is gone; the taker remainder rests alone at 250.
	bids, asks := e.Book(1, 5)
	if len(asks) != 0 {
		t.Fatalf("asks = %v, want empty", asks)
	}
	if len(bids) != 1 || bids[0].Price != 250 || bids[0].Total.Uint64() != 50 {
		t.Fatalf("bids = %v, want [{250 50}]", bids)
	}

	// A second covering bid queues behind the remainder.
	tail := placeOK(t, e, limitReq(1, c, SideBuy, TIFGTC, 250, 20))
	lvl := e.books[1].bids.levels[250]
	var order []OrderID
	for el := lvl.fifo.Front(); el != nil; el = el.Next() {
		order = append(order, el.Value.(*Order).ID)
	}
	if len(order) != 2 || order[0] != got.Order.ID || order[1] != tail.Order.ID {
		t.Fatalf("bid fifo = %v, want [%d %d]", order, got.Order.ID, tail.Order.ID)
	}
	if lvl.total.Uint64() != 70 {
		t.Fatalf("level total = %s, want 70", lvl.total)
	}
}

func TestPricePrioritySweepsCheapestAsks(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b, c, d := tacct(1), tacct(2), tacct(3), tacct(4)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 100, 100))
	placeOK(t, e, limitReq(1, c, SideSell, TIFGTC, 200, 100))
	placeOK(t, e, limitReq(1, d, SideSell, TIFGTC, 300, 100))

	got := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 300, 250))
	if len(got.Trades) != 3 {
		t.Fatalf("trades = %d, want 3", len(got.Trades))
	}
	gotPrices := []Price{got.Trades[0].Price, got.Trades[1].Price, got.Trades[2].Price}
	wantPrices := []Price{100, 200, 300}
	for i := range wantPrices {
		if gotPrices[i] != wantPrices[i] {
			t.Fatalf("trade prices = %v, want %v", gotPrices, wantPrices)
		}
	}
	if got.Trades[0].Qty.Uint64() != 100 || got.Trades[1].Qty.Uint64() != 100 || got.Trades[2].Qty.Uint64() != 50 {
		t.Fatalf("trade qtys = %s/%s/%s, want 100/100/50",
			got.Trades[0].Qty, got.Trades[1].Qty, got.Trades[2].Qty)
	}
	if got.Order.Status != StatusFilled || got.Order.Filled.Uint64() != 250 {
		t.Fatalf("taker status=%s filled=%s, want filled 250", got.Order.Status, got.Order.Filled)
	}
}

func TestTimePrioritySameLevel(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b, c := tacct(1), tacct(2), tacct(3)

	first := placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 100))
	second := placeOK(t, e, limitReq(1, c, SideSell, TIFGTC, 250, 100))

	got := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))
	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(got.Trades))
	}
	if got.Trades[0].MakerOrder != first.Order.ID {
		t.Fatalf("maker = %d, want earliest %d (not %d)", got.Trades[0].MakerOrder, first.Order.ID, second.Order.ID)
	}
}

// TestNonDivisibleRoundingNoLeak exercises a price that does not divide the
// notional exactly. The taker over-reserves by ceil and must get the rounding
// remainder back; both freezes end exactly at zero (no funds stranded).
func TestNonDivisibleRoundingNoLeak(t *testing.T) {
	e := newTestEngine(t, testMarket()) // scale 100
	a, b := tacct(1), tacct(2)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 101, 7))
	got := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 101, 7))

	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(got.Trades))
	}
	tr := got.Trades[0]
	if tr.Notional.Uint64() != 7 { // floor(7*101/100) = 7
		t.Fatalf("notional = %s, want floor 7", tr.Notional)
	}
	if want := testMarket().Notional(tr.Qty, tr.Price); tr.Notional.Cmp(want) != 0 {
		t.Fatalf("notional %s != formula %s", tr.Notional, want)
	}
	// Taker reserved ceil(7*101/100)=8; 7 consumed by the fill, 1 returned.
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 8 {
		t.Fatalf("taker quote released = %s, want 8 (7 fill + 1 rounding)", s)
	}
	if s := releaseSum(got.Releases, b, 2); s.Uint64() != 7 {
		t.Fatalf("maker base released = %s, want 7", s)
	}
	if e.all[got.Trades[0].MakerOrder].reserved.Sign() != 0 {
		t.Fatalf("maker still holds %s", e.all[got.Trades[0].MakerOrder].reserved)
	}
	if e.all[got.Order.ID].reserved.Sign() != 0 {
		t.Fatalf("taker still holds %s", e.all[got.Order.ID].reserved)
	}
	if len(e.OpenReserves()) != 0 {
		t.Fatalf("OpenReserves = %v, want empty", e.OpenReserves())
	}
}

// A resting BUY that is fully filled must release its ceil rounding remainder.
// Its reserve is ceil(qty*price/scale) while fills deduct floor(...), so the
// difference is a stranded freeze unless the maker's residual is released when
// it leaves the book. A maker that is a SELL cannot show this: its reserve is
// the exact base quantity.
func TestFullFillReleasesMakerBuyRoundingRemainder(t *testing.T) {
	e := newTestEngine(t, testMarket()) // scale 100
	maker, taker := tacct(1), tacct(2)

	// Maker rests a bid: reserve = ceil(7*101/100) = 8, but the fill's notional
	// is floor(7*101/100) = 7, leaving exactly 1 unit of stranded freeze.
	rest := placeOK(t, e, limitReq(1, maker, SideBuy, TIFGTC, 101, 7))
	if len(rest.Trades) != 0 || rest.Order.Status != StatusNew {
		t.Fatalf("maker should have rested: trades=%d status=%v", len(rest.Trades), rest.Order.Status)
	}

	got := placeOK(t, e, limitReq(1, taker, SideSell, TIFGTC, 101, 7))
	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(got.Trades))
	}
	if got.Trades[0].Notional.Uint64() != 7 {
		t.Fatalf("notional = %s, want 7", got.Trades[0].Notional)
	}

	makerOrder := e.all[rest.Order.ID]
	if makerOrder.Status != StatusFilled {
		t.Fatalf("maker status = %v, want Filled", makerOrder.Status)
	}
	// The maker's entire 8-unit freeze must be accounted for: 7 consumed by the
	// trade and 1 returned as rounding residue when it left the book.
	if s := releaseSum(got.Releases, maker, 1); s.Uint64() != 8 {
		t.Fatalf("maker released during fill = %s, want 8 (7 fill + 1 rounding)", s)
	}
	if makerOrder.reserved.Sign() != 0 {
		t.Fatalf("maker still holds %s after being fully filled; stranded freeze", makerOrder.reserved)
	}
	if len(e.OpenReserves()) != 0 {
		t.Fatalf("OpenReserves = %v, want empty", e.OpenReserves())
	}
}

func TestPlaceRejectsInvalidRequests(t *testing.T) {
	m := testMarket()
	m.MinQty = amt(100)
	e := newTestEngine(t, m)
	a := tacct(1)

	tests := []struct {
		name string
		req  PlaceRequest
		want error
	}{
		{"zero qty", limitReq(1, a, SideBuy, TIFGTC, 250, 0), ErrBadRequest},
		{"below min qty", limitReq(1, a, SideBuy, TIFGTC, 250, 50), ErrBadRequest},
		{"limit price zero", limitReq(1, a, SideBuy, TIFGTC, 0, 100), ErrBadRequest},
		{"unknown market", limitReq(9, a, SideBuy, TIFGTC, 250, 100), ErrUnknownMarket},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.Place(tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if len(e.all) != 0 {
		t.Fatalf("rejected requests created orders: %d", len(e.all))
	}
	if e.NextSeq() != 1 {
		t.Fatalf("NextSeq = %d, want 1 (no seq consumed by rejections)", e.NextSeq())
	}
	bids, asks := e.Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatal("rejected requests changed the book")
	}
}

func TestRejectionLeavesExistingBookUntouched(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a := tacct(1)
	placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))

	beforeBids, beforeAsks := e.Book(1, 5)
	if _, err := e.Place(limitReq(1, tacct(2), SideSell, TIFGTC, 0, 10)); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v, want ErrBadRequest", err)
	}
	afterBids, afterAsks := e.Book(1, 5)
	if len(afterBids) != len(beforeBids) || afterBids[0].Total.Uint64() != beforeBids[0].Total.Uint64() {
		t.Fatalf("book changed by rejection: before=%v after=%v", beforeBids, afterBids)
	}
	if len(afterAsks) != len(beforeAsks) {
		t.Fatal("asks changed by rejection")
	}
}

// --- Task 5: IOC and market orders ---

func TestIOCResidualNotRested(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b := tacct(1), tacct(2)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 50))
	got := placeOK(t, e, limitReq(1, a, SideBuy, TIFIOC, 250, 100))

	if got.Order.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Order.Status)
	}
	if got.Order.Filled.Uint64() != 50 {
		t.Fatalf("filled = %s, want 50", got.Order.Filled)
	}
	bids, asks := e.Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("IOC remainder must not rest: bids=%v asks=%v", bids, asks)
	}
	// Reserve 250; 125 consumed by the 50-lot fill; 125 returned.
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 250 {
		t.Fatalf("taker quote released = %s, want 250", s)
	}
	if e.all[got.Order.ID].reserved.Sign() != 0 {
		t.Fatalf("IOC order still holds %s", e.all[got.Order.ID].reserved)
	}
}

func TestMarketBuyUsesExactBudgetThenStops(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b, c := tacct(1), tacct(2), tacct(3)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 200, 100)) // notional 200
	placeOK(t, e, limitReq(1, c, SideSell, TIFGTC, 300, 100)) // notional 300

	got := placeOK(t, e, marketBuyReq(1, a, 200, 200))
	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1 (budget exhausted exactly)", len(got.Trades))
	}
	if got.Trades[0].Price != 200 || got.Trades[0].Notional.Uint64() != 200 {
		t.Fatalf("trade = %+v, want price 200 notional 200", got.Trades[0])
	}
	if got.Order.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Order.Status)
	}
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 200 {
		t.Fatalf("taker quote released = %s, want 200", s)
	}
}

func TestMarketBuyBudgetCapsFill(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b := tacct(1), tacct(2)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 100))

	// Budget 100 buys floor(100*100/250) = 40 base at notional 100.
	got := placeOK(t, e, marketBuyReq(1, a, 100, 100))
	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(got.Trades))
	}
	if got.Trades[0].Qty.Uint64() != 40 || got.Trades[0].Notional.Uint64() != 100 {
		t.Fatalf("trade qty=%s notional=%s, want 40/100", got.Trades[0].Qty, got.Trades[0].Notional)
	}
	if got.Order.Status != StatusCancelled || got.Order.Filled.Uint64() != 40 {
		t.Fatalf("status=%s filled=%s, want cancelled 40", got.Order.Status, got.Order.Filled)
	}
	if e.all[got.Order.ID].reserved.Sign() != 0 {
		t.Fatalf("market buy still holds %s", e.all[got.Order.ID].reserved)
	}
}

func TestMarketBuyBudgetTooSmallIsZeroFill(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b := tacct(1), tacct(2)

	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 100))

	// floor(1*100/250) = 0 -> no fill, whole budget returned.
	got := placeOK(t, e, marketBuyReq(1, a, 100, 1))
	if len(got.Trades) != 0 {
		t.Fatalf("trades = %d, want 0", len(got.Trades))
	}
	if got.Order.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Order.Status)
	}
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 1 {
		t.Fatalf("taker quote released = %s, want full budget 1", s)
	}
}

func TestMarketSellSweepsThenCancels(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b, c := tacct(1), tacct(2), tacct(3)

	placeOK(t, e, limitReq(1, b, SideBuy, TIFGTC, 200, 100))
	placeOK(t, e, limitReq(1, c, SideBuy, TIFGTC, 150, 50))

	got := placeOK(t, e, marketSellReq(1, a, 200))
	if len(got.Trades) != 2 {
		t.Fatalf("trades = %d, want 2", len(got.Trades))
	}
	if got.Trades[0].Price != 200 || got.Trades[1].Price != 150 {
		t.Fatalf("prices = %d,%d want 200,150", got.Trades[0].Price, got.Trades[1].Price)
	}
	if got.Order.Status != StatusCancelled || got.Order.Filled.Uint64() != 150 {
		t.Fatalf("status=%s filled=%s, want cancelled 150", got.Order.Status, got.Order.Filled)
	}
	// 100 + 50 filled plus 50 cancelled remainder = full 200 base returned.
	if s := releaseSum(got.Releases, a, 2); s.Uint64() != 200 {
		t.Fatalf("taker base released = %s, want 200", s)
	}
	bids, asks := e.Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("book should be empty: bids=%v asks=%v", bids, asks)
	}
}

func TestMarketCombinationsRejected(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a := tacct(1)

	cases := []struct {
		name string
		req  PlaceRequest
	}{
		{"market GTC", PlaceRequest{Market: 1, Account: a, Side: SideBuy, Type: TypeMarket, TIF: TIFGTC, Qty: amt(10), MaxNotional: amt(100)}},
		{"market buy nil max notional", PlaceRequest{Market: 1, Account: a, Side: SideBuy, Type: TypeMarket, TIF: TIFIOC, Qty: amt(10)}},
		{"market buy zero max notional", marketBuyReq(1, a, 10, 0)},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.Place(tt.req); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("err = %v, want ErrBadRequest", err)
			}
			if _, _, err := e.RequiredReserve(tt.req); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("RequiredReserve err = %v, want ErrBadRequest", err)
			}
		})
	}
}

func TestMarketBuyLeavesNoReserve(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b := tacct(1), tacct(2)
	placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 100))
	placeOK(t, e, marketBuyReq(1, a, 100, 250))

	res := e.OpenReserves()
	if _, ok := res[ledger.HoldKey{Account: a, Asset: 1}]; ok {
		t.Fatalf("market buy left a quote reserve: %v", res)
	}
}

// --- Task 6: STP and cancel ---

func TestSTPCancelsRestingMakerAndRests(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a := tacct(1)

	resting := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))
	got := placeOK(t, e, limitReq(1, a, SideSell, TIFGTC, 250, 100))

	if len(got.Trades) != 0 {
		t.Fatalf("trades = %d, want 0 (self-trade blocked)", len(got.Trades))
	}
	if o := e.all[resting.Order.ID]; o.Status != StatusCancelled {
		t.Fatalf("resting order status = %s, want cancelled", o.Status)
	}
	if o := e.all[resting.Order.ID]; o.reserved.Sign() != 0 {
		t.Fatalf("cancelled maker still holds %s", o.reserved)
	}
	if got.Order.Status != StatusNew {
		t.Fatalf("incoming status = %s, want new", got.Order.Status)
	}
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 250 {
		t.Fatalf("cancelled maker quote released = %s, want 250", s)
	}
	bids, asks := e.Book(1, 5)
	if len(bids) != 0 {
		t.Fatalf("bids = %v, want empty", bids)
	}
	if len(asks) != 1 || asks[0].Price != 250 || asks[0].Total.Uint64() != 100 {
		t.Fatalf("asks = %v, want [{250 100}]", asks)
	}
}

func TestSTPContinuesMatchingOtherAccounts(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, c := tacct(1), tacct(3)

	self := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))
	other := placeOK(t, e, limitReq(1, c, SideBuy, TIFGTC, 250, 100))

	// The incoming sell sees self's bid first (same account -> STP cancel), then
	// c's bid behind it, which it trades against.
	got := placeOK(t, e, limitReq(1, a, SideSell, TIFGTC, 250, 100))
	if len(got.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(got.Trades))
	}
	if got.Trades[0].Maker != c || got.Trades[0].Taker != a || got.Trades[0].TakerOrder != got.Order.ID {
		t.Fatalf("trade = %+v, want maker=c taker=a", got.Trades[0])
	}
	if got.Trades[0].MakerOrder != other.Order.ID {
		t.Fatalf("maker order = %d, want %d", got.Trades[0].MakerOrder, other.Order.ID)
	}
	if e.all[self.Order.ID].Status != StatusCancelled {
		t.Fatalf("self maker status = %s, want cancelled", e.all[self.Order.ID].Status)
	}
	if got.Order.Status != StatusFilled {
		t.Fatalf("incoming status = %s, want filled", got.Order.Status)
	}
	// STP released 250 quote; the taker sell released 100 base; c (buyer) had
	// 250 quote reserved and released it.
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 250 {
		t.Fatalf("STP quote released = %s, want 250", s)
	}
	if s := releaseSum(got.Releases, a, 2); s.Uint64() != 100 {
		t.Fatalf("taker base released = %s, want 100", s)
	}
	if s := releaseSum(got.Releases, c, 1); s.Uint64() != 250 {
		t.Fatalf("maker buy quote released = %s, want 250", s)
	}
	bids, asks := e.Book(1, 5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Fatalf("book not empty: bids=%v asks=%v", bids, asks)
	}
}

func TestCancelPermissionsLifecycleAndLevelCleanup(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a, b := tacct(1), tacct(2)

	placed := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))

	if _, err := e.Cancel(1, b, placed.Order.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("other-owner cancel err = %v, want ErrNotOwner", err)
	}
	if _, err := e.Cancel(1, a, 999); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("unknown-order cancel err = %v, want ErrOrderNotFound", err)
	}
	if _, err := e.Cancel(9, a, placed.Order.ID); !errors.Is(err, ErrUnknownMarket) {
		t.Fatalf("unknown-market cancel err = %v, want ErrUnknownMarket", err)
	}

	got, err := e.Cancel(1, a, placed.Order.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Order.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Order.Status)
	}
	if s := releaseSum(got.Releases, a, 1); s.Uint64() != 250 {
		t.Fatalf("released = %s, want 250", s)
	}
	if _, ok := e.books[1].bids.levels[250]; ok {
		t.Fatal("empty price level left behind after cancel")
	}
	if len(e.books[1].bids.prices) != 0 {
		t.Fatalf("bids prices = %v, want empty", e.books[1].bids.prices)
	}
	if _, err := e.Cancel(1, a, placed.Order.ID); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("double cancel err = %v, want ErrOrderNotFound", err)
	}
}

func TestRequiredReservePureFunction(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a := tacct(1)

	asset, amount, err := e.RequiredReserve(limitReq(1, a, SideBuy, TIFGTC, 101, 7))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if asset != 1 || amount.Uint64() != 8 { // ceil(7*101/100)
		t.Fatalf("limit buy reserve = (%d, %s), want (1, 8)", asset, amount)
	}

	asset, amount, err = e.RequiredReserve(limitReq(1, a, SideSell, TIFGTC, 101, 7))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if asset != 2 || amount.Uint64() != 7 {
		t.Fatalf("sell reserve = (%d, %s), want (2, 7)", asset, amount)
	}

	asset, amount, err = e.RequiredReserve(marketBuyReq(1, a, 50, 123))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if asset != 1 || amount.Uint64() != 123 {
		t.Fatalf("market buy reserve = (%d, %s), want (1, 123)", asset, amount)
	}

	// RequiredReserve must not mutate state.
	if len(e.all) != 0 || len(e.OpenReserves()) != 0 || e.NextSeq() != 1 {
		t.Fatal("RequiredReserve mutated engine state")
	}
}

// TestOrderLookupReturnsCopy guards the pointer rule on Engine.Order.
func TestOrderLookupReturnsCopy(t *testing.T) {
	e := newTestEngine(t, testMarket())
	a := tacct(1)
	placed := placeOK(t, e, limitReq(1, a, SideBuy, TIFGTC, 250, 100))

	got, ok := e.Order(placed.Order.ID)
	if !ok {
		t.Fatal("order not found")
	}
	got.Qty.SetUint64(1)
	got.reserved.SetUint64(1)
	if _, ok := e.Order(placed.Order.ID); !ok {
		t.Fatal("order disappeared")
	}
	if e.all[placed.Order.ID].Qty.Uint64() != 100 || e.all[placed.Order.ID].reserved.Uint64() != 250 {
		t.Fatal("internal order mutated through returned copy")
	}
	if _, ok := e.Order(999); ok {
		t.Fatal("unknown order reported found")
	}
}

// --- checked arithmetic: overflow must reject, never wrap ---

// TestOverflowingLimitBuyIsRejected is the reduced repro of the fund-safety
// bug: with 2 base decimals, Qty=2^193 and Price=2^63 give qty*price == 2^256
// == 0 (mod 2^256). Unchecked NotionalCeil returns 0, RequiredReserve returns a
// zero freeze and Place accepts a limit buy that freezes nothing. Checked code
// must reject with ErrAmountTooLarge and leave every counter and the book alone.
func TestOverflowingLimitBuyIsRejected(t *testing.T) {
	raised := testMarket()
	raised.MaxQty = pow2(200) // keep the qty bound from masking the overflow
	cases := []struct {
		name string
		m    Market
	}{
		{"default max qty (bound preempts)", testMarket()},
		{"raised max qty (notional overflow must reject)", raised},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, tc.m)
			req := PlaceRequest{
				Market: 1, Account: tacct(1), Side: SideBuy, Type: TypeLimit, TIF: TIFGTC,
				Price: Price(1) << 63,
				Qty:   pow2(193),
			}

			if _, _, err := e.RequiredReserve(req); !errors.Is(err, ErrAmountTooLarge) {
				t.Fatalf("RequiredReserve err = %v, want ErrAmountTooLarge", err)
			}

			var res Result
			var err error
			assertNoPanic(t, "Place overflow", func() { res, err = e.Place(req) })
			if !errors.Is(err, ErrAmountTooLarge) {
				t.Fatalf("Place err = %v, want ErrAmountTooLarge", err)
			}
			if res.Order != nil || len(res.Trades) != 0 || len(res.Releases) != 0 {
				t.Fatalf("rejected Place returned a non-empty Result: %+v", res)
			}
			if len(e.all) != 0 {
				t.Fatalf("rejected Place created %d orders", len(e.all))
			}
			if e.NextSeq() != 1 {
				t.Fatalf("NextSeq = %d, want 1 (no seq consumed)", e.NextSeq())
			}
			if bids, asks := e.Book(1, 5); len(bids) != 0 || len(asks) != 0 {
				t.Fatalf("book changed by rejection: bids=%v asks=%v", bids, asks)
			}
			if got := e.OpenReserves(); len(got) != 0 {
				t.Fatalf("OpenReserves = %v, want empty", got)
			}
		})
	}
}

// TestQtyAboveMaxIsRejected pins the request bound: a qty above Market.MaxQty
// is a rejection, and one at/below the bound is accepted.
func TestQtyAboveMaxIsRejected(t *testing.T) {
	m := testMarket()
	m.MaxQty = amt(1000)
	e := newTestEngine(t, m)
	a := tacct(1)

	above := limitReq(1, a, SideBuy, TIFGTC, 250, 1001)
	if _, _, err := e.RequiredReserve(above); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("above-max RequiredReserve err = %v, want ErrAmountTooLarge", err)
	}
	if _, err := e.Place(above); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("above-max Place err = %v, want ErrAmountTooLarge", err)
	}
	if len(e.all) != 0 || e.NextSeq() != 1 {
		t.Fatalf("over-max order mutated state: orders=%d seq=%d", len(e.all), e.NextSeq())
	}

	atBound := limitReq(1, a, SideBuy, TIFGTC, 250, 1000)
	if _, err := e.Place(atBound); err != nil {
		t.Fatalf("at-bound Place err = %v, want success", err)
	}
	below := limitReq(1, tacct(2), SideSell, TIFGTC, 250, 999)
	if _, err := e.Place(below); err != nil {
		t.Fatalf("below-max Place err = %v, want success", err)
	}
	if len(e.all) != 2 {
		t.Fatalf("orders = %d, want 2", len(e.all))
	}
}

// TestMarketBuyCapDoesNotOverflow exercises the market-buy budget cap with a
// very large MaxNotional. The cap must not wrap: with a non-binding budget the
// whole qty fills and the unused budget is released; with a binding budget the
// fill is clamped by floor(left*scale/price).
func TestMarketBuyCapDoesNotOverflow(t *testing.T) {
	t.Run("huge budget does not bind", func(t *testing.T) {
		e := newTestEngine(t, testMarket())
		a, b := tacct(1), tacct(2)
		placeOK(t, e, limitReq(1, b, SideSell, TIFGTC, 250, 1000))

		budget := pow2(200)
		req := PlaceRequest{
			Market: 1, Account: a, Side: SideBuy, Type: TypeMarket, TIF: TIFIOC,
			Qty: pow2(128), MaxNotional: budget,
		}
		var got Result
		assertNoPanic(t, "market buy with 2^200 budget", func() { got = placeOK(t, e, req) })

		if len(got.Trades) != 1 {
			t.Fatalf("trades = %d, want 1", len(got.Trades))
		}
		if got.Trades[0].Qty.Uint64() != 1000 || got.Trades[0].Notional.Uint64() != 2500 {
			t.Fatalf("trade qty=%s notional=%s, want 1000/2500", got.Trades[0].Qty, got.Trades[0].Notional)
		}
		// The whole budget is released: the fill's notional funds the transfer
		// and the remainder is the unused budget. No freeze may be stranded.
		if s := releaseSum(got.Releases, a, 1); s.Cmp(budget) != 0 {
			t.Fatalf("released = %s, want the full budget %s", s, budget)
		}
		if len(e.OpenReserves()) != 0 {
			t.Fatalf("OpenReserves = %v, want empty", e.OpenReserves())
		}
	})

	t.Run("binding budget clamps without wrapping", func(t *testing.T) {
		e := newTestEngine(t, testMarket())
		a, b := tacct(1), tacct(2)
		// A deep maker at a high price forces the else branch: at full size the
		// fill's notional (2^128*2^63/100) exceeds the much smaller budget.
		placeOK(t, e, PlaceRequest{
			Market: 1, Account: b, Side: SideSell, Type: TypeLimit, TIF: TIFGTC,
			Price: Price(1) << 63, Qty: pow2(128),
		})

		budget := pow2(100)
		req := PlaceRequest{
			Market: 1, Account: a, Side: SideBuy, Type: TypeMarket, TIF: TIFIOC,
			Qty: pow2(128), MaxNotional: budget,
		}
		var got Result
		assertNoPanic(t, "market buy with binding budget", func() { got = placeOK(t, e, req) })

		if len(got.Trades) != 1 {
			t.Fatalf("trades = %d, want 1", len(got.Trades))
		}
		// allowed = floor(2^100 * 100 / 2^63).
		wantQty := new(uint256.Int).Div(new(uint256.Int).Mul(budget, testMarket().scale()), amt(1<<63))
		if got.Trades[0].Qty.Cmp(wantQty) != 0 {
			t.Fatalf("trade qty = %s, want %s", got.Trades[0].Qty, wantQty)
		}
		if got.Trades[0].Notional.Cmp(budget) > 0 {
			t.Fatalf("trade notional %s exceeds budget %s", got.Trades[0].Notional, budget)
		}
		if s := releaseSum(got.Releases, a, 1); s.Cmp(budget) > 0 {
			t.Fatalf("released %s exceeds budget %s", s, budget)
		}
	})
}

// TestConsumeReserveUnderflowPanics pins that a reserve can never be subtracted
// below zero: the silent wrap is exactly what corrupted the reserve and drove
// holds.Release into underflow, so it must panic instead.
func TestConsumeReserveUnderflowPanics(t *testing.T) {
	e := newTestEngine(t, testMarket())

	o := &Order{ID: 1, Side: SideSell, reserved: amt(5)}
	var rel []HoldRelease
	assertPanics(t, "consumeReserve underflow", func() {
		e.consumeReserve(testMarket(), o, amt(10), nil, &rel)
	})
	if len(rel) != 0 {
		t.Fatalf("underflow emitted releases: %v", rel)
	}

	// A reserve that exactly covers the amount must consume cleanly.
	exact := &Order{ID: 2, Side: SideSell, reserved: amt(10)}
	assertNoPanic(t, "consumeReserve exact", func() {
		e.consumeReserve(testMarket(), exact, amt(10), nil, &rel)
	})
	if exact.reserved.Sign() != 0 {
		t.Fatalf("exact consume left reserved %s, want 0", exact.reserved)
	}
}

// The overflow fix must reject overflow, not size: a large but representable
// buy still has to produce a non-zero freeze. Guards against "fixing" the wrap
// by rejecting anything with a big magnitude.
func TestLargeRepresentableBuyReserveIsNonZero(t *testing.T) {
	e := newTestEngine(t, testMarket())
	qty := new(uint256.Int).Lsh(uint256.NewInt(1), 100) // 2^100, comfortably under MaxQty
	asset, amount, err := e.RequiredReserve(PlaceRequest{
		Market: 1, Account: tacct(1),
		Side: SideBuy, Type: TypeLimit, TIF: TIFGTC,
		Price: Price(1 << 40), Qty: qty,
	})
	if err != nil {
		t.Fatalf("RequiredReserve err = %v, want nil", err)
	}
	if asset != 1 {
		t.Fatalf("reserve asset = %d, want 1 (quote)", asset)
	}
	if amount.IsZero() {
		t.Fatal("required reserve is zero for a large legitimate buy")
	}
}

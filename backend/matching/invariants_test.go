package matching

import (
	"fmt"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// --- deterministic PRNG ---------------------------------------------------
//
// math/rand is forbidden in this package: determinism is a hard kernel
// requirement, so the invariant tests bring their own fixed-seed generator.
// This is a 64-bit LCG (Knuth/Numerical-Recipes multipliers); the exact
// sequence does not matter, only that it is identical across runs.

type lcg struct{ state uint64 }

func newLCG(seed uint64) *lcg { return &lcg{state: seed} }

func (r *lcg) next() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

func (r *lcg) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// --- random instruction generation ----------------------------------------

func testMarkets() []Market {
	return []Market{
		{ID: 1, Symbol: "ETH/USDT", Base: 2, Quote: 1, BaseDecimals: 2},
		{ID: 2, Symbol: "BTC/USDT", Base: 3, Quote: 1, BaseDecimals: 3},
	}
}

type command struct {
	place   bool
	req     PlaceRequest
	market  MarketID
	account ledger.AccountID
	orderID OrderID
}

// genCommands produces a mixed stream of Place and Cancel commands, including
// deliberately invalid ones (qty 0, price 0, Market+GTC, nil market-buy cap)
// and cancels that may target unknown or not-owned orders, so the failure paths
// are exercised and must behave deterministically.
func genCommands(seed uint64, n int) []command {
	r := newLCG(seed)
	marketIDs := []MarketID{1, 2}
	accounts := []ledger.AccountID{tacct(1), tacct(2), tacct(3)}
	cmds := make([]command, 0, n)

	for i := 0; i < n; i++ {
		if i > 0 && r.intn(4) == 0 {
			id := OrderID(1 + r.intn(i))
			if r.intn(6) == 0 { // unknown order
				id = OrderID(1_000_000 + r.intn(50))
			}
			cmds = append(cmds, command{
				place:   false,
				market:  marketIDs[r.intn(len(marketIDs))],
				account: accounts[r.intn(len(accounts))],
				orderID: id,
			})
			continue
		}

		side := Side(r.intn(2))
		typ := TypeLimit
		if r.intn(4) == 0 {
			typ = TypeMarket
		}
		tif := TIFGTC
		if typ == TypeMarket || r.intn(3) == 0 {
			tif = TIFIOC
		}
		price := Price(100 + r.intn(10)*25)
		qty := amt(uint64(1 + r.intn(20)))
		var maxNotional *uint256.Int
		if typ == TypeMarket && side == SideBuy {
			maxNotional = amt(uint64(1 + r.intn(500)))
		}

		// Periodically emit extreme magnitudes so the checked-arithmetic paths
		// are exercised: a tiny-value-only generator is exactly what let the
		// notional overflow (2^193 * 2^63 == 0 mod 2^256) escape. Qty is pushed
		// to and past the 2^128 bound, Price to the uint64 ceiling near 2^63,
		// MaxNotional to 2^200.
		if r.intn(4) == 0 {
			switch r.intn(4) {
			case 0:
				qty = pow2(128)
			case 1:
				qty = new(uint256.Int).Add(pow2(128), amt(1))
			case 2:
				qty = pow2(193)
			case 3:
				qty = new(uint256.Int).Add(pow2(192), amt(uint64(1+r.intn(1000))))
			}
		}
		if r.intn(4) == 0 {
			switch r.intn(3) {
			case 0:
				price = Price(1) << 63
			case 1:
				price = Price(^uint64(0))
			case 2:
				price = Price(1) << 62
			}
		}
		if maxNotional != nil && r.intn(4) == 0 {
			switch r.intn(2) {
			case 0:
				maxNotional = pow2(200)
			case 1:
				maxNotional = new(uint256.Int).Add(pow2(200), amt(1))
			}
		}
		// The exact reduced repro, injected verbatim.
		if r.intn(16) == 0 {
			qty = pow2(193)
			price = Price(1) << 63
			if maxNotional != nil {
				maxNotional = pow2(200)
			}
		}

		switch r.intn(16) { // periodic invalid requests
		case 0:
			qty = amt(0)
		case 1:
			if typ == TypeLimit {
				price = 0
			}
		case 2:
			if typ == TypeMarket {
				tif = TIFGTC
			}
		case 3:
			if typ == TypeMarket && side == SideBuy {
				maxNotional = nil
			}
		}

		cmds = append(cmds, command{place: true, req: PlaceRequest{
			Market:      marketIDs[r.intn(len(marketIDs))],
			Account:     accounts[r.intn(len(accounts))],
			Side:        side,
			Type:        typ,
			TIF:         tif,
			Price:       price,
			Qty:         qty,
			MaxNotional: maxNotional,
		}})
	}
	return cmds
}

// runCommands replays cmds, ignoring the expected rejections (they are part of
// the deterministic stream), collecting every trade produced. after, when
// non-nil, runs after each command so invariant breaks are caught early.
func runCommands(e *Engine, cmds []command, after func()) []Trade {
	var trades []Trade
	for _, c := range cmds {
		if c.place {
			res, err := e.Place(c.req)
			if err == nil {
				trades = append(trades, res.Trades...)
			}
		} else {
			res, err := e.Cancel(c.market, c.account, c.orderID)
			if err == nil {
				trades = append(trades, res.Trades...)
			}
		}
		if after != nil {
			after()
		}
	}
	return trades
}

// --- invariant checks (spec §4.7) -----------------------------------------

func checkBookInvariants(t *testing.T, e *Engine) {
	t.Helper()
	for _, m := range e.markets {
		bk := e.books[m.ID]
		checkSideInvariants(t, m, bk.bids, "bids")
		checkSideInvariants(t, m, bk.asks, "asks")
	}
	for id, o := range e.all {
		if !o.IsOpen() {
			if _, ok := e.books[o.Market].orders[id]; ok {
				t.Fatalf("order %d closed (%s) but still resting", id, o.Status)
			}
			continue
		}
		// Invariant 4: Filled <= Qty and Remaining > 0. Note a freshly rested
		// order has Filled == 0, so this is 0 <= Filled < Qty; the plan's
		// literal "0 < Filled" cannot hold for an untraded resting order and
		// is flagged in the report.
		if o.Filled.Cmp(o.Qty) > 0 {
			t.Fatalf("order %d: Filled %s exceeds Qty %s", id, o.Filled, o.Qty)
		}
		if o.Remaining().Sign() <= 0 {
			t.Fatalf("order %d: open with Remaining %s", id, o.Remaining())
		}
		if _, ok := e.books[o.Market].orders[id]; !ok {
			t.Fatalf("order %d open but absent from its book", id)
		}
		// Invariant 5.
		m := e.byID[o.Market]
		if o.Side == SideSell {
			if o.reserved.Cmp(o.Remaining()) != 0 {
				t.Fatalf("sell order %d: reserved %s != remaining %s", id, o.reserved, o.Remaining())
			}
		} else {
			need := m.NotionalCeil(o.Remaining(), o.Price)
			if o.reserved.Cmp(need) < 0 {
				t.Fatalf("buy order %d: reserved %s < ceil(remaining,Price) %s", id, o.reserved, need)
			}
		}
	}
}

func checkSideInvariants(t *testing.T, m Market, s *side, name string) {
	t.Helper()
	// Invariant 2: prices and levels key sets agree.
	if len(s.prices) != len(s.levels) {
		t.Fatalf("market %d %s: %d prices vs %d levels", m.ID, name, len(s.prices), len(s.levels))
	}
	for i := 1; i < len(s.prices); i++ {
		if !s.before(s.prices[i-1], s.prices[i]) {
			t.Fatalf("market %d %s: prices not strictly ordered: %v", m.ID, name, s.prices)
		}
	}
	for _, p := range s.prices {
		lvl, ok := s.levels[p]
		if !ok {
			t.Fatalf("market %d %s: price %d has no level", m.ID, name, p)
		}
		if lvl.price != p {
			t.Fatalf("market %d %s: level %d stores price %d", m.ID, name, p, lvl.price)
		}
		// Invariant 1: no empty level, total == sum of remaining.
		if lvl.fifo.Len() == 0 {
			t.Fatalf("market %d %s: empty level %d left behind", m.ID, name, p)
		}
		sum := uint256.NewInt(0)
		var prevSeq Seq
		first := true
		for el := lvl.fifo.Front(); el != nil; el = el.Next() {
			o := el.Value.(*Order)
			if o.Price != p {
				t.Fatalf("market %d %s: order %d (price %d) in level %d", m.ID, name, o.ID, o.Price, p)
			}
			if !o.IsOpen() {
				t.Fatalf("market %d %s: closed order %d in fifo", m.ID, name, o.ID)
			}
			// Invariant 3: FIFO order == ascending Seq.
			if !first && o.Seq <= prevSeq {
				t.Fatalf("market %d %s: fifo not Seq-ascending at order %d", m.ID, name, o.ID)
			}
			prevSeq = o.Seq
			first = false
			sum.Add(sum, o.Remaining())
		}
		if lvl.total.Cmp(sum) != 0 {
			t.Fatalf("market %d %s: level %d total %s != sum remaining %s", m.ID, name, p, lvl.total, sum)
		}
	}
	for p := range s.levels {
		found := false
		for _, q := range s.prices {
			if q == p {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("market %d %s: level %d missing from prices", m.ID, name, p)
		}
	}
}

// checkTrade pins invariant 6: a trade's notional is exactly the market formula.
func checkTrade(t *testing.T, e *Engine, tr Trade) {
	t.Helper()
	m, ok := e.byID[tr.Market]
	if !ok {
		t.Fatalf("trade %d references unknown market %d", tr.ID, tr.Market)
	}
	if tr.Price == 0 {
		t.Fatalf("trade %d has zero price", tr.ID)
	}
	if tr.Qty.Sign() <= 0 {
		t.Fatalf("trade %d has non-positive qty", tr.ID)
	}
	if got := m.Notional(tr.Qty, tr.Price); tr.Notional.Cmp(got) != 0 {
		t.Fatalf("trade %d notional %s != floor formula %s", tr.ID, tr.Notional, got)
	}
}

// TestBookInvariants drives several seeds and asserts spec §4.7 after every
// command. Breaking any invariant (e.g. removing empty-level cleanup) turns it
// red, which is the point of the test.
func TestBookInvariants(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 42, 0xDEADBEEF} {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			e := newTestEngine(t, testMarkets()...)
			cmds := genCommands(seed, 400)
			trades := runCommands(e, cmds, func() { checkBookInvariants(t, e) })
			for _, tr := range trades {
				checkTrade(t, e, tr)
			}
		})
	}
}

// TestDeterministicReplay is invariant 7: the same instruction stream into two
// fresh engines yields field-identical trades, books, per-order reserves and
// sequence counters.
func TestDeterministicReplay(t *testing.T) {
	for _, seed := range []uint64{7, 99, 123456, 0xABCDEF} {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			cmds := genCommands(seed, 500)
			a := newTestEngine(t, testMarkets()...)
			b := newTestEngine(t, testMarkets()...)
			ta := runCommands(a, cmds, nil)
			tb := runCommands(b, cmds, nil)

			if len(ta) != len(tb) {
				t.Fatalf("trade counts differ: %d vs %d", len(ta), len(tb))
			}
			for i := range ta {
				compareTrade(t, ta[i], tb[i])
			}
			compareEngines(t, a, b)
		})
	}
}

func compareTrade(t *testing.T, a, b Trade) {
	t.Helper()
	if a.ID != b.ID || a.Market != b.Market || a.Price != b.Price ||
		a.MakerOrder != b.MakerOrder || a.TakerOrder != b.TakerOrder ||
		a.Maker != b.Maker || a.Taker != b.Taker || a.MakerSide != b.MakerSide {
		t.Fatalf("trade %d header differs: %+v vs %+v", a.ID, a, b)
	}
	if a.Qty.Cmp(b.Qty) != 0 || a.Notional.Cmp(b.Notional) != 0 {
		t.Fatalf("trade %d amounts differ: %s/%s vs %s/%s", a.ID, a.Qty, a.Notional, b.Qty, b.Notional)
	}
}

func compareEngines(t *testing.T, a, b *Engine) {
	t.Helper()
	if a.NextSeq() != b.NextSeq() {
		t.Fatalf("NextSeq differs: %d vs %d", a.NextSeq(), b.NextSeq())
	}
	if len(a.all) != len(b.all) {
		t.Fatalf("order count differs: %d vs %d", len(a.all), len(b.all))
	}
	for id, oa := range a.all {
		ob, ok := b.all[id]
		if !ok {
			t.Fatalf("order %d missing from second engine", id)
		}
		compareOrder(t, oa, ob)
	}
	for _, m := range a.markets {
		ab, aa := a.Book(m.ID, 10)
		bb, ba := b.Book(m.ID, 10)
		compareLevels(t, m.ID, "bids", ab, bb)
		compareLevels(t, m.ID, "asks", aa, ba)
	}
	ra, rb := a.OpenReserves(), b.OpenReserves()
	if len(ra) != len(rb) {
		t.Fatalf("reserve count differs: %d vs %d", len(ra), len(rb))
	}
	for k, va := range ra {
		vb, ok := rb[k]
		if !ok || va.Cmp(vb) != 0 {
			t.Fatalf("reserve for %+v differs: %s vs %v", k, va, vb)
		}
	}
}

func compareOrder(t *testing.T, a, b *Order) {
	t.Helper()
	if a.ID != b.ID || a.Seq != b.Seq || a.Market != b.Market || a.Account != b.Account ||
		a.Side != b.Side || a.Type != b.Type || a.TIF != b.TIF || a.Price != b.Price ||
		a.Status != b.Status {
		t.Fatalf("order %d header differs:\n a=%+v\n b=%+v", a.ID, a, b)
	}
	if a.Qty.Cmp(b.Qty) != 0 || a.Filled.Cmp(b.Filled) != 0 || a.reserved.Cmp(b.reserved) != 0 {
		t.Fatalf("order %d amounts differ: qty %s/%s filled %s/%s reserved %s/%s",
			a.ID, a.Qty, b.Qty, a.Filled, b.Filled, a.reserved, b.reserved)
	}
	if (a.MaxNotional == nil) != (b.MaxNotional == nil) {
		t.Fatalf("order %d MaxNotional nil-ness differs", a.ID)
	}
	if a.MaxNotional != nil && a.MaxNotional.Cmp(b.MaxNotional) != 0 {
		t.Fatalf("order %d MaxNotional differs: %s vs %s", a.ID, a.MaxNotional, b.MaxNotional)
	}
}

func compareLevels(t *testing.T, market MarketID, name string, a, b []LevelView) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("market %d %s depth differs: %d vs %d", market, name, len(a), len(b))
	}
	for i := range a {
		if a[i].Price != b[i].Price || a[i].Total.Cmp(b[i].Total) != 0 {
			t.Fatalf("market %d %s level %d differs: %v/%s vs %v/%s",
				market, name, i, a[i].Price, a[i].Total, b[i].Price, b[i].Total)
		}
	}
}

// runCommandsAccounting is runCommands plus the freeze bookkeeping: it records
// every release emitted and the total freeze reserved for accepted orders
// (what the service layer would have frozen via RequiredReserve before Place).
// placedReserve records, per accepted order, the exact reserve that was frozen
// for it at placement so the "reserved never exceeds what was placed" check can
// run.
func runCommandsAccounting(e *Engine, cmds []command) (trades []Trade, released map[ledger.HoldKey]*uint256.Int, reservedAtPlacement *uint256.Int, placedReserve map[OrderID]*uint256.Int) {
	released = make(map[ledger.HoldKey]*uint256.Int)
	reservedAtPlacement = uint256.NewInt(0)
	placedReserve = make(map[OrderID]*uint256.Int)
	addRelease := func(rs []HoldRelease) {
		for _, r := range rs {
			k := ledger.HoldKey{Account: r.Account, Asset: r.Asset}
			if cur, ok := released[k]; ok {
				released[k] = new(uint256.Int).Add(cur, r.Amount)
				continue
			}
			released[k] = new(uint256.Int).Set(r.Amount)
		}
	}
	for _, c := range cmds {
		if c.place {
			asset, amount, err := e.RequiredReserve(c.req)
			if err != nil {
				continue // rejected: nothing is frozen, nothing to release
			}
			_ = asset
			res, err := e.Place(c.req)
			if err != nil {
				// A request that passes RequiredReserve but fails Place would
				// leak the freeze at the service layer, so it must not exist.
				panic(fmt.Sprintf("RequiredReserve accepted but Place rejected: %v", err))
			}
			reservedAtPlacement.Add(reservedAtPlacement, amount)
			if res.Order != nil {
				placedReserve[res.Order.ID] = new(uint256.Int).Set(amount)
			}
			addRelease(res.Releases)
			trades = append(trades, res.Trades...)
			continue
		}
		res, err := e.Cancel(c.market, c.account, c.orderID)
		if err == nil {
			addRelease(res.Releases)
			trades = append(trades, res.Trades...)
		}
	}
	return trades, released, reservedAtPlacement, placedReserve
}

// TestFreezeIsFullyAccounted closes the gap that let a stranded maker freeze
// slip through: checkBookInvariants only inspects orders that are still OPEN,
// so a rounding remainder left on an order that had already LEFT the book was
// invisible. Every unit ever frozen must end up either released or still
// reserved by an open order. It also asserts that no order's live reserve ever
// exceeds the reserve that was frozen for it at placement.
func TestFreezeIsFullyAccounted(t *testing.T) {
	for _, seed := range []uint64{1, 5, 13, 42, 0xDEADBEEF, 0xFEEDFACE} {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			e := newTestEngine(t, testMarkets()...)
			_, released, atPlacement, placedReserve := runCommandsAccounting(e, genCommands(seed, 500))

			total := new(uint256.Int).Set(atPlacement)
			for _, v := range released {
				total.Sub(total, v)
			}
			open := e.OpenReserves()
			for _, v := range open {
				total.Sub(total, v)
			}
			if !total.IsZero() {
				t.Fatalf("freeze not accounted for: %s units placed=%s released=%v stillOpen=%v",
					total, atPlacement, released, open)
			}

			// No order may hold more than the reserve placed for it.
			for id, o := range e.all {
				placed, ok := placedReserve[id]
				if !ok {
					t.Fatalf("order %d has no recorded placement reserve", id)
				}
				if o.reserved.Cmp(placed) > 0 {
					t.Fatalf("order %d reserved %s exceeds placed reserve %s", id, o.reserved, placed)
				}
			}
		})
	}
}

// TestConservationAcrossAccounts walks every produced trade and applies the two
// legs (base seller->buyer, quote buyer->seller), then asserts every asset's
// net change across all accounts is exactly zero.
func TestConservationAcrossAccounts(t *testing.T) {
	for _, seed := range []uint64{11, 22, 33, 44} {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			e := newTestEngine(t, testMarkets()...)
			trades := runCommands(e, genCommands(seed, 400), nil)

			deltas := map[ledger.HoldKey]int64{}
			add := func(a ledger.AccountID, asset ledger.AssetID, d int64) {
				k := ledger.HoldKey{Account: a, Asset: asset}
				deltas[k] += d
			}

			for _, tr := range trades {
				checkTrade(t, e, tr)
				m := e.byID[tr.Market]
				var buyer, seller ledger.AccountID
				if tr.MakerSide == SideBuy {
					buyer, seller = tr.Maker, tr.Taker
				} else {
					buyer, seller = tr.Taker, tr.Maker
				}
				q := int64(tr.Qty.Uint64())
				n := int64(tr.Notional.Uint64())
				add(buyer, m.Base, q)
				add(seller, m.Base, -q)
				add(buyer, m.Quote, -n)
				add(seller, m.Quote, n)
			}

			perAsset := map[ledger.AssetID]int64{}
			for k, v := range deltas {
				perAsset[k.Asset] += v
			}
			for asset, v := range perAsset {
				if v != 0 {
					t.Fatalf("asset %d not conserved: net %d", asset, v)
				}
			}
		})
	}
}

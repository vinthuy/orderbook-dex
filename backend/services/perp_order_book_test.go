package services

import (
	"fmt"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

func perpMarkets() []perp.Market {
	return []perp.Market{{ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 2, MaxLeverage: 10}}
}

func perpAcct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func mustPerpJournal(t *testing.T, dir string) *PerpJournal {
	t.Helper()
	j, err := NewPerpJournal(dir)
	if err != nil {
		t.Fatalf("NewPerpJournal: %v", err)
	}
	// Release the WAL handle before t.TempDir's RemoveAll runs (Windows refuses
	// to unlink an open file).
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestPerpOpenAndCloseSettlesPnL(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	// Fund three accounts with quote (asset 1).
	fund := func(a ledger.AccountID, ref string) {
		t.Helper()
		if err := l.Apply(ledger.DepositOp{Account: a, Asset: 1, Amount: uint256.NewInt(100000), Ref: ref}); err != nil {
			t.Fatalf("fund %s: %v", ref, err)
		}
	}
	fund(perpAcct(1), "d1")
	fund(perpAcct(2), "d2")
	fund(perpAcct(3), "d3")

	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustPerpJournal(t, ""), bus)
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	defer svc.Close()

	// Account 1 opens short 100 @ 100 (freezes margin 10).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open short: %v", err)
	}
	// Account 2 opens long 100 @ 100 (crosses).
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open long: %v", err)
	}

	// Account 2 closes long 100 @ 120 (profit = notional(100,120)-notional(100,100)
	// = 120-100 = 20). The closing order is a SELL, so it crosses resting bids:
	// account 3 must rest a LONG at 120. (A resting short would sit on the ask
	// side and never cross; the plan's original test had the wrong side here.)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("rest long: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 120, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("close long: %v", err)
	}

	// Account 2's quote balance should reflect +20 profit (and margin release).
	bal := l.Balance(perpAcct(2), 1).Uint64()
	// started 100000, opened long (margin 10 frozen, not deducted), closed for +20.
	// The realized profit 20 is credited; margin was frozen then released.
	if bal != 100020 {
		t.Fatalf("account 2 balance = %d, want 100020", bal)
	}
	if held := holds.Held(perpAcct(2), 1); !held.IsZero() {
		t.Fatalf("account 2 held = %s, want 0 (margin must be fully released)", held.Dec())
	}
	if _, ok := svc.Engine().Position(1, perpAcct(2)); ok {
		t.Fatal("account 2 still has a position, want closed")
	}
}

// TestPerpReplayRestoresPositionAndRestingOrderHolds covers the restart path:
// after replaying the WAL, Holds must be reconciled from BOTH position margin
// and the reserved remainder of resting orders. Restoring only positions would
// overstate available balance and make the later Cancel release more than is
// held (ErrHoldUnderflow → fatal degrade).
func TestPerpReplayRestoresPositionAndRestingOrderHolds(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	fund := func(a ledger.AccountID, ref string) {
		t.Helper()
		if err := l.Apply(ledger.DepositOp{Account: a, Asset: 1, Amount: uint256.NewInt(100000), Ref: ref}); err != nil {
			t.Fatalf("fund %s: %v", ref, err)
		}
	}
	fund(perpAcct(1), "r1")
	fund(perpAcct(2), "r2")
	fund(perpAcct(3), "r3")

	dir := t.TempDir()
	j1 := mustPerpJournal(t, dir)
	svc1, err := NewPerpOrderBookService(perpMarkets(), l, holds, j1, bus)
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}

	// Open positions: account 1 short 100 @ 100, account 2 crosses long.
	if _, err := svc1.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open short: %v", err)
	}
	if _, err := svc1.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open long: %v", err)
	}
	// Leave a resting order: account 3 bids 50 @ 80 (reserved 4) with no fill.
	rest, err := svc1.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 80, Qty: uint256.NewInt(50), Leverage: 10})
	if err != nil {
		t.Fatalf("rest long: %v", err)
	}
	if rest.Order == nil || !rest.Order.IsOpen() {
		t.Fatalf("order should rest, got %+v", rest.Order)
	}
	restingID := rest.Order.ID

	// Before restart: position margins 10 + 10, resting reserve 4.
	for acct, want := range map[byte]uint64{1: 10, 2: 10, 3: 4} {
		if got := holds.Held(perpAcct(acct), 1).Uint64(); got != want {
			t.Fatalf("before restart account %d held = %d, want %d", acct, got, want)
		}
	}

	// Simulate a restart: same ledger and holds, journal reopened on the same
	// directory. The constructor replays the WAL and reconciles the freezes.
	if err := svc1.Close(); err != nil {
		t.Fatalf("close first service: %v", err)
	}
	j2 := mustPerpJournal(t, dir)
	svc2, err := NewPerpOrderBookService(perpMarkets(), l, holds, j2, bus)
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	defer svc2.Close()

	// Both positions and the resting order's reserve must be restored.
	for acct, want := range map[byte]uint64{1: 10, 2: 10, 3: 4} {
		if got := holds.Held(perpAcct(acct), 1).Uint64(); got != want {
			t.Fatalf("after restart account %d held = %d, want %d", acct, got, want)
		}
	}
	if p, ok := svc2.Engine().Position(1, perpAcct(1)); !ok || p.Side != perp.SideShort || p.Size.Uint64() != 100 {
		t.Fatalf("account 1 position not restored: %+v", p)
	}
	if p, ok := svc2.Engine().Position(1, perpAcct(2)); !ok || p.Side != perp.SideLong || p.Size.Uint64() != 100 {
		t.Fatalf("account 2 position not restored: %+v", p)
	}

	// Cancelling the replayed resting order must release exactly its reserve,
	// not underflow the hold.
	if _, err := svc2.Cancel(1, perpAcct(3), restingID); err != nil {
		t.Fatalf("cancel restored resting order: %v", err)
	}
	if held := holds.Held(perpAcct(3), 1); !held.IsZero() {
		t.Fatalf("account 3 held = %s, want 0 after cancel", held.Dec())
	}
}

// TestPerpCloseCloseSettlesBothSides covers a trade where BOTH sides are
// reducing positions (maker and taker each close). The engine emits two
// PnLSettlements with the SAME TradeID; the ledger ref must include the account
// or the second settlement is silently deduplicated, destroying or minting the
// difference. It also replays the WAL and asserts the per-account refs stay
// idempotent across restart.
func TestPerpCloseCloseSettlesBothSides(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	fund := func(a ledger.AccountID, ref string) {
		t.Helper()
		if err := l.Apply(ledger.DepositOp{Account: a, Asset: 1, Amount: uint256.NewInt(1000), Ref: ref}); err != nil {
			t.Fatalf("fund %s: %v", ref, err)
		}
	}
	fund(perpAcct(1), "cc1")
	fund(perpAcct(2), "cc2")

	dir := t.TempDir()
	j1 := mustPerpJournal(t, dir)
	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, j1, bus)
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}

	// Build opposite positions at 100: acct2 rests a short, acct1 crosses long.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("open long: %v", err)
	}

	// Both close at 110 in one trade. acct1 (long) rests a closing ask, acct2
	// (short) crosses it with a closing bid, so BOTH fill legs reduce positions.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 110, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("rest close: %v", err)
	}
	res, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong, Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 110, Qty: uint256.NewInt(100), Leverage: 10})
	if err != nil {
		t.Fatalf("close short: %v", err)
	}

	// The engine must emit two settlements for one trade, one per side.
	if len(res.PnL) != 2 {
		t.Fatalf("PnL settlements = %d, want 2 (taker + maker): %+v", len(res.PnL), res.PnL)
	}
	if res.PnL[0].TradeID != res.PnL[1].TradeID {
		t.Fatalf("settlements have different trade ids: %d vs %d", res.PnL[0].TradeID, res.PnL[1].TradeID)
	}
	pnlByAcct := make(map[ledger.AccountID]*uint256.Int, len(res.PnL))
	for _, p := range res.PnL {
		pnlByAcct[p.Account] = p.Profit
	}
	acct1PnL := pnlByAcct[perpAcct(1)] // long 100 closed at 110: +10
	acct2PnL := pnlByAcct[perpAcct(2)] // short 100 closed at 110: -10
	if acct1PnL == nil || acct2PnL == nil {
		t.Fatalf("missing per-account settlements: %+v", res.PnL)
	}
	if acct1PnL.Sign() <= 0 || acct2PnL.Sign() >= 0 {
		t.Fatalf("unexpected PnL signs: acct1=%s acct2=%s, want +10/-10", acct1PnL.Dec(), acct2PnL.Dec())
	}
	if !new(uint256.Int).Add(acct1PnL, acct2PnL).IsZero() {
		t.Fatalf("PnL not zero-sum: acct1=%s acct2=%s", acct1PnL.Dec(), acct2PnL.Dec())
	}

	// Both per-account idempotency keys must have been applied to the ledger.
	tradeID := res.PnL[0].TradeID
	for _, a := range []ledger.AccountID{perpAcct(1), perpAcct(2)} {
		ref := fmt.Sprintf("perp:pnl:%d:%s", tradeID, a.Hex())
		if !l.WasApplied(ref) {
			t.Fatalf("settlement ref %q was not applied", ref)
		}
	}

	// Conservation: 1000 + 1000 in, 1010 + 990 out, total unchanged.
	assertLedger := func(when string) {
		t.Helper()
		if got := l.Balance(perpAcct(1), 1).Uint64(); got != 1010 {
			t.Fatalf("%s: account 1 balance = %d, want 1010", when, got)
		}
		if got := l.Balance(perpAcct(2), 1).Uint64(); got != 990 {
			t.Fatalf("%s: account 2 balance = %d, want 990", when, got)
		}
		if got := l.TotalBalance(1).Uint64(); got != 2000 {
			t.Fatalf("%s: total balance = %d, want 2000 (funds destroyed or minted)", when, got)
		}
	}
	assertLedger("after close")

	for _, a := range []ledger.AccountID{perpAcct(1), perpAcct(2)} {
		if held := holds.Held(a, 1); !held.IsZero() {
			t.Fatalf("account %s held = %s, want 0 after close", a.Hex(), held.Dec())
		}
		if _, ok := svc.Engine().Position(1, a); ok {
			t.Fatalf("account %s still has a position, want closed", a.Hex())
		}
	}

	// Restart must replay the same settlements without paying them twice.
	if err := svc.Close(); err != nil {
		t.Fatalf("close service: %v", err)
	}
	j2 := mustPerpJournal(t, dir)
	svc2, err := NewPerpOrderBookService(perpMarkets(), l, holds, j2, bus)
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	defer svc2.Close()

	assertLedger("after replay")
	for _, a := range []ledger.AccountID{perpAcct(1), perpAcct(2)} {
		if held := holds.Held(a, 1); !held.IsZero() {
			t.Fatalf("after replay account %s held = %s, want 0", a.Hex(), held.Dec())
		}
		if _, ok := svc2.Engine().Position(1, a); ok {
			t.Fatalf("after replay account %s still has a position, want closed", a.Hex())
		}
	}
}

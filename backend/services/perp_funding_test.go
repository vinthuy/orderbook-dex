package services

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// fundingFixture opens a long and a short of equal size in one market so a
// funding round has exactly one payer and one receiver.
func fundingFixture(t *testing.T, journalDir string) (*PerpOrderBookService, *ledger.Ledger, *ledger.Holds) {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	for i, ref := range []string{"d1", "d2", "d3"} {
		acct := perpAcct(byte(i + 1))
		if err := l.Apply(ledger.DepositOp{Account: acct, Asset: 1, Amount: uint256.NewInt(100000), Ref: ref}); err != nil {
			t.Fatalf("fund: %v", err)
		}
	}
	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustPerpJournal(t, journalDir), NewEventBus())
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	// Acct 1 shorts 100 @ 100, acct 2 buys it: long 100, short 100.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	return svc, l, holds
}

func TestSettleFundingMovesQuoteFromLongToShort(t *testing.T) {
	svc, l, _ := fundingFixture(t, "")
	// Market 1 has BaseDecimals 2: notional(qty, price) = qty*price/100.
	// A 100-base position at 100 is 100 quote, and 100 * 0.001 floors to zero,
	// so open a size large enough for the floor to bite.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place short 2: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place long 2: %v", err)
	}
	beforeLong := new(uint256.Int).Set(l.Balance(perpAcct(2), 1))
	beforeShort := new(uint256.Int).Set(l.Balance(perpAcct(1), 1))

	rate, _ := new(big.Int).SetString("1000000000000000", 10) // 0.001
	res, err := svc.SettleFunding(1, 1, 100, rate, 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Legs) != 1 {
		t.Fatalf("want 1 netted leg, got %d", len(res.Legs))
	}
	leg := res.Legs[0]
	if leg.Payer != perpAcct(2) || leg.Receiver != perpAcct(1) {
		t.Fatalf("want long (acct 2) to pay short (acct 1), got %x -> %x", leg.Payer, leg.Receiver)
	}
	// Positions total 10200 base: notional 10200 quote, * 0.001 = 10.2 -> 10.
	want := uint64(10)
	if leg.Amount.Uint64() != want {
		t.Fatalf("want %d, got %s", want, leg.Amount)
	}
	if got := new(uint256.Int).Sub(beforeLong, l.Balance(perpAcct(2), 1)).Uint64(); got != want {
		t.Fatalf("long debited %d, want %d", got, want)
	}
	if got := new(uint256.Int).Sub(l.Balance(perpAcct(1), 1), beforeShort).Uint64(); got != want {
		t.Fatalf("short credited %d, want %d", got, want)
	}
}

func TestSettleFundingNegativeRateReversesDirection(t *testing.T) {
	svc, _, _ := fundingFixture(t, "")
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place short 2: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place long 2: %v", err)
	}
	rate, _ := new(big.Int).SetString("-1000000000000000", 10)
	res, err := svc.SettleFunding(1, 1, 100, rate, 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Legs) != 1 {
		t.Fatalf("want 1 leg, got %d", len(res.Legs))
	}
	if res.Legs[0].Payer != perpAcct(1) || res.Legs[0].Receiver != perpAcct(2) {
		t.Fatalf("negative rate: want short (acct 1) to pay long (acct 2)")
	}
}

func TestSettleFundingZeroRateIsNoOp(t *testing.T) {
	svc, _, _ := fundingFixture(t, "")
	res, err := svc.SettleFunding(1, 1, 100, new(big.Int), 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Legs) != 0 {
		t.Fatalf("want no legs for a zero rate, got %d", len(res.Legs))
	}
}

func TestSettleFundingRecordsDebtWhenPayerIsBroke(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	// Only acct 1 gets quote; acct 2 (the long) starts with nothing.
	// To open a position acct 2 needs margin, so fund it just enough and then
	// drain its balance through a loss-free path: instead, fund both, open, then
	// withdraw the long's remaining balance.
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1, Amount: uint256.NewInt(100000), Ref: "d1"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1, Amount: uint256.NewInt(1000), Ref: "d2"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	defer svc.Close()

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10000), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}
	// Acct 2's balance is 1000 and its 10000-base x 10x position freezes exactly
	// 1000 as margin, so it has no available quote at all: the whole 1000 owed
	// becomes debt.
	rate, _ := new(big.Int).SetString("100000000000000000", 10)
	res, err := svc.SettleFunding(1, 1, 100, rate, 1000)
	if err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if len(res.Skips) != 1 {
		t.Fatalf("want 1 skip, got %d", len(res.Skips))
	}
	if debt := svc.FundingDebtOf(perpAcct(2), 1); debt.Uint64() != 1000 {
		t.Fatalf("want debt 1000 (nothing available), got %s", debt)
	}
	// Funding never degrades the book.
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10), Leverage: 10}); err != nil {
		t.Fatalf("book degraded after a funding shortfall: %v", err)
	}
}

func TestFundingIsIdempotentAcrossReplay(t *testing.T) {
	dir := t.TempDir()
	svc, l, _ := fundingFixture(t, dir)

	rate, _ := new(big.Int).SetString("1000000000000000", 10)
	if _, err := svc.SettleFunding(1, 1, 100, rate, 1000); err != nil {
		t.Fatalf("SettleFunding: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reproduce the same round: the journaled leg must not be paid twice.
	debits := new(uint256.Int).Set(l.Balance(perpAcct(2), 1))

	svc2, err := NewPerpOrderBookService(perpMarkets(), l, ledger.NewHolds(l), mustPerpJournal(t, dir), NewEventBus())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer svc2.Close()
	if got := l.Balance(perpAcct(2), 1); got.Cmp(debits) != 0 {
		t.Fatalf("replay re-paid funding: before %s, after %s", debits, got)
	}
	st, ok := svc2.FundingState(1)
	if !ok || st.LastIndex != 1 {
		t.Fatalf("funding state not restored: %+v ok=%v", st, ok)
	}
}

func TestMarkReplayRestoresPrice(t *testing.T) {
	dir := t.TempDir()
	svc, l, holds := fundingFixture(t, dir)
	if err := svc.journal.AppendMark(MarkCmd{Market: 1, Index: 100, Mark: 101, At: 500}); err != nil {
		t.Fatalf("AppendMark: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	svc2, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustPerpJournal(t, dir), NewEventBus())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer svc2.Close()
	mark, ok := svc2.MarkPrice(1)
	if !ok || mark != 101 {
		t.Fatalf("want restored mark 101, got %d (ok=%v)", mark, ok)
	}
}

func TestUnrealizedPnLRequiresMark(t *testing.T) {
	svc, _, _ := fundingFixture(t, "")
	if _, ok := svc.UnrealizedPnL(1, perpAcct(2)); ok {
		t.Fatal("want no unrealized PnL without a mark")
	}
}

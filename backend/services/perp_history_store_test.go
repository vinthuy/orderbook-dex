package services

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestHistoryPersistsAcrossRestart is the gap this closes: every row used to be
// in memory, so a deploy erased a user's trade and funding history.
func TestHistoryPersistsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")

	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		FeeRates: perp.FeeRates{TakerE18: uint256.NewInt(500000000000000)},
	}}
	for i, ref := range []string{"d1", "d2"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(10000000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	if err := svc.History().Attach(store); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	before := svc.History().Trades(perpAcct(2), nil, 10)
	if len(before) != 1 {
		t.Fatalf("before restart: %d rows, want 1", len(before))
	}
	if before[0].Fee == nil || before[0].Fee.Uint64() != 50 {
		t.Fatalf("before restart: fee = %v, want 50", before[0].Fee)
	}
	if err := svc.History().Close(); err != nil {
		t.Fatal(err)
	}
	_ = svc.Close()

	// Reopen and hydrate a fresh history from the same file.
	store2, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	h2 := NewPerpHistory()
	if err := h2.Attach(store2); err != nil {
		t.Fatalf("Attach after restart: %v", err)
	}
	defer h2.Close()

	after := h2.Trades(perpAcct(2), nil, 10)
	if len(after) != 1 {
		t.Fatalf("after restart: %d rows, want 1", len(after))
	}
	if after[0].TradeID != before[0].TradeID || after[0].Price != 100 {
		t.Fatalf("after restart row = %+v", after[0])
	}
	if after[0].Fee == nil || after[0].Fee.Uint64() != 50 {
		t.Fatalf("after restart: fee = %v, want the 50 that was charged", after[0].Fee)
	}
	if after[0].Role != perp.FeeTaker {
		t.Fatalf("after restart: role = %s, want taker", after[0].Role)
	}
}

// TestHistoryPersistsNegativePnL pins that a signed loss survives the round trip
// through JSON as a negative number, not as two's complement.
func TestHistoryPersistsNegativePnL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h := NewPerpHistory()
	if err := h.Attach(store); err != nil {
		t.Fatal(err)
	}

	a := perpAcct(3)
	h.RecordTrade(perp.Trade{
		ID: 7, Market: 1, Price: 100,
		Qty: uint256.NewInt(10), Notional: uint256.NewInt(1000),
		Maker: a, Taker: perpAcct(4), MakerSide: perp.SideLong,
	}, time.Now())
	h.AttachSettlement(7, a, nil, big.NewInt(-12345))
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h2 := NewPerpHistory()
	if err := h2.Attach(store2); err != nil {
		t.Fatal(err)
	}
	defer h2.Close()

	rows := h2.Trades(a, nil, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].RealizedPnL == nil || rows[0].RealizedPnL.Cmp(big.NewInt(-12345)) != 0 {
		t.Fatalf("realized PnL after restart = %v, want -12345", rows[0].RealizedPnL)
	}
}

// TestHistoryWithoutStoreStaysMemoryOnly pins that a nil path keeps the old
// behaviour rather than failing.
func TestHistoryWithoutStoreStaysMemoryOnly(t *testing.T) {
	store, err := OpenHistoryStore("")
	if err != nil {
		t.Fatalf("OpenHistoryStore(\"\"): %v", err)
	}
	if store != nil {
		t.Fatal("an empty path must yield a nil store")
	}
	h := NewPerpHistory()
	if err := h.Attach(store); err != nil {
		t.Fatalf("Attach(nil): %v", err)
	}
	h.RecordTrade(perp.Trade{ID: 1, Market: 1, Price: 100, Qty: uint256.NewInt(1),
		Notional: uint256.NewInt(100), Maker: perpAcct(1), Taker: perpAcct(2)}, time.Now())
	if rows := h.Trades(perpAcct(1), nil, 10); len(rows) != 1 {
		t.Fatalf("memory-only history should still work, got %d rows", len(rows))
	}
}

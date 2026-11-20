package services

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/perp"
)

// TestHistorySettlementAttachesAfterRestart pins the crash window: a fill can be
// persisted before its fee and PnL settle. After a restart the row must still be
// findable, or the settlement is silently dropped and the user sees a blank fee
// forever.
func TestHistorySettlementAttachesAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	a := perpAcct(5)

	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h := NewPerpHistory()
	if err := h.Attach(store); err != nil {
		t.Fatal(err)
	}
	// Record the bare fill only: no settlement yet.
	h.RecordTrade(perp.Trade{
		ID: 42, Market: 1, Price: 100,
		Qty: uint256.NewInt(1000), Notional: uint256.NewInt(100000),
		Maker: a, Taker: perpAcct(6), MakerSide: perp.SideShort,
	}, time.Now())
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart, then settle as a WAL replay would.
	store2, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h2 := NewPerpHistory()
	if err := h2.Attach(store2); err != nil {
		t.Fatal(err)
	}
	defer h2.Close()

	h2.AttachSettlement(42, a, uint256.NewInt(17), big.NewInt(-250))

	rows := h2.Trades(a, nil, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Fee == nil || rows[0].Fee.Uint64() != 17 {
		t.Fatalf("fee after restart-settlement = %v, want 17 (the settlement was dropped)", rows[0].Fee)
	}
	if rows[0].RealizedPnL == nil || rows[0].RealizedPnL.Cmp(big.NewInt(-250)) != 0 {
		t.Fatalf("pnl after restart-settlement = %v, want -250", rows[0].RealizedPnL)
	}
}

// TestHistorySettlementPersistsAfterRestartSettlement pins that the enriched row is
// itself durable, not only in memory.
func TestHistorySettlementPersistsAfterRestartSettlement(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	a := perpAcct(5)

	store, _ := OpenHistoryStore(dbPath)
	h := NewPerpHistory()
	if err := h.Attach(store); err != nil {
		t.Fatal(err)
	}
	h.RecordTrade(perp.Trade{ID: 7, Market: 1, Price: 100,
		Qty: uint256.NewInt(10), Notional: uint256.NewInt(1000),
		Maker: a, Taker: perpAcct(6), MakerSide: perp.SideLong}, time.Now())
	h.AttachSettlement(7, a, uint256.NewInt(3), nil)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	store2, _ := OpenHistoryStore(dbPath)
	h2 := NewPerpHistory()
	if err := h2.Attach(store2); err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	rows := h2.Trades(a, nil, 1)
	if len(rows) != 1 || rows[0].Fee == nil || rows[0].Fee.Uint64() != 3 {
		t.Fatalf("fee did not survive: %+v", rows)
	}
}

package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestUnfillableLiquidationWritesNoJournalRecord is the durability half of the
// unfillable case: a scan that cannot close anything must not grow the WAL, or a
// position stuck on a thin book accumulates unbounded garbage records.
func TestUnfillableLiquidationWritesNoJournalRecord(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	for i, ref := range []string{"d1", "d2"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(1_000_000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}

	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, dir), NewEventBus())
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

	src := NewStaticPriceSource()
	src.Set(1, 80)
	svc.SetPriceSource(src, PriceOptions{})
	for i := 0; i < 5; i++ {
		svc.tick(time.Unix(int64(2000+i), 0))
	}

	path := filepath.Join(dir, "perp_orders.wal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	lines := 0
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines++
		}
	}
	t.Logf("wal records: %d", lines)
	// Two places per market plus mark records; the point is that NO liquidation
	// record exists for a close that never filled.
	if strings.Contains(string(data), `"kind":"liquidation"`) {
		t.Fatal("an unfillable liquidation was journaled")
	}
}

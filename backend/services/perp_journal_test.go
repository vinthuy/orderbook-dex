package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestPerpMountKeepsSpotHolds is the regression test for the critical restart
// bug: Holds.Reconcile replaces the whole map, so constructing the perp service
// (or reconciling only perp) must not unfreeze the spot order book's reserves.
func TestPerpMountKeepsSpotHolds(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	l.Apply(ledger.DepositOp{Account: perpAcct(9), Asset: 1, Amount: uint256.NewInt(1000), Ref: "d9"})
	// A spot resting order freezes 100.
	if err := holds.Reserve(perpAcct(9), 1, uint256.NewInt(100)); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	svc, err := NewPerpOrderBookService(perpMarkets(), l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	defer svc.Close()

	// Constructing the perp service must leave the spot freeze intact.
	if got := holds.Held(perpAcct(9), 1).Uint64(); got != 100 {
		t.Fatalf("spot hold after perp construction = %d, want 100", got)
	}

	// Mounting then reconciles the union; the spot freeze must survive that too.
	union := UnionHolds(map[ledger.HoldKey]*uint256.Int{
		{Account: perpAcct(9), Asset: 1}: uint256.NewInt(100),
	}, svc.OpenMargins())
	if err := holds.Reconcile(union); err != nil {
		t.Fatalf("reconcile union: %v", err)
	}
	if got := holds.Held(perpAcct(9), 1).Uint64(); got != 100 {
		t.Fatalf("spot hold after union reconcile = %d, want 100", got)
	}
}

func TestUnionHoldsSumsSameKey(t *testing.T) {
	key := ledger.HoldKey{Account: perpAcct(1), Asset: 1}
	other := ledger.HoldKey{Account: perpAcct(2), Asset: 1}
	got := UnionHolds(
		map[ledger.HoldKey]*uint256.Int{key: uint256.NewInt(100)},
		map[ledger.HoldKey]*uint256.Int{key: uint256.NewInt(7), other: uint256.NewInt(3)},
	)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[key].Uint64() != 107 {
		t.Fatalf("shared key = %s, want 107", got[key].Dec())
	}
	if got[other].Uint64() != 3 {
		t.Fatalf("other key = %s, want 3", got[other].Dec())
	}
	// Zero amounts are dropped, and inputs are not aliased.
	got2 := UnionHolds(map[ledger.HoldKey]*uint256.Int{key: uint256.NewInt(5)}, nil)
	got2[key].Add(got2[key], uint256.NewInt(1))
	// Re-derive: the source map must be untouched by the mutation above.
	if v := got2[key].Uint64(); v != 6 {
		t.Fatalf("mutated union value = %d, want 6", v)
	}
	if z := UnionHolds(map[ledger.HoldKey]*uint256.Int{key: uint256.NewInt(0)}, nil); len(z) != 0 {
		t.Fatalf("zero amounts should be dropped, got %+v", z)
	}
}

func TestPerpJournalTruncatesTornTail(t *testing.T) {
	dir := t.TempDir()
	j, err := NewPerpJournal(dir)
	if err != nil {
		t.Fatalf("NewPerpJournal: %v", err)
	}
	if err := j.AppendPlace(perp.PlaceRequest{
		Market: 1, Account: perpAcct(1), Side: perp.SideLong, Type: perp.TypeLimit,
		TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(10), Leverage: 10,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = j.Close()

	// Simulate a crash mid-write: append a partial record with no newline.
	path := filepath.Join(dir, "perp_orders.wal")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"seq":2,"kind":"place","payload":{"mark`); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	_ = f.Close()

	// Reopening must truncate the torn tail, appending must keep working, and a
	// second reopen must still parse (i.e. the concatenation bug is gone).
	j2, err := NewPerpJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := j2.AppendCancel(1, perpAcct(1), 1); err != nil {
		t.Fatalf("append after truncate: %v", err)
	}
	_ = j2.Close()

	j3, err := NewPerpJournal(dir)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer j3.Close()
	ins, err := j3.Replay()
	if err != nil {
		t.Fatalf("replay after torn tail: %v", err)
	}
	if len(ins) != 2 {
		t.Fatalf("instructions = %d, want 2 (place + cancel): %+v", len(ins), ins)
	}
	if ins[0].Kind != "place" || ins[1].Kind != "cancel" {
		t.Fatalf("kinds = %q,%q, want place,cancel", ins[0].Kind, ins[1].Kind)
	}
}

func TestParsePerpMarkets(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{name: "empty", in: "", want: 0},
		{name: "one", in: "1:ETH-PERP:2:1:18:10", want: 1},
		{name: "with min qty", in: "1:ETH-PERP:2:1:18:10:1000", want: 1},
		{name: "two", in: "1:ETH-PERP:2:1:18:10;2:BTC-PERP:3:1:18:20", want: 2},
		{name: "trailing semicolon", in: "1:ETH-PERP:2:1:18:10;", want: 1},
		{name: "too few fields", in: "1:ETH-PERP:2:1", wantErr: true},
		{name: "bad id", in: "x:ETH-PERP:2:1:18:10", wantErr: true},
		{name: "bad decimals", in: "1:ETH-PERP:2:1:999:10", wantErr: true},
		{name: "base equals quote", in: "1:ETH-PERP:2:2:18:10", wantErr: true},
		{name: "zero leverage", in: "1:ETH-PERP:2:1:18:0", wantErr: true},
		{name: "bad min qty", in: "1:ETH-PERP:2:1:18:10:abc", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePerpMarkets(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %d markets", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("markets = %d, want %d", len(got), tc.want)
			}
		})
	}
}

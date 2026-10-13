package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
)

func jacct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func jreq() matching.PlaceRequest {
	return matching.PlaceRequest{
		Market:      1,
		Account:     jacct(7),
		Side:        matching.SideBuy,
		Type:        matching.TypeLimit,
		TIF:         matching.TIFGTC,
		Price:       300,
		Qty:         uint256.NewInt(10),
		MaxNotional: uint256.NewInt(30),
	}
}

func writeJournalFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, journalFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write journal: %v", err)
	}
}

func TestJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()

	if err := j.Append(PlaceEntry(jreq())); err != nil {
		t.Fatalf("append place: %v", err)
	}
	if err := j.Append(CancelEntry(1, jacct(7), 5)); err != nil {
		t.Fatalf("append cancel: %v", err)
	}

	entries, err := j.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	p := entries[0]
	if p.Seq != 1 || p.Kind != kindPlace || p.Place == nil {
		t.Fatalf("entry 0 = %+v, want seq 1 place", p)
	}
	if p.Place.Market != 1 || p.Place.Account != jacct(7) || p.Place.Side != matching.SideBuy ||
		p.Place.Type != matching.TypeLimit || p.Place.TIF != matching.TIFGTC || p.Place.Price != 300 {
		t.Fatalf("place header lost in round trip: %+v", p.Place)
	}
	if p.Place.Qty.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("qty = %s, want 10", p.Place.Qty)
	}
	if p.Place.MaxNotional == nil || p.Place.MaxNotional.Cmp(uint256.NewInt(30)) != 0 {
		t.Fatalf("max notional = %v, want 30", p.Place.MaxNotional)
	}

	c := entries[1]
	if c.Seq != 2 || c.Kind != kindCancel || c.Cancel == nil {
		t.Fatalf("entry 1 = %+v, want seq 2 cancel", c)
	}
	if c.Cancel.Market != 1 || c.Cancel.Account != jacct(7) || c.Cancel.OrderID != 5 {
		t.Fatalf("cancel lost in round trip: %+v", c.Cancel)
	}

	// Request() must hand the kernel identical, independent inputs.
	req := p.Place.Request()
	if req.Market != 1 || req.Price != 300 || req.Qty.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("Request() = %+v", req)
	}
	req.Qty.Add(req.Qty, uint256.NewInt(1))
	if p.Place.Qty.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("Request() aliased the command's Qty")
	}
}

func TestJournalTornTailIgnored(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chop    int
		wantErr bool
	}{
		{"missing newline only", 1, false},
		{"half written record", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			j, err := NewJournal(dir)
			if err != nil {
				t.Fatalf("NewJournal: %v", err)
			}
			if err := j.Append(PlaceEntry(jreq())); err != nil {
				t.Fatalf("append 1: %v", err)
			}
			if err := j.Append(CancelEntry(1, jacct(7), 5)); err != nil {
				t.Fatalf("append 2: %v", err)
			}
			if err := j.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			path := filepath.Join(dir, journalFileName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := os.WriteFile(path, data[:len(data)-tc.chop], 0o644); err != nil {
				t.Fatalf("truncate: %v", err)
			}

			j2, err := NewJournal(dir)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer j2.Close()
			entries, err := j2.Replay()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("a torn tail must be tolerated, got: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1 (torn tail dropped)", len(entries))
			}
			if entries[0].Kind != kindPlace || entries[0].Seq != 1 {
				t.Fatalf("surviving entry = %+v", entries[0])
			}
		})
	}
}

// TestJournalInteriorCorruptionErrors is the fail-loud half of the torn-tail
// rule: a bad record with a committed record after it means real data loss.
func TestJournalInteriorCorruptionErrors(t *testing.T) {
	dir := t.TempDir()
	writeJournalFile(t, dir,
		`{"seq":1,"kind":"cancel","payload":{"market":1,"account":"0x1111111111111111111111111111111111111111","order_id":1}}`+"\n"+
			`{this is not json}`+"\n"+
			`{"seq":3,"kind":"cancel","payload":{"market":1,"account":"0x1111111111111111111111111111111111111111","order_id":3}}`+"\n")

	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()
	if _, err := j.Replay(); err == nil {
		t.Fatal("interior corruption must be an error, got nil")
	} else if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error %q should name the corrupt line", err)
	}
}

func TestJournalSeqNotIncreasingErrors(t *testing.T) {
	dir := t.TempDir()
	writeJournalFile(t, dir,
		`{"seq":2,"kind":"cancel","payload":{"market":1,"account":"0x1111111111111111111111111111111111111111","order_id":1}}`+"\n"+
			`{"seq":2,"kind":"cancel","payload":{"market":1,"account":"0x1111111111111111111111111111111111111111","order_id":2}}`+"\n")

	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()
	if _, err := j.Replay(); err == nil {
		t.Fatal("a non-increasing seq must be an error, got nil")
	}
}

func TestJournalUnknownKindErrors(t *testing.T) {
	dir := t.TempDir()
	writeJournalFile(t, dir, `{"seq":1,"kind":"bogus","payload":{}}`+"\n")

	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()
	if _, err := j.Replay(); err == nil {
		t.Fatal("an unknown kind must be an error, got nil")
	}
}

// TestJournalMemoryMode pins dir == "" semantics: Append persists nothing and
// Replay returns nothing.
func TestJournalMemoryMode(t *testing.T) {
	j, err := NewJournal("")
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	if err := j.Append(PlaceEntry(jreq())); err != nil {
		t.Fatalf("memory append must be a no-op, got: %v", err)
	}
	entries, err := j.Replay()
	if err != nil {
		t.Fatalf("memory Replay: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("memory Replay = %d entries, want 0", len(entries))
	}
	if err := j.Close(); err != nil {
		t.Fatalf("memory Close: %v", err)
	}
}

// TestJournalAppendAfterClose guards the closed-state contract (and that Close
// is idempotent).
func TestJournalAppendAfterClose(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("second Close must be idempotent: %v", err)
	}
	if err := j.Append(PlaceEntry(jreq())); err != ErrJournalClosed {
		t.Fatalf("Append after Close = %v, want ErrJournalClosed", err)
	}
}

package ledger

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverFromWAL(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(30), Ref: "w1"})
	wantRoot := l.Root()
	wantVersion := l.Version()
	_ = store.Close()

	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store2: %v", err)
	}
	defer store2.Close()
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 70 {
		t.Fatalf("balance = %d, want 70", got)
	}
	if rec.Version() != wantVersion {
		t.Fatalf("version = %d, want %d", rec.Version(), wantVersion)
	}
	if rec.Root() != wantRoot {
		t.Fatal("recovered root != original root")
	}
}

func TestSnapshotThenReplay(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	if err := l.Snap(); err != nil {
		t.Fatalf("snap: %v", err)
	}
	_ = l.Apply(DepositOp{Account: b, Asset: 1, Amount: amt(50), Ref: "d2"})
	wantRoot := l.Root()
	_ = store.Close()

	store2, _ := NewFileStore(dir)
	defer store2.Close()
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rec.Balance(a, 1).Uint64() != 100 || rec.Balance(b, 1).Uint64() != 50 {
		t.Fatalf("unexpected balances a=%s b=%s", rec.Balance(a, 1), rec.Balance(b, 1))
	}
	if rec.Root() != wantRoot {
		t.Fatal("recovered root != original root")
	}
}

func TestRecoveredLedgerStillIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = store.Close()

	store2, _ := NewFileStore(dir)
	defer store2.Close()
	rec, _ := Recover(store2)
	if err := rec.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"}); err != nil {
		t.Fatalf("replay after recover: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
}

// TestSnapshotPathStillIdempotent covers the snapshot recovery path: an op whose
// key was sealed into the snapshot must remain idempotent after Recover.
func TestSnapshotPathStillIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	if err := l.Snap(); err != nil {
		t.Fatalf("snap: %v", err)
	}
	_ = l.Apply(DepositOp{Account: b, Asset: 1, Amount: amt(50), Ref: "d2"})
	wantRoot := l.Root()
	wantVersion := l.Version()
	_ = store.Close()

	store2, _ := NewFileStore(dir)
	defer store2.Close()
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	// Re-apply the key already captured by the snapshot: it must be a no-op.
	if err := rec.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"}); err != nil {
		t.Fatalf("re-apply snapshotted key: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 (no double credit)", got)
	}
	if rec.Root() != wantRoot {
		t.Fatal("root changed after re-applying a snapshotted key")
	}
	if rec.Version() != wantVersion {
		t.Fatalf("version = %d, want %d", rec.Version(), wantVersion)
	}
}

// TestRecoverTornTailMidRecord simulates a crash during AppendBatch's single
// Write: the third record is cut off mid-way. Recovery must succeed with the
// first two records and ignore the torn tail.
func TestRecoverTornTailMidRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(50), Ref: "d2"})
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(25), Ref: "d3"})
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	path := filepath.Join(dir, "wal.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		t.Fatal("precondition: wal should contain a trailing newline")
	}
	prevNL := bytes.LastIndexByte(data[:lastNL], '\n')
	start := prevNL + 1
	// Cut in the middle of the final record's bytes.
	mid := start + (lastNL-start)/2
	if err := os.Truncate(path, int64(mid)); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store2: %v", err)
	}
	defer store2.Close()
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover with torn tail: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 150 {
		t.Fatalf("balance = %d, want 150 (first two records only)", got)
	}
	if rec.Version() != 2 {
		t.Fatalf("version = %d, want 2", rec.Version())
	}
}

// TestReplayInteriorCorruptionFails guards the other half of the contract:
// corruption with valid records still after it is real data loss and must not
// be swallowed.
func TestReplayInteriorCorruptionFails(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(50), Ref: "d2"})
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(25), Ref: "d3"})
	_ = store.Close()

	path := filepath.Join(dir, "wal.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) < 4 {
		t.Fatalf("precondition: expected 3 records, got %d lines", len(lines))
	}
	// Corrupt the middle record, leaving the third record intact after it.
	lines[1] = []byte("{ this is not valid json")
	if err := os.WriteFile(path, bytes.Join(lines, []byte{'\n'}), 0o644); err != nil {
		t.Fatalf("write corrupt wal: %v", err)
	}

	store2, _ := NewFileStore(dir)
	defer store2.Close()
	if _, err := store2.Replay(0); err == nil {
		t.Fatal("Replay: expected error for interior corruption, got nil")
	}
	if _, err := Recover(store2); err == nil {
		t.Fatal("Recover: expected error for interior corruption, got nil")
	}
}

// TestRecoverTrailingRecordWithoutNewline covers the second torn-tail signal:
// the file's last byte is not '\n'. The unterminated final record was never
// committed (AppendBatch writes record+newline together and only Syncs once),
// so recovery must succeed and ignore it.
func TestRecoverTrailingRecordWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	l := New().WithStore(store)
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(50), Ref: "d2"})
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(25), Ref: "d3"})
	_ = store.Close()

	path := filepath.Join(dir, "wal.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("precondition: wal should end with newline")
	}
	// Drop only the final newline; the third record stays complete JSON.
	if err := os.Truncate(path, int64(len(data)-1)); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store2, _ := NewFileStore(dir)
	defer store2.Close()
	rec, err := Recover(store2)
	if err != nil {
		t.Fatalf("recover with unterminated final record: %v", err)
	}
	if got := rec.Balance(a, 1).Uint64(); got != 150 {
		t.Fatalf("balance = %d, want 150 (unterminated final record ignored)", got)
	}
	if rec.Version() != 2 {
		t.Fatalf("version = %d, want 2", rec.Version())
	}
}

package ledger

import "testing"

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

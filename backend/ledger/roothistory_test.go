package ledger

import (
	"testing"

	"github.com/holiman/uint256"
)

func deposit(t *testing.T, l *Ledger, a AccountID, asset AssetID, amt uint64, ref string) {
	t.Helper()
	if err := l.Apply(DepositOp{Account: a, Asset: asset, Amount: uint256.NewInt(amt), Ref: ref}); err != nil {
		t.Fatal(err)
	}
}

// TestRootHistoryProofVerifiesAgainstItsOwnRoot is the property that matters: a
// proof built from a retained record must verify against that record's root, and
// the balance handed to the contract must be the one the leaf commits to.
func TestRootHistoryProofVerifiesAgainstItsOwnRoot(t *testing.T) {
	l := New()
	alice := accountOf(0xA1)
	bob := accountOf(0xB2)
	deposit(t, l, alice, 1, 100, "d1")
	deposit(t, l, bob, 1, 200, "d2")

	h := NewRootHistory(8)
	rec := h.Record(l, 1000)

	bal, proof, err := rec.Proof(alice, 1)
	if err != nil {
		t.Fatalf("Proof: %v", err)
	}
	if bal.Uint64() != 100 {
		t.Fatalf("balance = %s, want 100", bal)
	}
	leaf := leafHash(alice, 1, bal)
	if !verifyProof(proof, rec.Root, leaf) {
		t.Fatal("proof does not verify against its own root")
	}
}

// TestRootHistoryProofSurvivesLaterChanges is the gap this closes: after the
// ledger moves on, a proof against the OLD anchored root must still verify. Before
// RootHistory, Ledger.Proof only ever answered for the current state.
func TestRootHistoryProofSurvivesLaterChanges(t *testing.T) {
	l := New()
	alice := accountOf(0xA1)
	deposit(t, l, alice, 1, 100, "d1")

	h := NewRootHistory(8)
	old := h.Record(l, 1000)

	// The ledger moves on: alice's balance changes, the current root changes.
	deposit(t, l, accountOf(0xC3), 1, 500, "d2")
	currentRoot, _ := l.RootAndVersion()
	if currentRoot == old.Root {
		t.Fatal("precondition: the root should have changed")
	}

	bal, proof, err := old.Proof(alice, 1)
	if err != nil {
		t.Fatalf("historical Proof: %v", err)
	}
	if bal.Uint64() != 100 {
		t.Fatalf("historical balance = %s, want 100 (the value at anchor time)", bal)
	}
	if !verifyProof(proof, old.Root, leafHash(alice, 1, bal)) {
		t.Fatal("historical proof must still verify against the old root")
	}
	// And it must NOT verify against the new one.
	if verifyProof(proof, currentRoot, leafHash(alice, 1, bal)) {
		t.Fatal("a proof for the old root must not verify against the new root")
	}
}

// TestRootHistoryLatestAtOrBefore pins the lookup a client uses: "my balance in
// the newest state I can prove against".
func TestRootHistoryLatestAtOrBefore(t *testing.T) {
	l := New()
	alice := accountOf(0xA1)
	h := NewRootHistory(8)

	deposit(t, l, alice, 1, 100, "d1")
	r1 := h.Record(l, 1000)
	deposit(t, l, alice, 1, 50, "d2")
	r2 := h.Record(l, 2000)

	got, ok := h.LatestAtOrBefore(r2.Version)
	if !ok || got.Version != r2.Version {
		t.Fatalf("want the latest record, got %d (ok=%v)", got.Version, ok)
	}
	got, ok = h.LatestAtOrBefore(r1.Version)
	if !ok || got.Version != r1.Version {
		t.Fatalf("want the first record, got %d (ok=%v)", got.Version, ok)
	}
	// A version before anything anchored has no record.
	if _, ok := h.LatestAtOrBefore(0); ok {
		t.Fatal("want no record before the first anchor")
	}
}

func TestRootHistoryRecordIsIdempotentForAVersion(t *testing.T) {
	l := New()
	deposit(t, l, accountOf(0xA1), 1, 100, "d1")
	h := NewRootHistory(8)
	a := h.Record(l, 1000)
	b := h.Record(l, 2000)
	if a.Version != b.Version {
		t.Fatalf("same ledger version recorded twice gave %d then %d", a.Version, b.Version)
	}
	if len(h.All()) != 1 {
		t.Fatalf("want 1 record, got %d", len(h.All()))
	}
}

func TestRootHistoryEvictsOldestBeyondMax(t *testing.T) {
	l := New()
	h := NewRootHistory(2)
	alice := accountOf(0xA1)

	deposit(t, l, alice, 1, 1, "d1")
	r1 := h.Record(l, 1000)
	deposit(t, l, alice, 1, 1, "d2")
	r2 := h.Record(l, 2000)
	deposit(t, l, alice, 1, 1, "d3")
	r3 := h.Record(l, 3000)

	all := h.All()
	if len(all) != 2 {
		t.Fatalf("want 2 retained records, got %d", len(all))
	}
	if _, ok := h.Version(r1.Version); ok {
		t.Fatal("the oldest record should have been evicted")
	}
	if _, ok := h.Version(r2.Version); !ok {
		t.Fatal("r2 should be retained")
	}
	if _, ok := h.Version(r3.Version); !ok {
		t.Fatal("r3 should be retained")
	}
}

func TestRootHistoryProofForMissingLeaf(t *testing.T) {
	l := New()
	deposit(t, l, accountOf(0xA1), 1, 100, "d1")
	h := NewRootHistory(8)
	rec := h.Record(l, 1000)
	if _, _, err := rec.Proof(accountOf(0xEE), 1); err == nil {
		t.Fatal("want ErrNoLeaf for an account with no balance")
	}
}

// TestRootHistoryEmptyLedger pins that recording an empty ledger yields the zero
// root rather than panicking.
func TestRootHistoryEmptyLedger(t *testing.T) {
	h := NewRootHistory(4)
	rec := h.Record(New(), 1000)
	if rec.Root != ([32]byte{}) {
		t.Fatalf("want the zero root for an empty ledger, got %x", rec.Root)
	}
}

// accountOf builds an account from one byte, matching the other ledger tests.
func accountOf(b byte) AccountID {
	var a AccountID
	a[19] = b
	return a
}

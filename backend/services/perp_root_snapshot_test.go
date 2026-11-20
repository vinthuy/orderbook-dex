package services

import (
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// TestRootSnapshotMatchesCommittedRootAcrossConcurrentApplies is the H1 regression:
// a snapshot taken by a separate later read could describe a different ledger
// version than the root that was committed, leaving the anchored version
// permanently unprovable.
//
// The production window is the on-chain commit latency; here the same interleaving
// is reproduced deterministically by applying between taking the snapshot and
// storing it.
func TestRootSnapshotMatchesCommittedRootAcrossConcurrentApplies(t *testing.T) {
	l := ledger.New()
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(1), Asset: 1,
		Amount: uint256.NewInt(1000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}

	h := ledger.NewRootHistory(16)
	// RecordWithin is one read: the root it returns and the leaves it stores describe
	// the same version, no matter what happens next.
	rec, version, root := h.RecordWithin(l, time.Now().Unix())

	// The ledger moves on, exactly as a fill would during commit latency.
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(2), Asset: 1,
		Amount: uint256.NewInt(5000), Ref: "d2"}); err != nil {
		t.Fatal(err)
	}

	if rec.Root != root {
		t.Fatal("the record root and the returned root must be the same value")
	}
	if rec.Version != version {
		t.Fatal("the record version and the returned version must agree")
	}

	// The snapshot must still describe the ORIGINAL state: a proof built from it
	// verifies against the committed root, which is what withdraw() checks.
	bal, proof, err := rec.Proof(perpAcct(1), 1)
	if err != nil {
		t.Fatalf("Proof: %v", err)
	}
	if bal.Uint64() != 1000 {
		t.Fatalf("snapshot balance = %s, want the 1000 at snapshot time", bal)
	}
	if !ledger.VerifyProofForTest(proof, root, perpAcct(1), 1, bal) {
		t.Fatal("a proof from the snapshot must verify against the root taken with it")
	}
}

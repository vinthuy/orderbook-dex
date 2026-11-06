package services

import (
	"errors"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// anchorFixture builds an anchor service over a ledger, without a settlement
// contract: the proof path is what is under test, and the on-chain commit is not
// part of producing a proof.
func anchorFixture(t *testing.T) (*StateAnchorService, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	svc := NewStateAnchorService(nil, l)
	return svc, l
}

// anchorDirect records a version as anchored without going through the chain,
// mirroring what AnchorOnce does after a successful commit.
func anchorDirect(s *StateAnchorService, l *ledger.Ledger, rootIndex uint64) uint64 {
	root, version := l.RootAndVersion()
	s.history.Record(l, time.Now().Unix())
	s.mu.Lock()
	s.records = append(s.records, RootRecord{Root: root, Version: version, RootIndex: rootIndex})
	s.byVer[version] = rootIndex
	s.mu.Unlock()
	return version
}

// TestProofAtProducesAUsableWithdrawalTriple pins the endpoint's contract: balance,
// proof and rootIndex together, against a root that was actually anchored.
func TestProofAtProducesAUsableWithdrawalTriple(t *testing.T) {
	svc, l := anchorFixture(t)
	alice := perpAcct(7)
	if err := l.Apply(ledger.DepositOp{Account: alice, Asset: 1,
		Amount: uint256.NewInt(1000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	version := anchorDirect(svc, l, 3)

	p, err := svc.ProofAt(alice, 1, version)
	if err != nil {
		t.Fatalf("ProofAt: %v", err)
	}
	if p.Balance.Uint64() != 1000 {
		t.Fatalf("balance = %s, want 1000", p.Balance)
	}
	if p.RootIndex != 3 {
		t.Fatalf("rootIndex = %d, want 3", p.RootIndex)
	}
	if p.Version != version {
		t.Fatalf("version = %d, want %d", p.Version, version)
	}
	if verifyProofForTest(p.Proof, p.Root, alice, 1, p.Balance) != true {
		t.Fatal("the proof must verify against the returned root")
	}
}

// TestProofAtKeepsWorkingAfterTheLedgerMoves is the whole point of the feature:
// the anchored root stays provable even after the live state diverges from it.
func TestProofAtKeepsWorkingAfterTheLedgerMoves(t *testing.T) {
	svc, l := anchorFixture(t)
	alice := perpAcct(7)
	if err := l.Apply(ledger.DepositOp{Account: alice, Asset: 1,
		Amount: uint256.NewInt(1000), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	v1 := anchorDirect(svc, l, 1)

	// The ledger moves on substantially.
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(8), Asset: 1,
		Amount: uint256.NewInt(5000), Ref: "d2"}); err != nil {
		t.Fatal(err)
	}
	v2 := anchorDirect(svc, l, 2)
	if v1 == v2 {
		t.Fatal("precondition: the versions should differ")
	}

	// The OLD version must still be provable at its old balance.
	p, err := svc.ProofAt(alice, 1, v1)
	if err != nil {
		t.Fatalf("historical ProofAt: %v", err)
	}
	if p.Balance.Uint64() != 1000 {
		t.Fatalf("historical balance = %s, want the balance at v1", p.Balance)
	}
	if p.RootIndex != 1 {
		t.Fatalf("historical rootIndex = %d, want 1", p.RootIndex)
	}
}

// TestProofAtRejectsUnanchoredVersion pins that a proof is never produced against a
// snapshot the contract does not know about — that proof would fail on-chain and
// waste the user's gas.
func TestProofAtRejectsUnanchoredVersion(t *testing.T) {
	svc, l := anchorFixture(t)
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(7), Asset: 1,
		Amount: uint256.NewInt(10), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProofAt(perpAcct(7), 1, 0); !errors.Is(err, ledger.ErrNoRootAtVersion) {
		t.Fatalf("want ErrNoRootAtVersion, got %v", err)
	}
}

// TestProofAtRejectsAccountWithNoBalance pins the missing-leaf path.
func TestProofAtRejectsAccountWithNoBalance(t *testing.T) {
	svc, l := anchorFixture(t)
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(7), Asset: 1,
		Amount: uint256.NewInt(10), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	v := anchorDirect(svc, l, 1)
	if _, err := svc.ProofAt(perpAcct(99), 1, v); !errors.Is(err, ledger.ErrNoLeaf) {
		t.Fatalf("want ErrNoLeaf, got %v", err)
	}
}

func TestAnchoredVersionsNewestFirst(t *testing.T) {
	svc, l := anchorFixture(t)
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(7), Asset: 1,
		Amount: uint256.NewInt(10), Ref: "d1"}); err != nil {
		t.Fatal(err)
	}
	v1 := anchorDirect(svc, l, 1)
	if err := l.Apply(ledger.DepositOp{Account: perpAcct(7), Asset: 1,
		Amount: uint256.NewInt(10), Ref: "d2"}); err != nil {
		t.Fatal(err)
	}
	v2 := anchorDirect(svc, l, 2)

	got := svc.AnchoredVersions()
	if len(got) != 2 || got[0] != v2 || got[1] != v1 {
		t.Fatalf("want [%d %d] newest first, got %v", v2, v1, got)
	}
}

// verifyProofForTest recomputes the root from the leaf and proof using the same
// rule the contract uses.
func verifyProofForTest(proof [][32]byte, root [32]byte, a ledger.AccountID, asset ledger.AssetID, bal *uint256.Int) bool {
	return ledger.VerifyProofForTest(proof, root, a, asset, bal)
}

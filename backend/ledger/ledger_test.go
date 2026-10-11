package ledger

import (
	"testing"

	"github.com/holiman/uint256"
)

func acct(b byte) AccountID {
	var a AccountID
	for i := range a {
		a[i] = b
	}
	return a
}

func amt(n uint64) *uint256.Int { return uint256.NewInt(n) }

func TestApplyDepositAndWithdraw(t *testing.T) {
	l := New()
	a := acct(1)

	if err := l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
	if err := l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(40), Ref: "w1"}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 60 {
		t.Fatalf("balance = %d, want 60", got)
	}
	if l.Version() != 2 {
		t.Fatalf("version = %d, want 2", l.Version())
	}
}

func TestWithdrawInsufficientIsAtomic(t *testing.T) {
	l := New()
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(10), Ref: "d1"})
	before := l.Version()

	err := l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(11), Ref: "w1"})
	if err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 10 {
		t.Fatalf("balance = %d, want unchanged 10", got)
	}
	if l.Version() != before {
		t.Fatalf("version changed on failed apply")
	}
}

func TestApplyIsAtomicAcrossOps(t *testing.T) {
	l := New()
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(10), Ref: "d1"})

	// second op fails -> first op must roll back
	err := l.Apply(
		WithdrawOp{Account: a, Asset: 1, Amount: amt(5), Ref: "w1"},
		WithdrawOp{Account: b, Asset: 1, Amount: amt(1), Ref: "w2"},
	)
	if err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 10 {
		t.Fatalf("balance = %d, want 10 (rolled back)", got)
	}
}

func TestTransferIsZeroSum(t *testing.T) {
	l := New()
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(30), Ref: "d1"})
	if err := l.Apply(TransferOp{From: a, To: b, Asset: 1, Amount: amt(30), Ref: "t1"}); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if l.Balance(a, 1).Sign() != 0 || l.Balance(b, 1).Uint64() != 30 {
		t.Fatalf("unexpected balances a=%s b=%s", l.Balance(a, 1), l.Balance(b, 1))
	}
	if l.TotalBalance(1).Uint64() != 30 {
		t.Fatalf("total = %s, want 30", l.TotalBalance(1))
	}
}

func TestSelfTransferIsNoop(t *testing.T) {
	l := New()
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(30), Ref: "d1"})
	before := l.Version()

	op := TransferOp{From: a, To: a, Asset: 1, Amount: amt(30), Ref: "t1"}
	if err := l.Apply(op); err != nil {
		t.Fatalf("self-transfer: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 30 {
		t.Fatalf("balance = %d, want unchanged 30", got)
	}
	if got := l.TotalBalance(1).Uint64(); got != 30 {
		t.Fatalf("total = %d, want unchanged 30", got)
	}
	applied := l.Version()
	if applied != before+1 {
		t.Fatalf("version = %d, want %d (op must count as applied)", applied, before+1)
	}

	// Replaying the same key must be a no-op and must not double-advance.
	if err := l.Apply(op); err != nil {
		t.Fatalf("self-transfer replay: %v", err)
	}
	if l.Version() != applied {
		t.Fatalf("version = %d, want %d (no double advance on replay)", l.Version(), applied)
	}
	if got := l.TotalBalance(1).Uint64(); got != 30 {
		t.Fatalf("total = %d, want unchanged 30 after replay", got)
	}
}

func TestIdempotentReplay(t *testing.T) {
	l := New()
	a := acct(1)
	op := DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "txhash:0"}
	if err := l.Apply(op); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := l.Apply(op); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 (no double credit)", got)
	}
	if l.Version() != 1 {
		t.Fatalf("version = %d, want 1", l.Version())
	}
}

func TestEmptyOpKeyRejected(t *testing.T) {
	l := New()
	err := l.Apply(DepositOp{Account: acct(1), Asset: 1, Amount: amt(1)})
	if err != ErrEmptyOpKey {
		t.Fatalf("err = %v, want ErrEmptyOpKey", err)
	}
}

func TestConservation(t *testing.T) {
	l := New()
	a, b := acct(1), acct(2)
	_ = l.Apply(DepositOp{Account: a, Asset: 7, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: b, Asset: 7, Amount: amt(50), Ref: "d2"})
	_ = l.Apply(TransferOp{From: a, To: b, Asset: 7, Amount: amt(20), Ref: "t1"})
	_ = l.Apply(WithdrawOp{Account: b, Asset: 7, Amount: amt(30), Ref: "w1"})

	want := uint256.NewInt(120) // 100 + 50 - 30
	if l.TotalBalance(7).Cmp(want) != 0 {
		t.Fatalf("total = %s, want %s", l.TotalBalance(7), want)
	}
}

// TestRootAndVersion checks the atomic accessor returns the same values as the
// separate Root()/Version() calls.
func TestRootAndVersion(t *testing.T) {
	l := New()

	root, version := l.RootAndVersion()
	if version != 0 {
		t.Fatalf("empty version = %d, want 0", version)
	}
	if root != l.Root() {
		t.Fatalf("empty root = %x, want %x", root, l.Root())
	}

	a := acct(1)
	if err := l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	root, version = l.RootAndVersion()
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	if root != l.Root() {
		t.Fatalf("root = %x, want %x", root, l.Root())
	}
}

// TestWasApplied covers the read-only accessor the settlement service uses to
// detect a replayed withdrawal ref whose in-memory record is gone: false for an
// unseen key, true once applied, and still true after a same-key replay.
func TestWasApplied(t *testing.T) {
	l := New()
	a := acct(1)

	if l.WasApplied("w1") {
		t.Fatalf("WasApplied(unseen) = true, want false")
	}

	op := DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "w1"}
	if err := l.Apply(op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !l.WasApplied("w1") {
		t.Fatalf("WasApplied(applied) = false, want true")
	}

	// A same-key replay is skipped but must leave the key applied.
	if err := l.Apply(op); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !l.WasApplied("w1") {
		t.Fatalf("WasApplied after replay = false, want true")
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 (no double credit)", got)
	}
}

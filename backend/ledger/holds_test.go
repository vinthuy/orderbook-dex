package ledger

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/holiman/uint256"
)

func testHolds(t *testing.T, balance uint64) (*Ledger, *Holds, AccountID) {
	t.Helper()
	l := New()
	a := acct(1)
	if err := l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(balance), Ref: "d1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	return l, NewHolds(l), a
}

func TestHoldsAvailableTracksReserveAndRelease(t *testing.T) {
	_, h, a := testHolds(t, 100)

	if got := h.Held(a, 1).Uint64(); got != 0 {
		t.Fatalf("Held before reserve = %d, want 0", got)
	}
	if got := h.Available(a, 1).Uint64(); got != 100 {
		t.Fatalf("Available before reserve = %d, want 100", got)
	}

	if err := h.Reserve(a, 1, amt(40)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := h.Held(a, 1).Uint64(); got != 40 {
		t.Fatalf("Held after reserve = %d, want 40", got)
	}
	if got := h.Available(a, 1).Uint64(); got != 60 {
		t.Fatalf("Available after reserve = %d, want 60", got)
	}

	if err := h.Release(a, 1, amt(15)); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := h.Held(a, 1).Uint64(); got != 25 {
		t.Fatalf("Held after release = %d, want 25", got)
	}
	if got := h.Available(a, 1).Uint64(); got != 75 {
		t.Fatalf("Available after release = %d, want 75", got)
	}
}

func TestHoldsReserveInsufficientLeavesStateUntouched(t *testing.T) {
	_, h, a := testHolds(t, 100)
	if err := h.Reserve(a, 1, amt(70)); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// 70 held leaves 30 available, so 31 must fail without changing anything.
	if err := h.Reserve(a, 1, amt(31)); err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if got := h.Held(a, 1).Uint64(); got != 70 {
		t.Fatalf("Held after failed reserve = %d, want 70", got)
	}
	if got := h.Available(a, 1).Uint64(); got != 30 {
		t.Fatalf("Available after failed reserve = %d, want 30", got)
	}

	// Reserving exactly the available amount must succeed.
	if err := h.Reserve(a, 1, amt(30)); err != nil {
		t.Fatalf("reserve exact available: %v", err)
	}
	if got := h.Available(a, 1).Uint64(); got != 0 {
		t.Fatalf("Available after exact reserve = %d, want 0", got)
	}
}

func TestHoldsReleaseToZeroDeletesKeyAndUnderflowFails(t *testing.T) {
	_, h, a := testHolds(t, 100)
	if err := h.Reserve(a, 1, amt(50)); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if err := h.Release(a, 1, amt(20)); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := h.Release(a, 1, amt(30)); err != nil {
		t.Fatalf("release to zero: %v", err)
	}
	if _, ok := h.held[HoldKey{Account: a, Asset: 1}]; ok {
		t.Fatal("hold key still present after releasing to zero")
	}

	// Anything more than held (here: zero) must be an explicit error, not a
	// silent clamp.
	if err := h.Release(a, 1, amt(1)); err != ErrHoldUnderflow {
		t.Fatalf("over-release err = %v, want ErrHoldUnderflow", err)
	}

	// Partially release then over-release: the state must not move.
	if err := h.Reserve(a, 1, amt(10)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := h.Release(a, 1, amt(11)); err != ErrHoldUnderflow {
		t.Fatalf("over-release err = %v, want ErrHoldUnderflow", err)
	}
	if got := h.Held(a, 1).Uint64(); got != 10 {
		t.Fatalf("Held after failed over-release = %d, want 10", got)
	}
}

func TestHoldsReconcileReplacesFreezeState(t *testing.T) {
	_, h, a := testHolds(t, 100)
	b := acct(2)

	if err := h.Reserve(a, 1, amt(90)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Reconcile must wipe the old 90 and install exactly the new map. Zero and
	// nil entries are skipped.
	open := map[HoldKey]*uint256.Int{
		{Account: a, Asset: 1}: amt(25),
		{Account: b, Asset: 2}: amt(0),
		{Account: a, Asset: 3}: nil,
	}
	if err := h.Reconcile(open); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := h.Held(a, 1).Uint64(); got != 25 {
		t.Fatalf("Held after reconcile = %d, want 25", got)
	}
	if _, ok := h.held[HoldKey{Account: b, Asset: 2}]; ok {
		t.Fatal("zero-valued reconcile entry must be skipped")
	}
	if _, ok := h.held[HoldKey{Account: a, Asset: 3}]; ok {
		t.Fatal("nil reconcile entry must be skipped")
	}

	// Mutating the caller's map after Reconcile must not affect internal state.
	open[HoldKey{Account: a, Asset: 1}].SetUint64(999)
	if got := h.Held(a, 1).Uint64(); got != 25 {
		t.Fatalf("Held changed through reconciled map alias = %d, want 25", got)
	}
}

func TestHoldsSpendDebitErrorKeepsHold(t *testing.T) {
	_, h, a := testHolds(t, 100)
	if err := h.Reserve(a, 1, amt(40)); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	sentinel := fmt.Errorf("debit failed")
	called := false
	err := h.Spend(a, 1, amt(10), func() error {
		called = true
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if !called {
		t.Fatal("debit callback not invoked")
	}
	if got := h.Held(a, 1).Uint64(); got != 40 {
		t.Fatalf("Held after failed debit = %d, want 40", got)
	}
	if got := h.Available(a, 1).Uint64(); got != 60 {
		t.Fatalf("Available after failed debit = %d, want 60", got)
	}
}

func TestHoldsSpendRejectsInsufficientWithoutCallingDebit(t *testing.T) {
	_, h, a := testHolds(t, 100)
	if err := h.Reserve(a, 1, amt(70)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	called := false
	err := h.Spend(a, 1, amt(31), func() error {
		called = true
		return nil
	})
	if err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if called {
		t.Fatal("debit must not run when available is insufficient")
	}
}

// TestHoldsReturnsCopies is the aliasing guard: mutating a value returned by
// Held/Available must not leak into the internal state. It would catch an
// implementation that returned (or handed out) a live internal pointer.
func TestHoldsReturnsCopies(t *testing.T) {
	_, h, a := testHolds(t, 100)
	if err := h.Reserve(a, 1, amt(50)); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	held := h.Held(a, 1)
	held.SetUint64(12345)
	if got := h.Held(a, 1).Uint64(); got != 50 {
		t.Fatalf("Held mutated through returned pointer: %d, want 50", got)
	}

	avail := h.Available(a, 1)
	avail.SetUint64(0)
	if got := h.Available(a, 1).Uint64(); got != 50 {
		t.Fatalf("Available mutated through returned pointer: %d, want 50", got)
	}
}

// TestHoldsReserveSpendConcurrency asserts the central safety property: the
// availability check and the mutation are atomic together, so a Reserve and a
// Spend can never both consume the same last unit. Sum of successes may not
// exceed the initial balance.
func TestHoldsReserveSpendConcurrency(t *testing.T) {
	l, h, a := testHolds(t, 100)

	const workers = 400
	var wg sync.WaitGroup
	var reserveOK, spendOK atomic.Int64

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				if err := h.Reserve(a, 1, amt(1)); err == nil {
					reserveOK.Add(1)
				}
				return
			}
			err := h.Spend(a, 1, amt(1), func() error {
				return l.Apply(WithdrawOp{Account: a, Asset: 1, Amount: amt(1), Ref: fmt.Sprintf("spend:%d", i)})
			})
			if err == nil {
				spendOK.Add(1)
			}
		}()
	}
	wg.Wait()

	total := reserveOK.Load() + spendOK.Load()
	if total > 100 {
		t.Fatalf("over-committed: reserves %d + spends %d = %d > 100", reserveOK.Load(), spendOK.Load(), total)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100-uint64(spendOK.Load()) {
		t.Fatalf("balance = %d, want %d", got, 100-spendOK.Load())
	}
	if got := h.Held(a, 1).Uint64(); got != uint64(reserveOK.Load()) {
		t.Fatalf("Held = %d, want %d", got, reserveOK.Load())
	}
}

package gateway

import (
	"testing"

	"web3-amm-dex-backend/matching"
)

func TestStateMachineLifecycle(t *testing.T) {
	sm := NewStateMachine()
	sm.Begin(1)
	sm.Accept(1, 42)
	rec, ok := sm.Get(1)
	if !ok {
		t.Fatal("record 1 should exist")
	}
	if rec.State != StateAccepted || rec.OrderID != 42 {
		t.Fatalf("record = %+v, want accepted/42", rec)
	}

	sm.Begin(2)
	sm.Reject(2, errSentinel{})
	rec, _ = sm.Get(2)
	if rec.State != StateRejected || rec.Err == "" {
		t.Fatalf("record = %+v, want rejected with error", rec)
	}
}

func TestStateMachineRequestForOrder(t *testing.T) {
	sm := NewStateMachine()
	sm.Begin(7)
	sm.Accept(7, 99)

	rec, ok := sm.RequestForOrder(99)
	if !ok || rec.RequestID != 7 {
		t.Fatalf("RequestForOrder(99) = %+v (ok=%v), want request 7", rec, ok)
	}
	if _, ok := sm.RequestForOrder(matching.OrderID(1)); ok {
		t.Fatal("unknown order should not be found")
	}
}

func TestStateMachineAcceptZeroOrderIDNotIndexed(t *testing.T) {
	sm := NewStateMachine()
	sm.Begin(1)
	sm.Accept(1, 0)
	if _, ok := sm.RequestForOrder(0); ok {
		t.Fatal("orderID 0 must not be indexed")
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "rejected" }

package gateway

import (
	"errors"
	"testing"

	"web3-amm-dex-backend/ledger"
)

func acct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func TestNonceStoreCheckAndAdvance(t *testing.T) {
	n, err := OpenNonceStore("")
	if err != nil {
		t.Fatalf("OpenNonceStore: %v", err)
	}
	defer n.Close()

	a := acct(1)
	if err := n.CheckAndAdvance(a, 1); err != nil {
		t.Fatalf("first nonce: %v", err)
	}
	if !errors.Is(n.CheckAndAdvance(a, 1), ErrNonceReused) {
		t.Fatalf("same nonce should be rejected")
	}
	if !errors.Is(n.CheckAndAdvance(a, 0), ErrNonceReused) {
		t.Fatalf("lower nonce should be rejected")
	}
	if err := n.CheckAndAdvance(a, 2); err != nil {
		t.Fatalf("higher nonce: %v", err)
	}
	if got := n.Upper(a); got != 2 {
		t.Fatalf("upper = %d, want 2", got)
	}
}

func TestNonceStorePersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	a := acct(7)

	n, err := OpenNonceStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := n.CheckAndAdvance(a, 5); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the upper bound must survive.
	n2, err := OpenNonceStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer n2.Close()
	if !errors.Is(n2.CheckAndAdvance(a, 5), ErrNonceReused) {
		t.Fatalf("nonce 5 should be rejected after reopen")
	}
	if err := n2.CheckAndAdvance(a, 6); err != nil {
		t.Fatalf("nonce 6 should pass: %v", err)
	}
}

func TestNonceStoreConcurrentAdvance(t *testing.T) {
	n, _ := OpenNonceStore("")
	defer n.Close()
	a := acct(9)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { done <- n.CheckAndAdvance(a, 1) }()
	}
	passes := 0
	for i := 0; i < 8; i++ {
		if err := <-done; err == nil {
			passes++
		}
	}
	if passes != 1 {
		t.Fatalf("passes = %d, want exactly 1 under concurrency", passes)
	}
}

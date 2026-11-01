package gateway

import (
	"testing"
	"time"
)

func TestRateLimiterExhaustsBucket(t *testing.T) {
	r := NewRateLimiter(100, 2) // burst 2, refills fast
	a := acct(1)

	if !r.Allow(a) || !r.Allow(a) {
		t.Fatal("first two calls should be allowed (burst 2)")
	}
	if r.Allow(a) {
		t.Fatal("third call should be denied (empty bucket)")
	}
}

func TestRateLimiterRefills(t *testing.T) {
	r := NewRateLimiter(1000, 1)
	a := acct(2)
	if !r.Allow(a) {
		t.Fatal("first call should be allowed")
	}
	if r.Allow(a) {
		t.Fatal("second call should be denied")
	}
	time.Sleep(2 * time.Millisecond)
	if !r.Allow(a) {
		t.Fatal("after refill the call should be allowed")
	}
}

func TestRateLimiterPerAccountIsolation(t *testing.T) {
	r := NewRateLimiter(0.001, 1) // effectively no refill within the test
	if !r.Allow(acct(1)) {
		t.Fatal("account 1 first call should pass")
	}
	if r.Allow(acct(1)) {
		t.Fatal("account 1 second call should be denied")
	}
	if !r.Allow(acct(2)) {
		t.Fatal("account 2 must not be affected by account 1")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	r := NewRateLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if !r.Allow(acct(3)) {
			t.Fatalf("rate <= 0 should never deny, failed at %d", i)
		}
	}
}

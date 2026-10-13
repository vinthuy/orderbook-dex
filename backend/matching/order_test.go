package matching

import (
	"testing"
)

func TestOrderRemaining(t *testing.T) {
	o := &Order{Qty: amt(100), Filled: amt(30)}
	if got := o.Remaining().Uint64(); got != 70 {
		t.Fatalf("Remaining = %d, want 70", got)
	}
}

// TestOrderRemainingReturnsCopy is the aliasing guard: mutating the returned
// amount must not touch the order's own Qty/Filled.
func TestOrderRemainingReturnsCopy(t *testing.T) {
	o := &Order{Qty: amt(100), Filled: amt(30)}
	r := o.Remaining()
	r.SetUint64(0)
	if o.Qty.Uint64() != 100 || o.Filled.Uint64() != 30 {
		t.Fatalf("order mutated through Remaining: qty=%s filled=%s", o.Qty, o.Filled)
	}
	if got := o.Remaining().Uint64(); got != 70 {
		t.Fatalf("Remaining changed to %d, want 70", got)
	}
}

func TestOrderIsOpen(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusNew, true},
		{StatusPartiallyFilled, true},
		{StatusFilled, false},
		{StatusCancelled, false},
		{StatusRejected, false},
	}
	for _, tt := range tests {
		o := &Order{Status: tt.status}
		if got := o.IsOpen(); got != tt.want {
			t.Fatalf("IsOpen(status=%s) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

// TestOrderCloneIsDeepCopy would catch clone sharing pointers with the source.
func TestOrderCloneIsDeepCopy(t *testing.T) {
	o := &Order{
		ID:          7,
		Qty:         amt(100),
		Filled:      amt(40),
		MaxNotional: amt(500),
		reserved:    amt(60),
		Status:      StatusPartiallyFilled,
	}
	c := o.clone()
	if c.Remaining().Uint64() != 60 {
		t.Fatalf("clone Remaining = %s, want 60", c.Remaining())
	}

	c.Qty.SetUint64(1)
	c.Filled.SetUint64(1)
	c.MaxNotional.SetUint64(1)
	c.reserved.SetUint64(1)

	if o.Qty.Uint64() != 100 || o.Filled.Uint64() != 40 || o.MaxNotional.Uint64() != 500 || o.reserved.Uint64() != 60 {
		t.Fatalf("source mutated through clone: %+v", o)
	}
}

func TestMinUint(t *testing.T) {
	if got := minUint(amt(3), amt(9)); got.Uint64() != 3 {
		t.Fatalf("min(3,9) = %s, want 3", got)
	}
	if got := minUint(amt(9), amt(3)); got.Uint64() != 3 {
		t.Fatalf("min(9,3) = %s, want 3", got)
	}
	a := amt(3)
	got := minUint(a, amt(9))
	got.SetUint64(42)
	if a.Uint64() != 3 {
		t.Fatalf("minUint aliased its input: %s", a)
	}
}

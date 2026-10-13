package ledger

import (
	"errors"
	"sync"

	"github.com/holiman/uint256"
)

// HoldKey identifies one frozen bucket: the amount an account has reserved per
// asset. It is the map key the matching kernel uses to hand its freeze state
// back to the ledger layer (see Holds.Reconcile).
type HoldKey struct {
	Account AccountID
	Asset   AssetID
}

// ErrHoldUnderflow signals an attempt to release more than is currently held.
// Holds are only ever created by Reserve/Reconcile and consumed by Release, so
// an underflow is an internal bug (a double release, or releasing a fill that
// was never reserved), never a user-facing condition. It is returned rather
// than clamped to zero so the bug cannot pass silently.
var ErrHoldUnderflow = errors.New("ledger: hold release exceeds held amount")

// Holds tracks engine-side frozen balances on top of a Ledger without touching
// the ledger itself: the Merkle root still publishes the *total* balance, while
// Available reports what a user may withdraw or commit anew.
//
// The mutex is what makes "available balance" meaningful under concurrency: a
// Reserve and a Spend cannot interleave between their check and their mutation,
// so two operations cannot both spend the same last unit. Lock order is
// Holds.mu -> Ledger.mu (Held/Available read the ledger under the hold lock);
// the ledger never calls back into Holds, so this is deadlock-free.
type Holds struct {
	mu   sync.Mutex
	l    *Ledger
	held map[HoldKey]*uint256.Int
}

// NewHolds wraps a ledger. The ledger pointer must remain valid for the
// lifetime of the Holds (it is the source of truth for balances).
func NewHolds(l *Ledger) *Holds {
	return &Holds{
		l:    l,
		held: make(map[HoldKey]*uint256.Int),
	}
}

// Held returns a copy of the amount currently frozen for (account, asset).
// Returning a copy is deliberate: callers must never be able to mutate the
// internal state through the returned pointer.
func (h *Holds) Held(a AccountID, asset AssetID) *uint256.Int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.heldLocked(a, asset)
}

func (h *Holds) heldLocked(a AccountID, asset AssetID) *uint256.Int {
	if v, ok := h.held[HoldKey{Account: a, Asset: asset}]; ok {
		return new(uint256.Int).Set(v)
	}
	return uint256.NewInt(0)
}

// Available returns max(0, balance - held) as a fresh copy. The balance can
// never legitimately drop below the held amount (freezing is gated on
// available), so the clamp only hides a bug rather than producing a negative
// quantity; tests assert the clamped case never arises.
func (h *Holds) Available(a AccountID, asset AssetID) *uint256.Int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.availableLocked(a, asset)
}

func (h *Holds) availableLocked(a AccountID, asset AssetID) *uint256.Int {
	balance := h.l.Balance(a, asset)
	held := h.heldLocked(a, asset)
	if held.Cmp(balance) >= 0 {
		return uint256.NewInt(0)
	}
	return new(uint256.Int).Sub(balance, held)
}

// Reserve freezes amount for (account, asset). It fails with
// ErrInsufficientBalance, leaving Held and Available untouched, when the
// request exceeds the available balance. A zero (or nil) amount is a no-op.
func (h *Holds) Reserve(a AccountID, asset AssetID, amount *uint256.Int) error {
	if amount == nil || amount.IsZero() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	avail := h.availableLocked(a, asset)
	if avail.Cmp(amount) < 0 {
		return ErrInsufficientBalance
	}

	key := HoldKey{Account: a, Asset: asset}
	next := new(uint256.Int).Add(h.heldLocked(a, asset), amount)
	h.held[key] = next
	return nil
}

// Release unfreezes amount for (account, asset). Reaching exactly zero deletes
// the key (so Reconcile/OpenReserves never see zero-valued holds); releasing
// more than is held returns ErrHoldUnderflow instead of clamping.
func (h *Holds) Release(a AccountID, asset AssetID, amount *uint256.Int) error {
	if amount == nil || amount.IsZero() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	key := HoldKey{Account: a, Asset: asset}
	cur, ok := h.held[key]
	if !ok || cur.Cmp(amount) < 0 {
		return ErrHoldUnderflow
	}
	next := new(uint256.Int).Sub(cur, amount)
	if next.IsZero() {
		delete(h.held, key)
		return nil
	}
	h.held[key] = next
	return nil
}

// Reconcile replaces the entire freeze state with open. It is the recovery
// hook: after replaying the instruction log, the kernel's derived reserves are
// authoritative, so the holds are set once rather than replayed incrementally.
// Nil and zero entries are skipped so the map stays canonical.
func (h *Holds) Reconcile(open map[HoldKey]*uint256.Int) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	next := make(map[HoldKey]*uint256.Int, len(open))
	for k, v := range open {
		if v == nil || v.IsZero() {
			continue
		}
		next[k] = new(uint256.Int).Set(v)
	}
	h.held = next
	return nil
}

// Spend runs debit only if the available balance covers amount, holding the
// Holds lock for the whole call so a concurrent Reserve cannot take the funds
// between the check and the debit. It is the withdrawal path: the ledger
// mutation and the availability check are made mutually exclusive.
//
// debit is a closing action: it is always invoked (even for a zero amount) and
// Spend never mutates the hold map, so a debit failure cannot leave a partial
// release behind.
func (h *Holds) Spend(a AccountID, asset AssetID, amount *uint256.Int, debit func() error) error {
	if debit == nil {
		return errors.New("ledger: nil spend callback")
	}
	if amount == nil || amount.IsZero() {
		return debit()
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	avail := h.availableLocked(a, asset)
	if avail.Cmp(amount) < 0 {
		return ErrInsufficientBalance
	}
	return debit()
}

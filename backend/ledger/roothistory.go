package ledger

import (
	"errors"
	"sort"
	"sync"

	"github.com/holiman/uint256"
)

// ErrNoRootAtVersion is returned when a version was never anchored and no earlier
// anchor covers it.
var ErrNoRootAtVersion = errors.New("ledger: no anchored root at or before that version")

// RootRecord is one anchored state: the root, the ledger version it covers, and
// the leaf set that produced it.
//
// Keeping the leaves — not only the root — is what makes a historical proof
// possible. A root alone can verify nothing: withdraw() on-chain needs a proof
// against that specific root, and a proof requires the sibling nodes, which are
// only derivable from the leaves of that exact state.
type RootRecord struct {
	Root    [32]byte
	Version uint64
	// RootIndex is the on-chain index this root was committed as. Zero means the
	// record is a snapshot that has not been anchored, which ProofAt refuses to
	// serve.
	RootIndex uint64
	// Leaves is the ordered leaf set for this state. Kept as hashes rather than
	// entries: a proof only needs sibling hashes.
	Leaves [][32]byte
	// Index maps (account, asset) to its leaf position, so a proof lookup does not
	// have to scan every leaf.
	Index map[leafKey]int
	// balances holds the plaintext balance for each leaf. A hash is one-way, so
	// without this a proof could be built but the on-chain call could not be
	// supplied the `balance` argument it requires.
	balances map[leafKey]*uint256.Int
	// At is the unix time the record was taken.
	At int64
}

// leafKey identifies one leaf without exporting leafEntry.
type leafKey struct {
	Account AccountID
	Asset   AssetID
}

// RootHistory keeps the most recent N anchored roots so proofs remain available
// for roots that are no longer the latest.
//
// Why not keep every root forever: each record holds a full leaf set, so an
// unbounded history grows with (roots × accounts × assets). A bounded ring is the
// honest tradeoff — a user has N anchored states in which to withdraw or exit,
// and the window is configurable.
type RootHistory struct {
	mu      sync.Mutex
	max     int
	records []RootRecord
}

// NewRootHistory builds a history retaining at most maxRoots anchored states.
func NewRootHistory(maxRoots int) *RootHistory {
	if maxRoots <= 0 {
		maxRoots = 64
	}
	return &RootHistory{max: maxRoots}
}

// RecordWithin builds a root record from the CURRENT ledger state and returns it
// together with the root and version it represents, all under one lock.
//
// This exists because anchoring is a two-step operation in time: read the state,
// then commit the root on-chain (seconds to minutes), then store the snapshot. If
// the snapshot is taken by a separate later read, any Apply in between advances the
// version and the snapshot no longer matches what was committed — leaving the
// anchored version permanently unprovable, which is exactly the failure the
// history was added to remove.
//
// The caller commits rec.Root and must store the returned record unchanged.
func (h *RootHistory) RecordWithin(l *Ledger, at int64) (RootRecord, uint64, [32]byte) {
	l.mu.Lock()
	entries := l.leafEntries()
	version := l.version
	l.mu.Unlock()

	leaves := make([][32]byte, len(entries))
	index := make(map[leafKey]int, len(entries))
	balances := make(map[leafKey]*uint256.Int, len(entries))
	for i, e := range entries {
		leaves[i] = leafHash(e.account, e.asset, e.balance)
		k := leafKey{Account: e.account, Asset: e.asset}
		index[k] = i
		balances[k] = new(uint256.Int).Set(e.balance)
	}
	root := merkleRoot(leaves)
	rec := RootRecord{Root: root, Version: version, Leaves: leaves, Index: index, balances: balances, At: at}
	return rec, version, root
}

// Add stores an externally built record, replacing any existing record for the same
// version. It is how AnchorOnce stores the snapshot it committed, so the stored
// record is by construction the one that was anchored.
func (h *RootHistory) Add(rec RootRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.records {
		if h.records[i].Version == rec.Version {
			h.records[i] = rec
			return
		}
	}
	h.records = append(h.records, rec)
	if len(h.records) > h.max {
		h.records = h.records[len(h.records)-h.max:]
	}
}

// Record captures the ledger's current state under its current version. It is
// idempotent for a version already recorded.
func (h *RootHistory) Record(l *Ledger, at int64) RootRecord {
	rec, _, _ := h.RecordWithin(l, at)
	h.Add(rec)
	return rec
}

// Version returns the anchored state for an exact version, if retained.
func (h *RootHistory) Version(v uint64) (RootRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Version == v {
			return r, true
		}
	}
	return RootRecord{}, false
}

// LatestAtOrBefore returns the newest anchored state whose version is <= v. This
// is what a user needs: their balance in the most recent anchored state they can
// prove against.
func (h *RootHistory) LatestAtOrBefore(v uint64) (RootRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	best := -1
	for i, r := range h.records {
		if r.Version <= v && (best < 0 || r.Version > h.records[best].Version) {
			best = i
		}
	}
	if best < 0 {
		return RootRecord{}, false
	}
	return h.records[best], true
}

// All returns the retained records oldest first.
func (h *RootHistory) All() []RootRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RootRecord, len(h.records))
	copy(out, h.records)
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// Proof returns the balance and Merkle proof for one (account, asset) against
// this record's root.
func (r RootRecord) Proof(a AccountID, asset AssetID) (*uint256.Int, [][32]byte, error) {
	idx, ok := r.Index[leafKey{Account: a, Asset: asset}]
	if !ok {
		return nil, nil, ErrNoLeaf
	}
	// The balance is needed so the caller can present it on-chain; recover it from
	// the stored leaf set is impossible (a hash is one-way), so the record keeps
	// the entries alongside. See RecordWithBalances.
	bal, ok := r.balances[leafKey{Account: a, Asset: asset}]
	if !ok {
		return nil, nil, ErrNoLeaf
	}
	return new(uint256.Int).Set(bal), merkleProof(r.Leaves, idx), nil
}

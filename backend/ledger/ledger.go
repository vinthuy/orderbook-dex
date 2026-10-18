package ledger

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/holiman/uint256"
)

// AssetID identifies a registered ERC20 asset on-chain.
type AssetID uint32

// AccountID is a 20-byte account identifier (v1: an EOA address).
type AccountID [20]byte

func (a AccountID) Hex() string { return "0x" + hex.EncodeToString(a[:]) }

func AccountFromHex(s string) (AccountID, error) {
	var a AccountID
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return a, fmt.Errorf("ledger: bad account hex %q: %w", s, err)
	}
	if len(b) != 20 {
		return a, fmt.Errorf("ledger: account must be 20 bytes, got %d", len(b))
	}
	copy(a[:], b)
	return a, nil
}

var (
	ErrInsufficientBalance = errors.New("ledger: insufficient balance")
	ErrEmptyOpKey          = errors.New("ledger: empty op key")
	ErrNoLeaf              = errors.New("ledger: leaf not found")
)

// Ledger is an in-memory balance ledger with an optional write-ahead log.
type Ledger struct {
	mu       sync.Mutex
	balances map[AccountID]map[AssetID]*uint256.Int
	applied  map[string]struct{}
	version  uint64
	store    Store
}

func New() *Ledger {
	return &Ledger{
		balances: make(map[AccountID]map[AssetID]*uint256.Int),
		applied:  make(map[string]struct{}),
	}
}

// WithStore attaches a persistence store; Apply then writes a WAL batch per call.
func (l *Ledger) WithStore(s Store) *Ledger {
	l.store = s
	return l
}

func (l *Ledger) Version() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.version
}

// RootAndVersion returns the Merkle root and the ledger version together, under
// a single lock, so a concurrent Apply cannot make the recorded pair
// inconsistent (which separate Root()/Version() calls allow).
func (l *Ledger) RootAndVersion() ([32]byte, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return merkleRoot(l.leavesLocked()), l.version
}

func (l *Ledger) Balance(a AccountID, asset AssetID) *uint256.Int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balanceLocked(a, asset)
}

func (l *Ledger) balanceLocked(a AccountID, asset AssetID) *uint256.Int {
	m := l.balances[a]
	if m == nil {
		return uint256.NewInt(0)
	}
	b := m[asset]
	if b == nil {
		return uint256.NewInt(0)
	}
	return new(uint256.Int).Set(b)
}

func (l *Ledger) TotalBalance(asset AssetID) *uint256.Int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := uint256.NewInt(0)
	for _, m := range l.balances {
		if b, ok := m[asset]; ok {
			total.Add(total, b)
		}
	}
	return total
}

// Assets returns the sorted set of asset IDs with a non-zero balance (or that
// have ever been referenced). It is read-only and used by the indexer's
// reconciliation to enumerate per-asset totals.
func (l *Ledger) Assets() []AssetID {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[AssetID]bool)
	for _, m := range l.balances {
		for asset, b := range m {
			if b != nil && !b.IsZero() {
				seen[asset] = true
			}
		}
	}
	out := make([]AssetID, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// WasApplied reports whether an op with the given idempotency key has already
// been applied (and therefore would be skipped by Apply).
//
// It is a read-only accessor. The settlement service uses it to detect a
// withdrawal ref that the ledger durably remembers but whose in-memory
// withdrawal record is gone (e.g. after a process restart): in that case there
// is no trustworthy way to reconstruct the original transaction, so the caller
// must be told to reconcile rather than have the ref re-broadcast.
func (l *Ledger) WasApplied(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.applied[key]
	return ok
}

// Apply validates and applies ops atomically. Ops whose key was already applied
// (or repeated within the batch) are skipped. On any error nothing changes.
func (l *Ledger) Apply(ops ...Op) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	fresh, err := l.freshOps(ops)
	if err != nil {
		return err
	}
	if len(fresh) == 0 {
		return nil
	}

	t, err := l.mutateLocked(fresh)
	if err != nil {
		return err
	}

	if l.store != nil {
		entries := make([]WALEntry, 0, len(fresh))
		seq := l.version
		for _, op := range fresh {
			seq++
			entries = append(entries, WALEntry{Seq: seq, Op: op.walRecord()})
		}
		if err := l.store.AppendBatch(entries); err != nil {
			t.rollback()
			return err
		}
		l.version = seq
	} else {
		l.version += uint64(len(fresh))
	}

	for _, op := range fresh {
		l.applied[op.Key()] = struct{}{}
	}
	return nil
}

func (l *Ledger) freshOps(ops []Op) ([]Op, error) {
	fresh := make([]Op, 0, len(ops))
	seen := make(map[string]struct{})
	for _, op := range ops {
		k := op.Key()
		if k == "" {
			return nil, ErrEmptyOpKey
		}
		if _, ok := l.applied[k]; ok {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		fresh = append(fresh, op)
	}
	return fresh, nil
}

func (l *Ledger) mutateLocked(fresh []Op) (*tx, error) {
	t := &tx{l: l}
	for _, op := range fresh {
		if err := op.apply(t); err != nil {
			t.rollback()
			return nil, err
		}
	}
	return t, nil
}

type journalEntry struct {
	a       AccountID
	asset   AssetID
	prev    *uint256.Int
	existed bool
}

// tx is a mutation scope over the ledger with a rollback journal.
type tx struct {
	l       *Ledger
	journal []journalEntry
}

func (t *tx) get(a AccountID, asset AssetID) *uint256.Int {
	m := t.l.balances[a]
	if m == nil {
		return uint256.NewInt(0)
	}
	b := m[asset]
	if b == nil {
		return uint256.NewInt(0)
	}
	return b
}

func (t *tx) record(a AccountID, asset AssetID) {
	e := journalEntry{a: a, asset: asset}
	if m := t.l.balances[a]; m != nil {
		if b, ok := m[asset]; ok {
			e.prev = new(uint256.Int).Set(b)
			e.existed = true
		}
	}
	t.journal = append(t.journal, e)
}

func (t *tx) set(a AccountID, asset AssetID, v *uint256.Int) {
	m := t.l.balances[a]
	if m == nil {
		m = make(map[AssetID]*uint256.Int)
		t.l.balances[a] = m
	}
	m[asset] = new(uint256.Int).Set(v)
}

func (t *tx) rollback() {
	for i := len(t.journal) - 1; i >= 0; i-- {
		e := t.journal[i]
		m := t.l.balances[e.a]
		if m == nil {
			continue
		}
		if e.existed {
			m[e.asset] = e.prev
		} else {
			delete(m, e.asset)
		}
	}
}

// --- snapshot serialization ---

type snapshotBalance struct {
	Account string `json:"account"`
	Asset   uint32 `json:"asset"`
	Amount  string `json:"amount"`
}

type snapshotState struct {
	Version  uint64            `json:"version"`
	Balances []snapshotBalance `json:"balances"`
}

func (l *Ledger) SnapshotBytes() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotBytesLocked()
}

func (l *Ledger) snapshotBytesLocked() ([]byte, error) {
	st := snapshotState{Version: l.version}
	for a, m := range l.balances {
		for asset, b := range m {
			if b.IsZero() {
				continue
			}
			st.Balances = append(st.Balances, snapshotBalance{a.Hex(), uint32(asset), b.Dec()})
		}
	}
	return json.Marshal(st)
}

func (l *Ledger) restoreSnapshot(data []byte) error {
	var st snapshotState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	l.balances = make(map[AccountID]map[AssetID]*uint256.Int)
	for _, sb := range st.Balances {
		a, err := AccountFromHex(sb.Account)
		if err != nil {
			return err
		}
		amount, err := uint256.FromDecimal(sb.Amount)
		if err != nil {
			return err
		}
		m := l.balances[a]
		if m == nil {
			m = make(map[AssetID]*uint256.Int)
			l.balances[a] = m
		}
		m[AssetID(sb.Asset)] = amount
	}
	l.version = st.Version
	return nil
}

// Snap persists a snapshot of the current state (no-op without a store).
func (l *Ledger) Snap() error {
	if l.store == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := l.snapshotBytesLocked()
	if err != nil {
		return err
	}
	return l.store.SaveSnapshot(data)
}

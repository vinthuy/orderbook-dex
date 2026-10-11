package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// WALEntry is one append-only operation record.
type WALEntry struct {
	Seq uint64   `json:"seq"`
	Op  OpRecord `json:"op"`
}

// Store persists the ledger: an append-only WAL and periodic snapshots.
type Store interface {
	AppendBatch(entries []WALEntry) error
	SaveSnapshot(state []byte) error
	LoadSnapshot() ([]byte, error) // nil, nil when no snapshot exists
	Replay(fromSeq uint64) ([]WALEntry, error)
	Close() error
}

// FileStore is a directory-backed Store: <dir>/wal.log and <dir>/snapshot.json.
type FileStore struct {
	dir string
	mu  sync.Mutex
	wal *os.File
}

func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileStore{dir: dir, wal: f}, nil
}

func (s *FileStore) AppendBatch(entries []WALEntry) error {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf bytes.Buffer
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if _, err := s.wal.Write(buf.Bytes()); err != nil {
		return err
	}
	return s.wal.Sync()
}

func (s *FileStore) SaveSnapshot(state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := filepath.Join(s.dir, "snapshot.json.tmp")
	if err := os.WriteFile(tmp, state, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "snapshot.json"))
}

func (s *FileStore) LoadSnapshot() ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "snapshot.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Replay reads the WAL and returns the entries with Seq > fromSeq.
//
// A torn/incomplete *trailing* record is tolerated and ignored (with a log
// line): AppendBatch writes a whole batch — records plus their '\n'
// terminators — in a single Write and only Syncs once, so a crash or short
// write can leave a partial final record. There are two such signals: the
// final non-empty line does not parse as JSON, or the file's last byte is not
// '\n' (a half-written record). Either way the trailing record was never
// committed and dropping it lets Recover rebuild the ledger.
//
// Corruption with more data after it is real data loss and is returned as an
// error — it is never silently swallowed.
func (s *FileStore) Replay(fromSeq uint64) ([]WALEntry, error) {
	path := filepath.Join(s.dir, "wal.log")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	hasTrailingNewline := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})

	// Index of the last non-empty line: corruption there is a torn tail, while
	// corruption anywhere earlier means the log lost committed records.
	lastDataLine := -1
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) > 0 {
			lastDataLine = i
		}
	}

	var out []WALEntry
	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if i == lastDataLine && !hasTrailingNewline {
			// The final record was never terminated, so it was never
			// committed; ignore it and stop.
			log.Printf("ledger: wal %s: ignoring torn trailing record (missing newline)", path)
			break
		}
		var e WALEntry
		if err := json.Unmarshal(line, &e); err != nil {
			if i == lastDataLine {
				// Half-written final record: tolerate it.
				log.Printf("ledger: wal %s: ignoring torn trailing record: %v", path, err)
				break
			}
			return nil, fmt.Errorf("ledger: corrupt wal record at line %d: %w", i+1, err)
		}
		if e.Seq > fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wal.Close()
}

// Recover rebuilds a ledger from a snapshot plus the WAL.
//
// The WAL is replayed from seq 0 (FileStore never truncates it) and the entries
// are split at the snapshot's version. Entries at or below the split are
// already reflected in the snapshot, so only their idempotency keys are
// restored (never their balances); entries above it are applied normally.
// Replaying from 0 rather than from the snapshot version is what rebuilds the
// applied-key set for operations captured by the snapshot — without it, an op
// recorded before a snapshot could be credited twice after a restart.
func Recover(store Store) (*Ledger, error) {
	l := New().WithStore(store)

	state, err := store.LoadSnapshot()
	if err != nil {
		return nil, err
	}
	if state != nil {
		if err := l.restoreSnapshot(state); err != nil {
			return nil, err
		}
	}

	split := l.version

	entries, err := store.Replay(0)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		op, err := e.Op.ToOp()
		if err != nil {
			return nil, err
		}
		if e.Seq <= split {
			// Already applied by the snapshot: restore idempotency only.
			l.applied[op.Key()] = struct{}{}
			l.version = e.Seq
			continue
		}
		if _, ok := l.applied[op.Key()]; ok {
			l.version = e.Seq
			continue
		}
		if _, err := l.mutateLocked([]Op{op}); err != nil {
			return nil, err
		}
		l.applied[op.Key()] = struct{}{}
		l.version = e.Seq
	}
	return l, nil
}

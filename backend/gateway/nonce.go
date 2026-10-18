package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"web3-amm-dex-backend/ledger"
)

const nonceFileName = "nonces.wal"

// ErrNonceReused is returned when a signed request's nonce is not strictly
// greater than the account's last used nonce.
var ErrNonceReused = errors.New("gateway: nonce already used")

// nonceLine is one on-disk record: an account and the highest nonce it has used.
type nonceLine struct {
	Account string `json:"account"`
	Nonce   uint64 `json:"nonce"`
}

// NonceStore persists each account's highest used nonce in an append-only JSONL
// file and keeps the upper bounds in memory. A dir of "" is memory-only.
type NonceStore struct {
	dir   string
	mu    sync.Mutex
	f     *os.File
	upper map[ledger.AccountID]uint64
}

// OpenNonceStore opens (creating if needed) the nonce log and replays it to
// rebuild the upper bounds. An empty dir yields a memory-only store.
func OpenNonceStore(dir string) (*NonceStore, error) {
	n := &NonceStore{dir: dir, upper: make(map[ledger.AccountID]uint64)}
	if dir == "" {
		return n, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, nonceFileName)
	if err := n.replay(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	n.f = f
	return n, nil
}

// replay reads the nonce log, keeping each account's maximum nonce. The final
// record is tolerated as torn only if the file does not end in a newline;
// corruption anywhere else is a hard error (same rule as the order-book WAL).
func (n *NonceStore) replay(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	hasNL := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})

	// Index of the last non-empty line: a torn tail is only ever the final
	// record, never an interior one.
	lastDataLine := -1
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) > 0 {
			lastDataLine = i
		}
	}

	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var rec nonceLine
		if err := json.Unmarshal(line, &rec); err != nil {
			if i == lastDataLine && !hasNL {
				break // torn trailing record: never committed
			}
			return fmt.Errorf("gateway: corrupt nonce record at line %d: %w", i+1, err)
		}
		a, err := ledger.AccountFromHex(rec.Account)
		if err != nil {
			return err
		}
		if rec.Nonce > n.upper[a] {
			n.upper[a] = rec.Nonce
		}
	}
	return nil
}

// CheckAndAdvance accepts only a nonce strictly greater than the account's
// upper bound, then durably records and advances it. The check, the write, and
// the in-memory advance all happen under one lock.
func (n *NonceStore) CheckAndAdvance(a ledger.AccountID, nonce uint64) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nonce <= n.upper[a] {
		return fmt.Errorf("%w: account %s used %d", ErrNonceReused, a.Hex(), n.upper[a])
	}
	if n.f != nil {
		line, err := json.Marshal(nonceLine{Account: a.Hex(), Nonce: nonce})
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if _, err := n.f.Write(line); err != nil {
			return err
		}
		if err := n.f.Sync(); err != nil {
			return err
		}
	}
	n.upper[a] = nonce
	return nil
}

// Upper reports the account's highest used nonce.
func (n *NonceStore) Upper(a ledger.AccountID) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.upper[a]
}

// Close releases the nonce file handle. Idempotent and safe on memory stores.
func (n *NonceStore) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.f == nil {
		return nil
	}
	err := n.f.Close()
	n.f = nil
	return err
}

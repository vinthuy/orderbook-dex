package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
)

// Journal file name inside the order-book data directory (spec §6.4).
const journalFileName = "orders.wal"

// Instruction kinds recorded in the order-book WAL.
const (
	kindPlace  = "place"
	kindCancel = "cancel"
)

// ErrJournalClosed is returned by Append after Close.
var ErrJournalClosed = errors.New("services: journal is closed")

// PlaceCommand is the durable form of a matching.PlaceRequest. Amounts are kept
// as uint256 so a replay reconstructs the exact request (the kernel is only
// deterministic if the inputs are identical).
type PlaceCommand struct {
	Market      matching.MarketID
	Account     ledger.AccountID
	Side        matching.Side
	Type        matching.Type
	TIF         matching.TIF
	Price       matching.Price
	Qty         *uint256.Int
	MaxNotional *uint256.Int
}

// Request converts the command back into a kernel request. Amounts are copied
// so a caller cannot alias the command's fields into kernel state.
func (c PlaceCommand) Request() matching.PlaceRequest {
	return matching.PlaceRequest{
		Market:      c.Market,
		Account:     c.Account,
		Side:        c.Side,
		Type:        c.Type,
		TIF:         c.TIF,
		Price:       c.Price,
		Qty:         cloneUint(c.Qty),
		MaxNotional: cloneUint(c.MaxNotional),
	}
}

// CancelCommand is the durable form of a Cancel request.
type CancelCommand struct {
	Market  matching.MarketID
	Account ledger.AccountID
	OrderID matching.OrderID
}

// Entry is one decoded journal record. Exactly one of Place/Cancel is set,
// selected by Kind.
type Entry struct {
	Seq    uint64
	Kind   string
	Place  *PlaceCommand
	Cancel *CancelCommand
}

// PlaceEntry builds a place Entry for Append.
func PlaceEntry(req matching.PlaceRequest) Entry {
	return Entry{Kind: kindPlace, Place: &PlaceCommand{
		Market:      req.Market,
		Account:     req.Account,
		Side:        req.Side,
		Type:        req.Type,
		TIF:         req.TIF,
		Price:       req.Price,
		Qty:         cloneUint(req.Qty),
		MaxNotional: cloneUint(req.MaxNotional),
	}}
}

// CancelEntry builds a cancel Entry for Append.
func CancelEntry(market matching.MarketID, account ledger.AccountID, orderID matching.OrderID) Entry {
	return Entry{Kind: kindCancel, Cancel: &CancelCommand{Market: market, Account: account, OrderID: orderID}}
}

// journalLine is the on-disk JSON envelope: {seq, kind, payload}.
type journalLine struct {
	Seq     uint64          `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// placeCommandJSON is the wire form of a PlaceCommand. Amounts are decimal
// strings (uint256 does not round-trip through encoding/json as a number).
type placeCommandJSON struct {
	Market      uint32 `json:"market"`
	Account     string `json:"account"`
	Side        uint8  `json:"side"`
	Type        uint8  `json:"type"`
	TIF         uint8  `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"max_notional,omitempty"`
}

// cancelCommandJSON is the wire form of a CancelCommand.
type cancelCommandJSON struct {
	Market  uint32 `json:"market"`
	Account string `json:"account"`
	OrderID uint64 `json:"order_id"`
}

// Journal is the order book's instruction write-ahead log: an append-only file
// of JSON lines, one per Place/Cancel, that is the single source of truth for
// rebuilding the book (and, via the kernel's derived reserves, the freeze
// state) after a crash.
//
// It reuses the ledger WAL's torn-tail rule (see ledger.FileStore.Replay): a
// final record that fails to parse or is not newline-terminated was never
// committed and is ignored with a log line, while corruption with committed
// records after it is returned as an error and never swallowed.
//
// A Journal with dir == "" is an in-memory stub: Append is a no-op and Replay
// returns nothing. That keeps the order book runnable (and its tests fast)
// without persistence, matching spec §7's default of a memory-only order book.
type Journal struct {
	dir string

	mu     sync.Mutex
	f      *os.File
	seq    uint64
	closed bool
}

// NewJournal opens (creating if needed) the order-book WAL in dir. An empty dir
// yields an in-memory journal that persists nothing.
func NewJournal(dir string) (*Journal, error) {
	j := &Journal{dir: dir}
	if dir == "" {
		return j, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, journalFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

// Append durably records one instruction with the next sequence number. The
// record is written and fsync'd in a single call so a crash either leaves it
// complete or leaves a torn tail that Replay ignores.
func (j *Journal) Append(e Entry) error {
	if j.dir == "" {
		return nil // memory mode: nothing to persist, Replay returns nothing
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrJournalClosed
	}
	e.Seq = j.seq + 1
	line, err := encodeEntry(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		return err
	}
	if err := j.f.Sync(); err != nil {
		return err
	}
	j.seq = e.Seq
	return nil
}

// Replay returns every committed entry in file order. Sequence numbers must be
// strictly increasing and every kind must be known; either violation is an
// error. Only a torn trailing record is tolerated.
func (j *Journal) Replay() ([]Entry, error) {
	if j.dir == "" {
		return nil, nil
	}
	path := filepath.Join(j.dir, journalFileName)
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
	// corruption anywhere earlier means committed records were lost.
	lastDataLine := -1
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) > 0 {
			lastDataLine = i
		}
	}

	var (
		out     []Entry
		lastSeq uint64
	)
	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if i == lastDataLine && !hasTrailingNewline {
			// The final record was never terminated, so never committed.
			log.Printf("order book: journal %s: ignoring torn trailing record (missing newline)", path)
			break
		}
		var jl journalLine
		if err := json.Unmarshal(line, &jl); err != nil {
			if i == lastDataLine {
				log.Printf("order book: journal %s: ignoring torn trailing record: %v", path, err)
				break
			}
			return nil, fmt.Errorf("services: corrupt journal record at line %d: %w", i+1, err)
		}
		e, err := decodeEntry(jl)
		if err != nil {
			return nil, fmt.Errorf("services: corrupt journal record at line %d: %w", i+1, err)
		}
		if e.Seq <= lastSeq {
			return nil, fmt.Errorf("services: journal sequence not increasing at line %d: %d <= %d", i+1, e.Seq, lastSeq)
		}
		lastSeq = e.Seq
		out = append(out, e)
	}

	j.mu.Lock()
	if lastSeq > j.seq {
		j.seq = lastSeq
	}
	j.mu.Unlock()
	return out, nil
}

// Close releases the WAL handle. It is idempotent and safe on a memory journal.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil || j.closed {
		return nil
	}
	j.closed = true
	return j.f.Close()
}

// Dir reports the data directory (empty for the in-memory journal).
func (j *Journal) Dir() string { return j.dir }

func encodeEntry(e Entry) ([]byte, error) {
	var payload any
	switch e.Kind {
	case kindPlace:
		if e.Place == nil {
			return nil, fmt.Errorf("services: place entry %d has no payload", e.Seq)
		}
		payload = placeEnvelope(*e.Place)
	case kindCancel:
		if e.Cancel == nil {
			return nil, fmt.Errorf("services: cancel entry %d has no payload", e.Seq)
		}
		payload = cancelEnvelope(*e.Cancel)
	default:
		return nil, fmt.Errorf("services: unknown journal kind %q", e.Kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(journalLine{Seq: e.Seq, Kind: e.Kind, Payload: body})
}

func decodeEntry(jl journalLine) (Entry, error) {
	e := Entry{Seq: jl.Seq, Kind: jl.Kind}
	switch jl.Kind {
	case kindPlace:
		var p placeCommandJSON
		if err := json.Unmarshal(jl.Payload, &p); err != nil {
			return Entry{}, err
		}
		cmd, err := p.command()
		if err != nil {
			return Entry{}, err
		}
		e.Place = cmd
	case kindCancel:
		var c cancelCommandJSON
		if err := json.Unmarshal(jl.Payload, &c); err != nil {
			return Entry{}, err
		}
		cmd, err := c.command()
		if err != nil {
			return Entry{}, err
		}
		e.Cancel = cmd
	default:
		return Entry{}, fmt.Errorf("unknown journal kind %q", jl.Kind)
	}
	return e, nil
}

func placeEnvelope(c PlaceCommand) placeCommandJSON {
	p := placeCommandJSON{
		Market:  uint32(c.Market),
		Account: c.Account.Hex(),
		Side:    uint8(c.Side),
		Type:    uint8(c.Type),
		TIF:     uint8(c.TIF),
		Price:   uint64(c.Price),
	}
	if c.Qty != nil {
		p.Qty = c.Qty.Dec()
	}
	if c.MaxNotional != nil && !c.MaxNotional.IsZero() {
		p.MaxNotional = c.MaxNotional.Dec()
	}
	return p
}

func (p placeCommandJSON) command() (*PlaceCommand, error) {
	account, err := ledger.AccountFromHex(p.Account)
	if err != nil {
		return nil, err
	}
	qty, err := uint256.FromDecimal(p.Qty)
	if err != nil {
		return nil, fmt.Errorf("services: bad qty %q: %w", p.Qty, err)
	}
	cmd := &PlaceCommand{
		Market:  matching.MarketID(p.Market),
		Account: account,
		Side:    matching.Side(p.Side),
		Type:    matching.Type(p.Type),
		TIF:     matching.TIF(p.TIF),
		Price:   matching.Price(p.Price),
		Qty:     qty,
	}
	if p.MaxNotional != "" {
		maxNotional, err := uint256.FromDecimal(p.MaxNotional)
		if err != nil {
			return nil, fmt.Errorf("services: bad max notional %q: %w", p.MaxNotional, err)
		}
		cmd.MaxNotional = maxNotional
	}
	return cmd, nil
}

func cancelEnvelope(c CancelCommand) cancelCommandJSON {
	return cancelCommandJSON{Market: uint32(c.Market), Account: c.Account.Hex(), OrderID: uint64(c.OrderID)}
}

func (c cancelCommandJSON) command() (*CancelCommand, error) {
	account, err := ledger.AccountFromHex(c.Account)
	if err != nil {
		return nil, err
	}
	return &CancelCommand{Market: matching.MarketID(c.Market), Account: account, OrderID: matching.OrderID(c.OrderID)}, nil
}

// cloneUint copies a uint256, preserving nil.
func cloneUint(v *uint256.Int) *uint256.Int {
	if v == nil {
		return nil
	}
	return new(uint256.Int).Set(v)
}

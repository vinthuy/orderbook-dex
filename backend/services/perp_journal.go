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
	"web3-amm-dex-backend/perp"
)

// PerpJournal is the perpetual order book's instruction WAL. It mirrors the
// spot Journal's torn-tail rule: a final non-newline-terminated or unparseable
// record is a torn tail and is ignored; interior corruption is a hard error.
// dir == "" is memory-only.
type PerpJournal struct {
	dir string
	mu  sync.Mutex
	f   *os.File
	seq uint64
}

func NewPerpJournal(dir string) (*PerpJournal, error) {
	j := &PerpJournal{dir: dir}
	if dir == "" {
		return j, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "perp_orders.wal"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

func (j *PerpJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

type perpPlaceJSON struct {
	Market      uint32 `json:"market"`
	Account     string `json:"account"`
	Side        uint8  `json:"side"`
	Type        uint8  `json:"type"`
	TIF         uint8  `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"max_notional,omitempty"`
	Leverage    uint32 `json:"leverage"`
}

type perpCancelJSON struct {
	Market  uint32 `json:"market"`
	Account string `json:"account"`
	OrderID uint64 `json:"order_id"`
}

type perpLine struct {
	Seq     uint64          `json:"seq"`
	Kind    string          `json:"kind"` // "place" | "cancel"
	Payload json.RawMessage `json:"payload"`
}

// Append records one place instruction durably.
func (j *PerpJournal) AppendPlace(req perp.PlaceRequest) error {
	payload := perpPlaceJSON{
		Market:   uint32(req.Market),
		Account:  req.Account.Hex(),
		Side:     uint8(req.Side),
		Type:     uint8(req.Type),
		TIF:      uint8(req.TIF),
		Price:    uint64(req.Price),
		Leverage: req.Leverage,
	}
	if req.Qty != nil {
		payload.Qty = req.Qty.Dec()
	}
	if req.MaxNotional != nil && !req.MaxNotional.IsZero() {
		payload.MaxNotional = req.MaxNotional.Dec()
	}
	return j.append("place", payload)
}

// AppendCancel records one cancel instruction durably.
func (j *PerpJournal) AppendCancel(m perp.MarketID, a ledger.AccountID, id perp.OrderID) error {
	return j.append("cancel", perpCancelJSON{Market: uint32(m), Account: a.Hex(), OrderID: uint64(id)})
}

func (j *PerpJournal) append(kind string, payload any) error {
	if j.dir == "" {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return errors.New("services: perp journal closed")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	j.seq++
	line, err := json.Marshal(perpLine{Seq: j.seq, Kind: kind, Payload: body})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		return err
	}
	return j.f.Sync()
}

// PerpInstruction is one decoded instruction for replay.
type PerpInstruction struct {
	Kind   string // "place" | "cancel"
	Place  *perp.PlaceRequest
	Cancel *perpCancelInstruction
}

type perpCancelInstruction struct {
	Market  perp.MarketID
	Account ledger.AccountID
	OrderID perp.OrderID
}

// Replay returns every committed instruction in file order. Only a torn
// trailing record is tolerated.
func (j *PerpJournal) Replay() ([]PerpInstruction, error) {
	if j.dir == "" {
		return nil, nil
	}
	path := filepath.Join(j.dir, "perp_orders.wal")
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
	hasNL := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})
	lastData := -1
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) > 0 {
			lastData = i
		}
	}
	var out []PerpInstruction
	for i, raw := range lines {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var ln perpLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			if i == lastData && !hasNL {
				log.Printf("perp journal: ignoring torn trailing record: %v", err)
				break
			}
			return nil, fmt.Errorf("services: corrupt perp journal record at line %d: %w", i+1, err)
		}
		switch ln.Kind {
		case "place":
			var p perpPlaceJSON
			if err := json.Unmarshal(ln.Payload, &p); err != nil {
				return nil, err
			}
			account, err := ledger.AccountFromHex(p.Account)
			if err != nil {
				return nil, err
			}
			qty, err := uint256.FromDecimal(p.Qty)
			if err != nil {
				return nil, fmt.Errorf("services: bad perp qty %q: %w", p.Qty, err)
			}
			req := perp.PlaceRequest{
				Market: perp.MarketID(p.Market), Account: account,
				Side: perp.Side(p.Side), Type: perp.Type(p.Type), TIF: perp.TIF(p.TIF),
				Price: perp.Price(p.Price), Qty: qty, Leverage: p.Leverage,
			}
			if p.MaxNotional != "" {
				mn, err := uint256.FromDecimal(p.MaxNotional)
				if err != nil {
					return nil, err
				}
				req.MaxNotional = mn
			}
			out = append(out, PerpInstruction{Kind: "place", Place: &req})
		case "cancel":
			var c perpCancelJSON
			if err := json.Unmarshal(ln.Payload, &c); err != nil {
				return nil, err
			}
			account, err := ledger.AccountFromHex(c.Account)
			if err != nil {
				return nil, err
			}
			out = append(out, PerpInstruction{Kind: "cancel", Cancel: &perpCancelInstruction{
				Market: perp.MarketID(c.Market), Account: account, OrderID: perp.OrderID(c.OrderID),
			}})
		default:
			return nil, fmt.Errorf("services: unknown perp journal kind %q", ln.Kind)
		}
	}
	return out, nil
}

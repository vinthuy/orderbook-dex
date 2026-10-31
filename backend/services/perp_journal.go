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
	path := filepath.Join(dir, "perp_orders.wal")
	// Drop a torn trailing record before opening for append: O_APPEND would
	// otherwise write the next record onto the partial bytes, turning a
	// tolerated one-time tear into permanent interior corruption.
	if err := truncateTornTail(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

// truncateTornTail cuts the file back to the end of its last newline-terminated
// record when the trailing bytes are an incomplete record. A file whose last
// byte is a newline is already clean. Interior corruption is left alone so
// Replay reports it loudly instead of silently discarding data.
func truncateTornTail(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(data) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if data[len(data)-1] == '\n' {
		return nil
	}
	idx := bytes.LastIndexByte(data, '\n')
	keep := 0
	if idx >= 0 {
		keep = idx + 1
	}
	log.Printf("perp journal: truncating %d torn trailing byte(s)", len(data)-keep)
	return os.Truncate(path, int64(keep))
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
	Kind    string // "place" | "cancel" | "mark" | "funding"
	Place   *perp.PlaceRequest
	Cancel  *perpCancelInstruction
	Mark    *MarkCmd
	Funding *FundingCmd
}

// MarkCmd records one observed price observation. It is journaled so a replay
// rebuilds the exact mark-price history instead of re-reading an oracle whose
// price may have moved (spec §3.5).
type MarkCmd struct {
	Market perp.MarketID
	Index  perp.Price
	Mark   perp.Price
	At     int64
}

// FundingLeg is one netted funding payment: Payer is debited and Receiver
// credited, both in Amount of the market's quote asset.
type FundingLeg struct {
	Payer    ledger.AccountID
	Receiver ledger.AccountID
	Amount   *uint256.Int
}

// FundingCmd records one funding round. Index identifies the round and is part
// of every settlement's idempotency key, so replaying the WAL cannot pay twice.
type FundingCmd struct {
	Market perp.MarketID
	Index  uint64
	Rate   string // signed, 1e18-scaled decimal
	Mark   perp.Price
	At     int64
	Legs   []FundingLeg
}

type perpMarkJSON struct {
	Market uint32 `json:"market"`
	Index  uint64 `json:"index_price"`
	Mark   uint64 `json:"mark_price"`
	At     int64  `json:"at"`
}

type perpFundingLegJSON struct {
	Payer    string `json:"payer"`
	Receiver string `json:"receiver"`
	Amount   string `json:"amount"`
}

type perpFundingJSON struct {
	Market uint32               `json:"market"`
	Index  uint64               `json:"funding_index"`
	Rate   string               `json:"rate"`
	Mark   uint64               `json:"mark_price"`
	At     int64                `json:"at"`
	Legs   []perpFundingLegJSON `json:"legs"`
}

// AppendMark records one price observation durably.
func (j *PerpJournal) AppendMark(c MarkCmd) error {
	return j.append("mark", perpMarkJSON{
		Market: uint32(c.Market), Index: uint64(c.Index), Mark: uint64(c.Mark), At: c.At,
	})
}

// AppendFunding records one funding round durably, before any ledger mutation.
func (j *PerpJournal) AppendFunding(c FundingCmd) error {
	payload := perpFundingJSON{
		Market: uint32(c.Market), Index: c.Index, Rate: c.Rate, Mark: uint64(c.Mark), At: c.At,
	}
	for _, l := range c.Legs {
		payload.Legs = append(payload.Legs, perpFundingLegJSON{
			Payer: l.Payer.Hex(), Receiver: l.Receiver.Hex(), Amount: l.Amount.Dec(),
		})
	}
	return j.append("funding", payload)
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
		case "mark":
			var mk perpMarkJSON
			if err := json.Unmarshal(ln.Payload, &mk); err != nil {
				return nil, err
			}
			out = append(out, PerpInstruction{Kind: "mark", Mark: &MarkCmd{
				Market: perp.MarketID(mk.Market), Index: perp.Price(mk.Index),
				Mark: perp.Price(mk.Mark), At: mk.At,
			}})
		case "funding":
			var fd perpFundingJSON
			if err := json.Unmarshal(ln.Payload, &fd); err != nil {
				return nil, err
			}
			cmd := FundingCmd{
				Market: perp.MarketID(fd.Market), Index: fd.Index, Rate: fd.Rate,
				Mark: perp.Price(fd.Mark), At: fd.At,
			}
			for _, l := range fd.Legs {
				payer, err := ledger.AccountFromHex(l.Payer)
				if err != nil {
					return nil, err
				}
				receiver, err := ledger.AccountFromHex(l.Receiver)
				if err != nil {
					return nil, err
				}
				amount, err := uint256.FromDecimal(l.Amount)
				if err != nil {
					return nil, fmt.Errorf("services: bad funding amount %q: %w", l.Amount, err)
				}
				cmd.Legs = append(cmd.Legs, FundingLeg{Payer: payer, Receiver: receiver, Amount: amount})
			}
			out = append(out, PerpInstruction{Kind: "funding", Funding: &cmd})
		default:
			return nil, fmt.Errorf("services: unknown perp journal kind %q", ln.Kind)
		}
	}
	return out, nil
}

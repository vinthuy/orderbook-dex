package services

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"

	bolt "go.etcd.io/bbolt"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// History persistence buckets. One bucket per record kind, keyed so writes are
// idempotent and a replay cannot duplicate a row.
var (
	histBucketTrades       = []byte("perp_trade_history")
	histBucketFunding      = []byte("perp_funding_history")
	histBucketLiquidations = []byte("perp_liquidation_history")
	histBucketADL          = []byte("perp_adl_history")
)

// historyStore is the durable backing for PerpHistory.
//
// Why persist at all: the in-memory store lost every row on restart, so a user's
// trade and funding history vanished with a deploy. The instruction WAL records
// what happened but not who it happened to in a form a client can page through, so
// this is the queryable projection of the same facts.
//
// It is a projection, not a source of truth: money is reconciled against the
// ledger and the chain. A history write failing is therefore logged and skipped,
// never allowed to block a trade.
type historyStore struct {
	db *bolt.DB
}

// OpenHistoryStore opens (creating if needed) the history database at path. An
// empty path yields a nil store, which means memory-only.
func OpenHistoryStore(path string) (*historyStore, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return nil, fmt.Errorf("services: perp history: create dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("services: perp history: open: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{histBucketTrades, histBucketFunding, histBucketLiquidations, histBucketADL} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("services: perp history: init: %w", err)
	}
	return &historyStore{db: db}, nil
}

// Close releases the database handle.
func (s *historyStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// histTradeRow is the persisted form of a trade history entry. Amounts are decimal
// strings so nothing is lost to JSON float conversion.
type histTradeRow struct {
	TradeID     uint64 `json:"tradeId"`
	Account     string `json:"account"`
	Market      uint32 `json:"market"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	Notional    string `json:"notional"`
	Side        uint8  `json:"side"`
	Role        uint8  `json:"role"`
	Fee         string `json:"fee,omitempty"`
	RealizedPnL string `json:"realizedPnl,omitempty"`
	At          int64  `json:"at"`
}

type histFundingRow struct {
	Account string `json:"account"`
	Market  uint32 `json:"market"`
	Index   uint64 `json:"index"`
	Rate    string `json:"rate"`
	Mark    uint64 `json:"mark"`
	Amount  string `json:"amount"`
	Sign    int    `json:"sign"`
	At      int64  `json:"at"`
	// Seq makes the key unique per (account, round, direction): one account can
	// appear as both payer and receiver only in impossible states, but two rounds
	// with the same index and a re-emitted event must not collide.
	Seq int `json:"seq"`
}

type histLiquidationRow struct {
	Account string `json:"account"`
	Market  uint32 `json:"market"`
	Size    string `json:"size"`
	Mark    uint64 `json:"mark"`
	Equity  string `json:"equity,omitempty"`
	Fee     string `json:"fee,omitempty"`
	Partial bool   `json:"partial"`
	At      int64  `json:"at"`
}

type histADLRow struct {
	Account string `json:"account"`
	Market  uint32 `json:"market"`
	Qty     string `json:"qty"`
	Mark    uint64 `json:"mark"`
	At      int64  `json:"at"`
}

func hexAccount(a ledger.AccountID) string { return a.Hex() }

// SaveTrade persists one account's row for a trade.
func (s *historyStore) SaveTrade(e TradeHistoryEntry, a ledger.AccountID) error {
	if s == nil {
		return nil
	}
	row := histTradeRow{
		TradeID: uint64(e.TradeID), Account: hexAccount(a), Market: uint32(e.Market),
		Price: uint64(e.Price), Side: uint8(e.Side), Role: uint8(e.Role), At: e.At,
	}
	if e.Qty != nil {
		row.Qty = e.Qty.Dec()
	}
	if e.Notional != nil {
		row.Notional = e.Notional.Dec()
	}
	if e.Fee != nil {
		row.Fee = e.Fee.Dec()
	}
	if e.RealizedPnL != nil {
		row.RealizedPnL = e.RealizedPnL.String()
	}
	key := tradeHistKey(a, e.TradeID)
	return s.put(histBucketTrades, key, row)
}

// SaveFunding persists one account's funding row.
func (s *historyStore) SaveFunding(e FundingHistoryEntry, a ledger.AccountID, seq int) error {
	if s == nil {
		return nil
	}
	row := histFundingRow{
		Account: hexAccount(a), Market: uint32(e.Market), Index: e.Index, Rate: e.Rate,
		Mark: uint64(e.Mark), Sign: e.Sign, At: e.At, Seq: seq,
	}
	if e.Amount != nil {
		row.Amount = e.Amount.Dec()
	}
	return s.put(histBucketFunding, fundingHistKey(a, e.Index, e.Sign, seq), row)
}

// SaveLiquidation persists one liquidation row.
func (s *historyStore) SaveLiquidation(e LiquidationHistoryEntry, a ledger.AccountID) error {
	if s == nil {
		return nil
	}
	row := histLiquidationRow{
		Account: hexAccount(a), Market: uint32(e.Market), Mark: uint64(e.Mark),
		Partial: e.Partial, At: e.At,
	}
	if e.Size != nil {
		row.Size = e.Size.Dec()
	}
	if e.Equity != nil {
		row.Equity = e.Equity.String()
	}
	if e.Fee != nil {
		row.Fee = e.Fee.Dec()
	}
	return s.put(histBucketLiquidations, liquidationHistKey(a, e.At, e.Market), row)
}

// SaveADL persists one ADL row.
func (s *historyStore) SaveADL(e ADLHistoryEntry, a ledger.AccountID) error {
	if s == nil {
		return nil
	}
	row := histADLRow{Account: hexAccount(a), Market: uint32(e.Market), Mark: uint64(e.Mark), At: e.At}
	if e.Qty != nil {
		row.Qty = e.Qty.Dec()
	}
	return s.put(histBucketADL, adlHistKey(a, e.At, e.Market), row)
}

func (s *historyStore) put(bucket, key []byte, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("services: perp history: missing bucket %s", bucket)
		}
		return b.Put(key, body)
	})
}

// loadAll reads every persisted row, for rehydrating the in-memory store at
// startup. Trade rows carry their account, so they are returned keyed by account
// rather than as a flat slice.
func (s *historyStore) loadAll() (map[ledger.AccountID][]TradeHistoryEntry,
	map[ledger.AccountID][]FundingHistoryEntry,
	map[ledger.AccountID][]LiquidationHistoryEntry,
	map[ledger.AccountID][]ADLHistoryEntry, map[ledger.AccountID]map[perp.TradeID]struct{}, error) {
	trades := make(map[ledger.AccountID][]TradeHistoryEntry)
	funding := make(map[ledger.AccountID][]FundingHistoryEntry)
	liqs := make(map[ledger.AccountID][]LiquidationHistoryEntry)
	adls := make(map[ledger.AccountID][]ADLHistoryEntry)
	seen := make(map[ledger.AccountID]map[perp.TradeID]struct{})
	if s == nil {
		return trades, funding, liqs, adls, seen, nil
	}

	err := s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(histBucketTrades); b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				var row histTradeRow
				if err := json.Unmarshal(v, &row); err != nil {
					return err
				}
				a, err := ledger.AccountFromHex(row.Account)
				if err != nil {
					return err
				}
				e, err := row.toEntry()
				if err != nil {
					return err
				}
				trades[a] = append(trades[a], e)
				if seen[a] == nil {
					seen[a] = make(map[perp.TradeID]struct{})
				}
				seen[a][e.TradeID] = struct{}{}
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket(histBucketFunding); b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				var row histFundingRow
				if err := json.Unmarshal(v, &row); err != nil {
					return err
				}
				a, err := ledger.AccountFromHex(row.Account)
				if err != nil {
					return err
				}
				amt, err := parseAmt(row.Amount)
				if err != nil {
					return err
				}
				funding[a] = append(funding[a], FundingHistoryEntry{
					Market: perp.MarketID(row.Market), Index: row.Index, Rate: row.Rate,
					Mark: perp.Price(row.Mark), Amount: amt, Sign: row.Sign, At: row.At,
				})
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket(histBucketLiquidations); b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				var row histLiquidationRow
				if err := json.Unmarshal(v, &row); err != nil {
					return err
				}
				a, err := ledger.AccountFromHex(row.Account)
				if err != nil {
					return err
				}
				size, err := parseAmt(row.Size)
				if err != nil {
					return err
				}
				fee, err := parseAmt(row.Fee)
				if err != nil {
					return err
				}
				liqs[a] = append(liqs[a], LiquidationHistoryEntry{
					Market: perp.MarketID(row.Market), Size: size, Mark: perp.Price(row.Mark),
					Equity: parseSigned(row.Equity), Fee: fee, Partial: row.Partial, At: row.At,
				})
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket(histBucketADL); b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				var row histADLRow
				if err := json.Unmarshal(v, &row); err != nil {
					return err
				}
				a, err := ledger.AccountFromHex(row.Account)
				if err != nil {
					return err
				}
				qty, err := parseAmt(row.Qty)
				if err != nil {
					return err
				}
				adls[a] = append(adls[a], ADLHistoryEntry{
					Market: perp.MarketID(row.Market), Qty: qty, Mark: perp.Price(row.Mark), At: row.At,
				})
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	// Bolt iterates keys in byte order, which for our fixed-width keys is
	// chronological; sort defensively so the in-memory order matches a live run.
	for a := range trades {
		sort.Slice(trades[a], func(i, j int) bool { return trades[a][i].TradeID < trades[a][j].TradeID })
	}
	for a := range funding {
		sort.SliceStable(funding[a], func(i, j int) bool { return funding[a][i].At < funding[a][j].At })
	}
	for a := range liqs {
		sort.SliceStable(liqs[a], func(i, j int) bool { return liqs[a][i].At < liqs[a][j].At })
	}
	for a := range adls {
		sort.SliceStable(adls[a], func(i, j int) bool { return adls[a][i].At < adls[a][j].At })
	}
	return trades, funding, liqs, adls, seen, nil
}

// parseSigned reads a possibly-negative decimal, returning nil for an empty string
// or garbage rather than failing the whole load: history is a projection, and one
// malformed row must not stop the service from starting.
func parseSigned(s string) *big.Int {
	if s == "" {
		return nil
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil
	}
	return n
}

// parseAmt reads a decimal amount, treating empty or invalid input as zero.
func parseAmt(s string) (*uint256.Int, error) {
	if s == "" {
		return uint256.NewInt(0), nil
	}
	v, err := uint256.FromDecimal(s)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// toEntry converts a persisted trade row back into an in-memory entry.
func (r histTradeRow) toEntry() (TradeHistoryEntry, error) {
	qty, err := parseAmt(r.Qty)
	if err != nil {
		return TradeHistoryEntry{}, err
	}
	notional, err := parseAmt(r.Notional)
	if err != nil {
		return TradeHistoryEntry{}, err
	}
	e := TradeHistoryEntry{
		TradeID: perp.TradeID(r.TradeID), Market: perp.MarketID(r.Market),
		Price: perp.Price(r.Price), Qty: qty, Notional: notional,
		Side: perp.Side(r.Side), Role: perp.FeeRole(r.Role), At: r.At,
	}
	if r.Fee != "" {
		if f, err := parseAmt(r.Fee); err == nil {
			e.Fee = f
		}
	}
	e.RealizedPnL = parseSigned(r.RealizedPnL)
	return e, nil
}

// --- keys ------------------------------------------------------------------
// Every key is fixed-width so Bolt's byte ordering matches chronological order,
// which keeps a range scan of one account contiguous.

func tradeHistKey(a ledger.AccountID, id perp.TradeID) []byte {
	k := make([]byte, 0, 20+8)
	k = append(k, a[:]...)
	return append(k, bigEndian8(uint64(id))...)
}

func fundingHistKey(a ledger.AccountID, index uint64, sign, seq int) []byte {
	k := make([]byte, 0, 20+8+1+4)
	k = append(k, a[:]...)
	k = append(k, bigEndian8(index)...)
	s := byte(0)
	if sign > 0 {
		s = 1
	}
	k = append(k, s)
	return append(k, bigEndian4(uint32(seq))...)
}

func liquidationHistKey(a ledger.AccountID, at int64, m perp.MarketID) []byte {
	k := make([]byte, 0, 20+8+4)
	k = append(k, a[:]...)
	k = append(k, bigEndian8(uint64(at))...)
	return append(k, bigEndian4(uint32(m))...)
}

func adlHistKey(a ledger.AccountID, at int64, m perp.MarketID) []byte {
	return liquidationHistKey(a, at, m)
}

func bigEndian8(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func bigEndian4(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}


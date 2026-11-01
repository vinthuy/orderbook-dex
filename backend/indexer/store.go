package indexer

import (
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketTrades = []byte("trades")
	bucketFunds  = []byte("fund_events")
	bucketRoots  = []byte("root_events")
	bucketCursor = []byte("cursor")
	cursorKey    = []byte("chain_events")
)

// Store persists indexer records in a bbolt file. Keys are deterministic so
// writes are idempotent: trades key on trade:<id>, fund events on
// txHash:logIndex, roots on root:<index>.
type Store struct {
	db *bolt.DB
}

// OpenStore opens (creating if needed) the bbolt database at path.
func OpenStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("indexer: open store: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketTrades, bucketFunds, bucketRoots, bucketCursor} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("indexer: init store: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// PutTrade upserts a trade record keyed by trade:<id>.
func (s *Store) PutTrade(t TradeRecord) error {
	return s.putJSON(bucketTrades, tradeKey(t.TradeID), t)
}

// PutFundEvent upserts a funding event keyed by txHash:logIndex.
func (s *Store) PutFundEvent(e FundEvent) error {
	return s.putJSON(bucketFunds, []byte(e.TxHash+":"+fmt.Sprint(e.LogIndex)), e)
}

// PutRootEvent upserts a root event keyed by root:<index>.
func (s *Store) PutRootEvent(e RootEvent) error {
	return s.putJSON(bucketRoots, rootKey(e.RootIndex), e)
}

func (s *Store) putJSON(bucket, key []byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("indexer: marshal: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucket).Put(key, data); err != nil {
			return fmt.Errorf("indexer: put %s: %w", key, err)
		}
		return nil
	})
}

// LastProcessedBlock returns the last chain-event block height processed.
func (s *Store) LastProcessedBlock() (uint64, error) {
	var h uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketCursor).Get(cursorKey)
		if v == nil {
			return nil
		}
		h = binaryBigEndianUint64(v)
		return nil
	})
	return h, err
}

// SetLastProcessedBlock records the last chain-event block height processed.
func (s *Store) SetLastProcessedBlock(h uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCursor).Put(cursorKey, uint64ToBytes(h))
	})
}

// QueryTrades returns trades filtered by market and account, up to limit,
// newest first. market==0 means all; account=="" means all.
func (s *Store) QueryTrades(market uint32, account string, limit int) ([]TradeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []TradeRecord{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketTrades).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var t TradeRecord
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if market != 0 && t.Market != market {
				continue
			}
			if account != "" && t.Maker != account && t.Taker != account {
				continue
			}
			out = append(out, t)
			if len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// QueryFundEvents returns funding events filtered by account and asset, newest
// first, up to limit. account=="" means all; asset==0 means all.
func (s *Store) QueryFundEvents(account string, asset uint32, limit int) ([]FundEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	all := []FundEvent{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketFunds).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var e FundEvent
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if account != "" && e.Account != account {
				continue
			}
			if asset != 0 && e.Asset != asset {
				continue
			}
			all = append(all, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Block != all[j].Block {
			return all[i].Block > all[j].Block // newest first
		}
		return all[i].LogIndex > all[j].LogIndex
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// QueryRoots returns root events in ascending root-index order, up to limit.
func (s *Store) QueryRoots(from, limit int) ([]RootEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []RootEvent{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketRoots).Cursor()
		start := rootKey(uint64(from))
		for k, v := c.Seek(start); k != nil; k, v = c.Next() {
			var r RootEvent
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

func uint64ToBytes(v uint64) []byte {
	b := make([]byte, 8)
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
	return b
}

func binaryBigEndianUint64(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}

func tradeKey(id uint64) []byte { return []byte(fmt.Sprintf("trade:%020d", id)) }
func rootKey(idx uint64) []byte { return []byte(fmt.Sprintf("root:%020d", idx)) }

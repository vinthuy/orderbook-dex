# 子项目 5（索引器 + 状态重建）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为订单簿 DEX 增加一个索引器，把成交/资金/根三类历史持久化到 bbolt，提供只读查询，并提供从链上事件独立重建账本再与主账本对账的审计能力。

**Architecture:** 新包 `backend/indexer`：`Store`（bbolt 封装）→ `IndexerService`（订阅 `EventBus` 成交 + 轮询链上事件）→ `Rebuild`/`CompareTotals`（独立重建 + 对账）→ `QueryServer`（HTTP 只读路由）。小改 `services/order_book.go` 的 `recover` 补发成交事件以支持重启回填。

**Tech Stack:** Go 1.25（go-ethereum、gin、holiman/uint256、**go.etcd.io/bbolt 新增**）。

**设计文档：** `docs/superpowers/specs/2026-10-18-orderbook-dex-indexer-design.md`

---

## File Structure

- 新增 `backend/indexer/types.go` — `TradeRecord`/`FundEvent`/`RootEvent`/`AssetDelta` 记录类型
- 新增 `backend/indexer/store.go` — bbolt 封装 + 游标 + 三类查询
- 新增 `backend/indexer/indexer.go` — `IndexerService`（订阅 + 轮询 + 生命周期）
- 新增 `backend/indexer/rebuild.go` — `Rebuild` + `CompareTotals`
- 新增 `backend/indexer/query.go` — `QueryServer` + HTTP 只读路由
- 各 `*_test.go`
- 修改 `backend/services/order_book.go` — `recover` 补发 `orderbook.trade` 事件
- 修改 `backend/config/config.go` — `IndexerConfig` + `LoadIndexerConfig`
- 修改 `backend/main.go` — 装配索引器 + 挂查询路由
- 修改 `backend/go.mod` / `go.sum` — 新增 bbolt
- 修改 `README.md` — 索引器接口、残余假设

关键类型/方法（已存在于代码库，本计划依赖）：

- `services.EventBus`：`Subscribe(topic, func(Event) error)`；`Event{Topic, Data, Timestamp}`；`services.TopicOrderBookTrade`；`services.TradeEvent{Trade matching.Trade}`；`services.NewEventBus()`。
- `services.BlockchainAdapter`：`GetLogs(from, to uint64, address string, topics [][]string) ([]Log, error)`、`GetBlockNumber() (uint64, error)`；`Log{Address, Topics []string, Data []byte, BlockNumber uint64, TxHash string, LogIndex uint}`；`services.NewMockBlockchainAdapter()`（`Logs []Log`、`BlockNumber uint64` 字段可直接赋值）。
- `matching.Trade`：`ID TradeID; Market MarketID; Price Price; Qty *uint256.Int; Notional *uint256.Int; MakerOrder OrderID; TakerOrder OrderID; Maker ledger.AccountID; Taker ledger.AccountID; MakerSide Side`。
- `matching.Side.String()` 返回 "buy"/"sell"。
- `ledger.Ledger`：`New()`、`Apply(ops ...Op) error`、`TotalBalance(asset AssetID) *uint256.Int`、`RootAndVersion() ([32]byte, uint64)`；`DepositOp{Account, Asset, Amount, Ref}`；`WithdrawOp{Account, Asset, Amount, Ref}`；`AccountID [20]byte` 有 `.Hex()`；`AccountFromHex(string)`。
- `ledger.AssetID uint32`。
- 链上事件签名（topic0 = keccak256）：
  - `Deposit(address,uint32,uint256,uint256)`：topics[1]=account，topics[2]=assetId，data[0:32]=amount，data[32:64]=seq
  - `Withdrawal(address,uint32,uint256,uint256)`：topics[1]=account，topics[2]=assetId，data[0:32]=amount，data[32:64]=rootIndex
  - `ExitFinalized(address,uint32,uint256,uint256)`：同 Withdrawal
  - `StateRootCommitted(uint256,bytes32,uint64)`：topics[1]=rootIndex，data[0:32]=root，data[32:64]=timestamp

---

## Task 1: 依赖 + 记录类型

**Files:**
- Modify: `backend/go.mod`（+bbolt）
- Create: `backend/indexer/types.go`

- [ ] **Step 1: 添加 bbolt 依赖**

Run: `cd D:\code\web3\backend && go get go.etcd.io/bbolt@v1.3.11`
Expected: 成功，`go.mod` 与 `go.sum` 更新

- [ ] **Step 2: 写记录类型**

```go
// Package indexer persists order-book history (trades, on-chain funding
// events, state roots) and can rebuild an independent ledger from on-chain
// events to reconcile against the running ledger. It never mutates the
// running ledger or the matching kernel.
package indexer

// TradeRecord is the persisted form of one on-chain-book fill.
type TradeRecord struct {
	TradeID   uint64 `json:"tradeId"`
	Market    uint32 `json:"market"`
	Price     uint64 `json:"price"`
	Qty       string `json:"qty"`
	Notional  string `json:"notional"`
	Maker     string `json:"maker"`
	Taker     string `json:"taker"`
	MakerSide string `json:"makerSide"`
}

// FundEvent is one on-chain funding event (deposit / withdrawal / forced exit).
type FundEvent struct {
	Kind      string `json:"kind"` // deposit | withdrawal | exit
	TxHash    string `json:"txHash"`
	LogIndex  uint   `json:"logIndex"`
	Block     uint64 `json:"block"`
	Account   string `json:"account"`
	Asset     uint32 `json:"asset"`
	Amount    string `json:"amount"`
	Seq       uint64 `json:"seq,omitempty"`       // deposit only
	RootIndex uint64 `json:"rootIndex,omitempty"` // withdrawal/exit only
}

// RootEvent is one committed state root.
type RootEvent struct {
	RootIndex uint64 `json:"rootIndex"`
	Root      string `json:"root"`
	Timestamp uint64 `json:"timestamp"`
	Block     uint64 `json:"block"`
}

// AssetDelta reports the per-asset reconciliation result between a rebuilt
// ledger and the running ledger.
type AssetDelta struct {
	Asset   ledger.AssetID `json:"asset"`
	Rebuilt string         `json:"rebuilt"`
	Running string         `json:"running"`
	Match   bool           `json:"match"`
}
```

> `types.go` 需要 import `web3-amm-dex-backend/ledger`（仅 `AssetID`）。

- [ ] **Step 3: 编译验证**

Run: `cd D:\code\web3\backend && go build ./indexer/`
Expected: 退出码 0

- [ ] **Step 4: 提交**

```bash
git add backend/go.mod backend/go.sum backend/indexer/types.go
git commit -m "feat(indexer): add bbolt dep and record types"
```

---

## Task 2: bbolt Store

**Files:**
- Create: `backend/indexer/store.go`
- Test: `backend/indexer/store_test.go`

- [ ] **Step 1: 写失败测试**

```go
package indexer

import (
	"testing"

	"go.etcd.io/bbolt"
)

func mustOpenStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStorePutTradeIdempotent(t *testing.T) {
	s := mustOpenStore(t)
	tr := TradeRecord{TradeID: 1, Market: 1, Price: 100, Qty: "1", Notional: "100", Maker: "0xaa", Taker: "0xbb", MakerSide: "sell"}
	if err := s.PutTrade(tr); err != nil {
		t.Fatalf("PutTrade: %v", err)
	}
	// Same key twice must not create a duplicate.
	tr2 := tr
	tr2.Notional = "999"
	if err := s.PutTrade(tr2); err != nil {
		t.Fatalf("PutTrade again: %v", err)
	}
	got, err := s.QueryTrades(0, "", "", 10)
	if err != nil {
		t.Fatalf("QueryTrades: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (idempotent overwrite)", len(got))
	}
	if got[0].Notional != "999" {
		t.Fatalf("notional = %s, want 999 (last write wins)", got[0].Notional)
	}
}

func TestStoreFundEventIdempotent(t *testing.T) {
	s := mustOpenStore(t)
	e := FundEvent{Kind: "deposit", TxHash: "0xtx", LogIndex: 0, Block: 5, Account: "0xaa", Asset: 1, Amount: "100", Seq: 1}
	if err := s.PutFundEvent(e); err != nil {
		t.Fatalf("PutFundEvent: %v", err)
	}
	if err := s.PutFundEvent(e); err != nil {
		t.Fatalf("PutFundEvent again: %v", err)
	}
	got, err := s.QueryFundEvents("", 0, 10)
	if err != nil {
		t.Fatalf("QueryFundEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}

func TestStoreCursorRoundTrip(t *testing.T) {
	s := mustOpenStore(t)
	if got, _ := s.LastProcessedBlock(); got != 0 {
		t.Fatalf("initial cursor = %d, want 0", got)
	}
	if err := s.SetLastProcessedBlock(42); err != nil {
		t.Fatalf("SetLastProcessedBlock: %v", err)
	}
	if got, _ := s.LastProcessedBlock(); got != 42 {
		t.Fatalf("cursor = %d, want 42", got)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/index.db"
	s, _ := OpenStore(path)
	_ = s.PutRootEvent(RootEvent{RootIndex: 1, Root: "0xrr", Block: 7})
	_ = s.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	roots, err := s2.QueryRoots(0, 100)
	if err != nil {
		t.Fatalf("QueryRoots: %v", err)
	}
	if len(roots) != 1 || roots[0].RootIndex != 1 {
		t.Fatalf("roots = %v, want exactly root 1", roots)
	}
}

// Keep bbolt imported for the doc reference even if unused here.
var _ = bbolt.DefaultOptions
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run TestStore -v`
Expected: FAIL — `undefined: OpenStore`

- [ ] **Step 3: 实现 Store**

```go
package indexer

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketTrades    = []byte("trades")
	bucketFunds     = []byte("fund_events")
	bucketRoots     = []byte("root_events")
	bucketCursor    = []byte("cursor")
	cursorKey       = []byte("chain_events")
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
	return s.putJSON(bucketTrades, []byte(fmt.Sprintf("trade:%d", t.TradeID)), t)
}

// PutFundEvent upserts a funding event keyed by txHash:logIndex.
func (s *Store) PutFundEvent(e FundEvent) error {
	return s.putJSON(bucketFunds, []byte(e.TxHash+":"+fmt.Sprint(e.LogIndex)), e)
}

// PutRootEvent upserts a root event keyed by root:<index>.
func (s *Store) PutRootEvent(e RootEvent) error {
	return s.putJSON(bucketRoots, []byte(fmt.Sprintf("root:%d", e.RootIndex)), e)
}

func (s *Store) putJSON(bucket, key []byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(key, data)
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
	out := []FundEvent{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketFunds).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
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
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// QueryRoots returns root events in ascending root-index order, up to limit.
func (s *Store) QueryRoots(from, limit int) ([]RootEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []RootEvent{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketRoots).Cursor()
		start := []byte(fmt.Sprintf("root:%d", from))
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
```

- [ ] **Step 4: 补二进制辅助（store.go 末尾）**

```go
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
```

store.go 的 import 需含 `"fmt"`、`"encoding/json"`、`bolt "go.etcd.io/bbolt"`。

- [ ] **Step 5: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run TestStore -v`
Expected: PASS（4 个测试全过）

- [ ] **Step 6: 提交**

```bash
git add backend/indexer/store.go backend/indexer/store_test.go
git commit -m "feat(indexer): bbolt-backed store with idempotent writes and queries"
```

---

## Task 3: IndexerService（订阅成交 + 轮询链上事件）

**Files:**
- Create: `backend/indexer/indexer.go`
- Test: `backend/indexer/indexer_test.go`

- [ ] **Step 1: 写失败测试（onTrade + 事件轮询）**

```go
package indexer

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

func tradeLog(t *testing.T, kind string, block uint64, account common.Address, assetID, val uint64, seq uint64) services.Log {
	t.Helper()
	topic0 := crypto.Keccak256Hash([]byte(kind)).Hex()
	topic1 := common.BytesToHash(account.Bytes()).Hex()
	topic2 := common.BigToHash(new(big.Int).SetUint64(assetID)).Hex()
	data := make([]byte, 64)
	amt := new(big.Int).SetUint64(val).Bytes()
	copy(data[32-len(amt):32], amt)
	seqB := common.BigToHash(new(big.Int).SetUint64(seq))
	copy(data[32:64], seqB.Bytes())
	return services.Log{
		Address:     "0xvault",
		Topics:      []string{topic0, topic1, topic2},
		Data:        data,
		BlockNumber: block,
		TxHash:      "0xtx" + new(big.Int).SetUint64(block).String(),
		LogIndex:    uint(block),
	}
}

func TestIndexerOnTradeWritesHistory(t *testing.T) {
	s := mustOpenStore(t)
	bus := services.NewEventBus()
	idx := New(s, bus, nil, "0xvault", 1)

	if err := idx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer idx.Stop()

	var a, b ledger.AccountID
	a[19] = 1
	b[19] = 2
	tr := matching.Trade{
		ID: 7, Market: 1, Price: 100,
		Qty: uint256.NewInt(1), Notional: uint256.NewInt(100),
		Maker: a, Taker: b, MakerSide: matching.SideSell,
	}
	_ = bus.Publish(services.Event{Topic: services.TopicOrderBookTrade, Data: services.TradeEvent{Trade: tr}})

	got, err := s.QueryTrades(0, "", 10)
	if err != nil {
		t.Fatalf("QueryTrades: %v", err)
	}
	if len(got) != 1 || got[0].TradeID != 7 {
		t.Fatalf("trades = %v, want exactly trade 7", got)
	}
}

func TestIndexerPollOnceWritesEventsAndAdvancesCursor(t *testing.T) {
	s := mustOpenStore(t)
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 5, account, 1, 100, 1),
		tradeLog(t, "Withdrawal(address,uint32,uint256,uint256)", 10, account, 1, 30, 2),
	}

	idx := New(s, nil, adapter, "0xvault", 1)
	if err := idx.PollOnce(); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	events, err := s.QueryFundEvents("", 0, 10)
	if err != nil {
		t.Fatalf("QueryFundEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if got, _ := s.LastProcessedBlock(); got != 19 {
		t.Fatalf("cursor = %d, want 19", got)
	}
}

func TestIndexerOnTradeWithNoSubscriberStillWrites(t *testing.T) {
	// onTrade must write even when the indexer's own Start was never called
	// (direct subscription registration is part of Start; here we call the
	// handler method directly to decouple from lifecycle).
	s := mustOpenStore(t)
	idx := New(s, nil, nil, "0xvault", 1)
	var a ledger.AccountID
	a[19] = 9
	tr := matching.Trade{ID: 1, Market: 1, Price: 1, Qty: uint256.NewInt(1), Notional: uint256.NewInt(1), Maker: a, Taker: a, MakerSide: matching.SideBuy}
	if err := idx.handleTrade(services.Event{Data: services.TradeEvent{Trade: tr}}); err != nil {
		t.Fatalf("handleTrade: %v", err)
	}
	got, _ := s.QueryTrades(0, "", 10)
	if len(got) != 1 {
		t.Fatalf("trades = %d, want 1", len(got))
	}
}

func TestIndexerPollRespectsConfirmations(t *testing.T) {
	s := mustOpenStore(t)
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x2222222222222222222222222222222222222222")
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 19, account, 1, 10, 1),
	}
	idx := New(s, nil, adapter, "0xvault", 5) // only blocks <= 15 confirmed
	if err := idx.PollOnce(); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	events, _ := s.QueryFundEvents("", 0, 10)
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0 (unconfirmed)", len(events))
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run TestIndexer -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: 实现 IndexerService**

```go
package indexer

import (
	"encoding/binary"
	"errors"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

// On-chain event topic hashes (keccak256 of the ABI signature).
var (
	depositTopic  = crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)"))
	withdrawTopic = crypto.Keccak256Hash([]byte("Withdrawal(address,uint32,uint256,uint256)"))
	exitTopic     = crypto.Keccak256Hash([]byte("ExitFinalized(address,uint32,uint256,uint256)"))
	rootTopic     = crypto.Keccak256Hash([]byte("StateRootCommitted(uint256,bytes32,uint64)"))
)

// IndexerService persists trades (from the EventBus) and on-chain funding/root
// events (from the BlockchainAdapter) into the Store. It is a services.Service.
type IndexerService struct {
	store         *Store
	bus           *services.EventBus
	adapter       services.BlockchainAdapter
	chain         string
	confirmations uint64

	mu       sync.Mutex
	nextBlock uint64
	interval  time.Duration
	stop      chan struct{}
	started   bool
}

func New(store *Store, bus *services.EventBus, adapter services.BlockchainAdapter, chain string, confirmations uint64) *IndexerService {
	if confirmations == 0 {
		confirmations = 1
	}
	return &IndexerService{
		store:         store,
		bus:           bus,
		adapter:       adapter,
		chain:         chain,
		confirmations: confirmations,
		nextBlock:     1,
		interval:      5 * time.Second,
		stop:          make(chan struct{}),
	}
}

func (s *IndexerService) Name() string { return "IndexerService" }

func (s *IndexerService) Initialize(cfg services.Config) error {
	if cfg.RequiredConfirmations > 0 {
		s.confirmations = cfg.RequiredConfirmations
	}
	if cfg.SettlementAddress != "" {
		s.chain = cfg.SettlementAddress
	}
	return nil
}

// SetPollInterval sets the chain-event poll interval.
func (s *IndexerService) SetPollInterval(d time.Duration) {
	if d > 0 {
		s.interval = d
	}
}

func (s *IndexerService) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.stop = make(chan struct{})
	stop := s.stop
	s.mu.Unlock()

	if s.bus != nil {
		s.bus.Subscribe(services.TopicOrderBookTrade, s.handleTrade)
	}
	if s.adapter != nil {
		go func() {
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if err := s.PollOnce(); err != nil {
						log.Printf("indexer: poll: %v", err)
					}
				}
			}
		}()
	}
	return nil
}

func (s *IndexerService) Stop() error {
	s.mu.Lock()
	if s.started {
		close(s.stop)
		s.started = false
	}
	s.mu.Unlock()
	return nil
}

// Store exposes the underlying store for query handlers.
func (s *IndexerService) Store() *Store { return s.store }

// handleTrade persists one fill.
func (s *IndexerService) handleTrade(e services.Event) error {
	ev, ok := e.Data.(services.TradeEvent)
	if !ok {
		return errors.New("indexer: trade event has wrong type")
	}
	t := ev.Trade
	return s.store.PutTrade(TradeRecord{
		TradeID:   uint64(t.ID),
		Market:    uint32(t.Market),
		Price:     uint64(t.Price),
		Qty:       t.Qty.Dec(),
		Notional:  t.Notional.Dec(),
		Maker:     t.Maker.Hex(),
		Taker:     t.Taker.Hex(),
		MakerSide: t.MakerSide.String(),
	})
}

// PollOnce fetches and persists all confirmed funding/root logs once, then
// advances the cursor. It is idempotent across runs thanks to the cursor and
// the deterministic store keys.
func (s *IndexerService) PollOnce() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.adapter == nil {
		return nil
	}

	// Seed the cursor from the store on first run so a restart resumes where it
	// left off rather than re-scanning from genesis.
	if s.nextBlock == 1 {
		if h, err := s.store.LastProcessedBlock(); err == nil && h > 0 {
			s.nextBlock = h + 1
		}
	}

	latest, err := s.adapter.GetBlockNumber()
	if err != nil {
		return err
	}
	if latest < s.confirmations {
		return nil
	}
	to := latest - s.confirmations
	if to < s.nextBlock {
		return nil
	}

	topics := [][]string{{
		depositTopic.Hex(), withdrawTopic.Hex(), exitTopic.Hex(), rootTopic.Hex(),
	}}
	logs, err := s.adapter.GetLogs(s.nextBlock, to, s.chain, topics)
	if err != nil {
		return err
	}
	for _, l := range logs {
		if err := s.persistLog(l); err != nil {
			return err
		}
	}
	s.nextBlock = to + 1
	return s.store.SetLastProcessedBlock(to)
}

// persistLog decodes one funding/root log and stores it. Unknown topics are
// ignored (defensive; the filter should have excluded them).
func (s *IndexerService) persistLog(l services.Log) error {
	ev, err := decodeEvent(l)
	if err != nil {
		return err
	}
	if ev == nil {
		return nil // not a recognized funding/root event
	}
	switch ev.Kind {
	case "root":
		return s.store.PutRootEvent(RootEvent{
			RootIndex: ev.RootIndex,
			Root:      ev.Root,
			Timestamp: ev.Timestamp,
			Block:     l.BlockNumber,
		})
	default:
		return s.store.PutFundEvent(FundEvent{
			Kind:      ev.Kind,
			TxHash:    l.TxHash,
			LogIndex:  l.LogIndex,
			Block:     l.BlockNumber,
			Account:   ev.Account,
			Asset:     ev.Asset,
			Amount:    ev.Amount,
			Seq:       ev.Seq,
			RootIndex: ev.RootIndex,
		})
	}
}

// decodedEvent is the intermediate parsed form of one chain log.
type decodedEvent struct {
	Kind      string // deposit | withdrawal | exit | root
	Account   string
	Asset     uint32
	Amount    string
	Seq       uint64
	RootIndex uint64
	Root      string
	Timestamp uint64
}

func decodeEvent(l services.Log) (*decodedEvent, error) {
	if len(l.Topics) < 1 {
		return nil, errors.New("indexer: log missing topic0")
	}
	t0 := l.Topics[0]
	switch {
	case t0 == depositTopic.Hex():
		a, asset, amount, err := decodeAccountAssetAmount(l)
		if err != nil {
			return nil, err
		}
		seq := uint64(0)
		if len(l.Data) >= 64 {
			seq = binary.BigEndian.Uint64(l.Data[56:64])
		}
		return &decodedEvent{Kind: "deposit", Account: a, Asset: asset, Amount: amount, Seq: seq}, nil
	case t0 == withdrawTopic.Hex():
		a, asset, amount, err := decodeAccountAssetAmount(l)
		if err != nil {
			return nil, err
		}
		rootIdx := uint64(0)
		if len(l.Data) >= 64 {
			rootIdx = binary.BigEndian.Uint64(l.Data[56:64])
		}
		return &decodedEvent{Kind: "withdrawal", Account: a, Asset: asset, Amount: amount, RootIndex: rootIdx}, nil
	case t0 == exitTopic.Hex():
		a, asset, amount, err := decodeAccountAssetAmount(l)
		if err != nil {
			return nil, err
		}
		rootIdx := uint64(0)
		if len(l.Data) >= 64 {
			rootIdx = binary.BigEndian.Uint64(l.Data[56:64])
		}
		return &decodedEvent{Kind: "exit", Account: a, Asset: asset, Amount: amount, RootIndex: rootIdx}, nil
	case t0 == rootTopic.Hex():
		if len(l.Data) < 64 {
			return nil, errors.New("indexer: root log short data")
		}
		root := "0x" + common.Bytes2Hex(l.Data[0:32])
		ts := binary.BigEndian.Uint64(l.Data[56:64])
		rootIdx := uint64(0)
		if len(l.Topics) >= 2 {
			rootIdx = new(big.Int).SetBytes(common.HexToHash(l.Topics[1]).Bytes()).Uint64()
		}
		return &decodedEvent{Kind: "root", RootIndex: rootIdx, Root: root, Timestamp: ts}, nil
	default:
		return nil, nil
	}
}

func decodeAccountAssetAmount(l services.Log) (string, uint32, string, error) {
	if len(l.Topics) < 3 {
		return "", 0, "", errors.New("indexer: fund log missing topics")
	}
	if len(l.Data) < 32 {
		return "", 0, "", errors.New("indexer: fund log short data")
	}
	var a ledger.AccountID
	copy(a[:], common.HexToHash(l.Topics[1]).Bytes()[12:32])
	asset := uint32(new(big.Int).SetBytes(common.HexToHash(l.Topics[2]).Bytes()).Uint64())
	amount := new(big.Int).SetBytes(l.Data[0:32]).String()
	return a.Hex(), asset, amount, nil
}

var _ = uint256.NewInt // keep holiman/uint256 imported for potential reuse
```

> indexer.go import 需含 `"encoding/binary"`、`"errors"`、`"log"`、`"math/big"`、`"sync"`、`"time"`、
> `"github.com/ethereum/go-ethereum/common"`、`"github.com/ethereum/go-ethereum/crypto"`、
> `"github.com/holiman/uint256"`、`"web3-amm-dex-backend/ledger"`、`"web3-amm-dex-backend/matching"`、
> `"web3-amm-dex-backend/services"`。若 `uint256` 实际未用（用 `big.Int` 即可），删掉 `holiman/uint256` import
> 与 `var _ = uint256.NewInt` 那行。

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run TestIndexer -v`
Expected: PASS（4 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/indexer/indexer.go backend/indexer/indexer_test.go
git commit -m "feat(indexer): IndexerService subscribing trades and polling chain events"
```

---

## Task 4: 独立重建 + 对账

**Files:**
- Create: `backend/indexer/rebuild.go`
- Test: `backend/indexer/rebuild_test.go`

- [ ] **Step 1: 写失败测试**

```go
package indexer

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

func TestRebuildAndCompareTotals(t *testing.T) {
	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 30
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 5, account, 1, 100, 1),
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 6, account, 1, 50, 2),
		tradeLog(t, "Withdrawal(address,uint32,uint256,uint256)", 7, account, 1, 30, 1),
	}

	rebuilt, err := Rebuild(adapter, "0xvault", 1)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	var a ledger.AccountID
	copy(a[:], account.Bytes())
	if got := rebuilt.TotalBalance(1).Uint64(); got != 120 {
		t.Fatalf("rebuilt total = %d, want 120 (100+50-30)", got)
	}

	// Running ledger with the same total (the matching transfer legs are zero-sum
	// so they don't change the total).
	running := ledger.New()
	_ = running.Apply(
		ledger.DepositOp{Account: a, Asset: 1, Amount: uint256.NewInt(150), Ref: "d"},
		ledger.WithdrawOp{Account: a, Asset: 1, Amount: uint256.NewInt(30), Ref: "w"},
	)

	deltas, err := CompareTotals(rebuilt, running)
	if err != nil {
		t.Fatalf("CompareTotals: %v", err)
	}
	for _, d := range deltas {
		if !d.Match {
			t.Fatalf("asset %d mismatch: rebuilt=%s running=%s", d.Asset, d.Rebuilt, d.Running)
		}
	}
	if len(deltas) == 0 {
		t.Fatal("expected at least one asset delta")
	}
}

func TestCompareTotalsDetectsMismatch(t *testing.T) {
	rebuilt := ledger.New()
	_ = rebuilt.Apply(ledger.DepositOp{Account: [20]byte{1}, Asset: 1, Amount: uint256.NewInt(100), Ref: "d"})

	running := ledger.New()
	_ = running.Apply(ledger.DepositOp{Account: [20]byte{1}, Asset: 1, Amount: uint256.NewInt(90), Ref: "d"})

	deltas, err := CompareTotals(rebuilt, running)
	if err != nil {
		t.Fatalf("CompareTotals: %v", err)
	}
	if len(deltas) != 1 || deltas[0].Match {
		t.Fatalf("deltas = %v, want exactly one non-matching asset", deltas)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run 'TestRebuild|TestCompareTotals' -v`
Expected: FAIL — `undefined: Rebuild, CompareTotals`

- [ ] **Step 3: 实现 rebuild**

```go
package indexer

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

// Rebuild replays the settlement contract's funding events into a fresh ledger,
// yielding per-asset total balances. Matching legs are zero-sum and absent from
// the chain, so only per-asset totals are meaningful (spec §6.1). It is
// read-only with respect to the running ledger.
func Rebuild(adapter services.BlockchainAdapter, chain string, fromBlock uint64) (*ledger.Ledger, error) {
	l := ledger.New()
	if adapter == nil {
		return nil, errors.New("indexer: rebuild requires an adapter")
	}
	latest, err := adapter.GetBlockNumber()
	if err != nil {
		return nil, err
	}
	topics := [][]string{{
		depositTopic.Hex(), withdrawTopic.Hex(), exitTopic.Hex(),
	}}
	logs, err := adapter.GetLogs(fromBlock, latest, chain, topics)
	if err != nil {
		return nil, err
	}
	// Rebuild in event order (block, then log index) so deposits/withdrawals
	// apply in the order they occurred.
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].LogIndex < logs[j].LogIndex
	})
	for _, lg := range logs {
		ev, err := decodeEvent(lg)
		if err != nil {
			return nil, err
		}
		if ev == nil || ev.Kind == "root" {
			continue
		}
		account, err := ledger.AccountFromHex(ev.Account)
		if err != nil {
			return nil, err
		}
		amount, err := uint256.FromDecimal(ev.Amount)
		if err != nil {
			return nil, fmt.Errorf("indexer: rebuild: bad amount %q: %w", ev.Amount, err)
		}
		ref := lg.TxHash + ":" + strconv.Itoa(int(lg.LogIndex))
		switch ev.Kind {
		case "deposit":
			if err := l.Apply(ledger.DepositOp{Account: account, Asset: ledger.AssetID(ev.Asset), Amount: amount, Ref: ref}); err != nil {
				return nil, err
			}
		case "withdrawal", "exit":
			if err := l.Apply(ledger.WithdrawOp{Account: account, Asset: ledger.AssetID(ev.Asset), Amount: amount, Ref: ref}); err != nil {
				return nil, err
			}
		}
	}
	return l, nil
}

// CompareTotals compares the rebuilt ledger's per-asset totals against the
// running ledger's, returning one delta per asset present in either ledger.
func CompareTotals(rebuilt, running *ledger.Ledger) ([]AssetDelta, error) {
	if rebuilt == nil || running == nil {
		return nil, errors.New("indexer: compare requires both ledgers")
	}
	assets := map[ledger.AssetID]bool{}
	// Enumerate assets by scanning known asset range is not possible generically;
	// instead, collect the union from a bounded scan is also not available. We
	// use a caller-friendly approach: both ledgers must expose their assets.
	//
	// Because Ledger has no asset enumeration API, CompareTotals works over the
	// assets observed by comparing TotalBalance across the union gathered from
	// the ledger's leaf entries via a helper. If that is unavailable, the
	// caller must pass the asset set.
	_ = assets
	return nil, errors.New("indexer: CompareTotals needs the asset set (see plan note)")
}
```

> **注意**：`ledger.Ledger` 没有公开的「枚举资产」API。上面的 `CompareTotals` 是占位，说明缺口。
> 两个解决选项：**(A)** 给 `ledger` 加一个 `Assets() []AssetID` 方法（遍历 balances 收集资产集合，
> 只读、无副作用）；**(B)** 让 `CompareTotals` 接受资产集合参数。**本计划选 A**——它更符合"对账遍历
> 所有资产"的语义，且是 `ledger` 包一个干净的小增强。

因此本任务实际还需一个子步骤：**给 `ledger.Ledger` 加 `Assets()` 方法**。

- [ ] **Step 4: 给 ledger 加 Assets()（modify backend/ledger/ledger.go）**

在 `TotalBalance` 方法后加：

```go
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
```

> ledger.go 需 import `"sort"`（若未引入）。

- [ ] **Step 5: 用 Assets() 实现 CompareTotals 与 rebuild 辅助**

把 `rebuild.go` 的 `CompareTotals` 替换为：

```go
// CompareTotals compares the rebuilt ledger's per-asset totals against the
// running ledger's, returning one delta per asset present in either ledger.
func CompareTotals(rebuilt, running *ledger.Ledger) ([]AssetDelta, error) {
	if rebuilt == nil || running == nil {
		return nil, errors.New("indexer: compare requires both ledgers")
	}
	assets := map[ledger.AssetID]bool{}
	for _, a := range rebuilt.Assets() {
		assets[a] = true
	}
	for _, a := range running.Assets() {
		assets[a] = true
	}
	sorted := make([]ledger.AssetID, 0, len(assets))
	for a := range assets {
		sorted = append(sorted, a)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := make([]AssetDelta, 0, len(sorted))
	for _, a := range sorted {
		rb := rebuilt.TotalBalance(a).Dec()
		rn := running.TotalBalance(a).Dec()
		out = append(out, AssetDelta{Asset: a, Rebuilt: rb, Running: rn, Match: rb == rn})
	}
	return out, nil
}
```

> rebuild.go import 需含 `"errors"`、`"fmt"`、`"sort"`、`"strconv"`、
> `"github.com/holiman/uint256"`、`"web3-amm-dex-backend/ledger"`、
> `"web3-amm-dex-backend/services"`。`common` 若未实际用到则删掉。

- [ ] **Step 6: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run 'TestRebuild|TestCompareTotals' -v && go test ./ledger/ -run TestAssets -v`
Expected: PASS（indexer 2 个 + ledger 新增的 Assets 测试）

> 需给 `ledger` 加一个 `Assets()` 的测试（Task 4 附带）。在 `backend/ledger/ledger_test.go` 追加：

```go
func TestAssetsSorted(t *testing.T) {
	l := New()
	_ = l.Apply(DepositOp{Account: [20]byte{1}, Asset: 3, Amount: uint256.NewInt(1), Ref: "a"})
	_ = l.Apply(DepositOp{Account: [20]byte{2}, Asset: 1, Amount: uint256.NewInt(1), Ref: "b"})
	got := l.Assets()
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("Assets = %v, want [1 3]", got)
	}
}
```

- [ ] **Step 7: 提交**

```bash
git add backend/indexer/rebuild.go backend/indexer/rebuild_test.go backend/ledger/ledger.go backend/ledger/ledger_test.go
git commit -m "feat(indexer): independent rebuild and per-asset reconciliation"
```

---

## Task 5: order_book recover 补发成交事件

**Files:**
- Modify: `backend/services/order_book.go`
- Test: `backend/services/order_book_test.go`（追加）

- [ ] **Step 1: 写失败测试**

在 `backend/services/order_book_test.go` 追加：

```go
func TestRecoverReemitsTradeEvents(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	journal := mustJournal(t, dir)
	bus := NewEventBus()

	// Fund two accounts for a crossing pair.
	deposit := func(a ledger.AccountID, asset ledger.AssetID, n uint64, ref string) {
		t.Helper()
		if err := l.Apply(ledger.DepositOp{Account: a, Asset: asset, Amount: uint256.NewInt(n), Ref: ref}); err != nil {
			t.Fatalf("deposit: %v", err)
		}
	}
	seller := obAcct(1)
	buyer := obAcct(2)
	deposit(seller, 2, 100, "d1") // base asset 2
	deposit(buyer, 1, 100, "d2")  // quote asset 1

	ob := mustNewOrderBook(t, obMarkets(), l, holds, journal, bus)
	defer ob.Close()

	// Place a sell then a crossing buy so a trade occurs and is journaled.
	if _, err := ob.Place(limitReq(seller, matching.SideSell, 10, 5)); err != nil {
		t.Fatalf("place sell: %v", err)
	}
	if _, err := ob.Place(limitReq(buyer, matching.SideBuy, 10, 5)); err != nil {
		t.Fatalf("place buy: %v", err)
	}

	// Count trade events on a fresh bus while a fresh service replays the WAL.
	count := 0
	bus2 := NewEventBus()
	bus2.Subscribe(TopicOrderBookTrade, func(e Event) error {
		count++
		return nil
	})

	ob2 := mustNewOrderBook(t, obMarkets(), l, holds, mustJournal(t, dir), bus2)
	defer ob2.Close()
	_ = ob2 // recovery runs in the constructor

	if count == 0 {
		t.Fatal("recover should re-emit the journaled trade event")
	}
}
```

> 注意：这个测试构造了新账本 `l` 与新的 `journal`，但 `mustNewOrderBook` 的构造函数里 `recover` 会重放
> journal；关键在于 `recover` 补发事件到传入的 `bus2`。上面的测试需要仔细对齐 `obMarkets`/`obAcct`/
> `limitReq`/`mustJournal`/`mustNewOrderBook` 这些 helper 是否都在 `services` 包测试里已定义（它们来自
> 既有 `order_book_test.go`，确认存在）。若 `limitReq` 的默认市场是 1、base=2、quote=1（见既有测试），
> 则上面的充值资产号一致。

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestRecoverReemitsTradeEvents -v`
Expected: FAIL（count == 0，recover 未发事件）

- [ ] **Step 3: 在 recover 补发事件**

修改 `backend/services/order_book.go` 的 `recover`，在 `case kindPlace` 里 `settleTrades(res.Trades)`
成功之后、`continue` 之前，对每笔成交补发事件：

在 `recover` 的 `case kindPlace` 分支里，找到：

```go
			if err := s.settleTrades(res.Trades); err != nil {
				return fmt.Errorf("services: order book: replay settle: %w", err)
			}
```

在其后插入：

```go
			// Re-emit each replayed fill so downstream subscribers (notably the
			// indexer) observe the trades the crash already settled. The trade
			// ids are deterministic, so a subscriber's idempotent keys make a
			// duplicate delivery harmless.
			for _, t := range res.Trades {
				_ = s.bus.Publish(Event{Topic: TopicOrderBookTrade, Data: TradeEvent{Trade: t}})
			}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestRecoverReemitsTradeEvents -v && go test ./services/ -count=1`
Expected: PASS（新测试 + 既有 services 全绿）

- [ ] **Step 5: 提交**

```bash
git add backend/services/order_book.go backend/services/order_book_test.go
git commit -m "fix(order-book): re-emit trade events on journal replay"
```

---

## Task 6: 查询路由 + 装配

**Files:**
- Create: `backend/indexer/query.go`
- Test: `backend/indexer/query_test.go`
- Modify: `backend/config/config.go`（+IndexerConfig）
- Modify: `backend/main.go`（装配 + 挂路由）

- [ ] **Step 1: 写失败测试（QueryServer HTTP）**

```go
package indexer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/services"
)

func TestQueryServerTrades(t *testing.T) {
	s := mustOpenStore(t)
	_ = s.PutTrade(TradeRecord{TradeID: 1, Market: 1, Price: 5, Qty: "1", Notional: "5", Maker: "0xaa", Taker: "0xbb", MakerSide: "sell"})

	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 20

	qs := &QueryServer{Store: s, Adapter: adapter, Chain: "0xvault"}
	r := gin.New()
	qs.Register(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/trades", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var trades []TradeRecord
	if err := json.Unmarshal(w.Body.Bytes(), &trades); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(trades))
	}
}

func TestQueryServerReconcile(t *testing.T) {
	s := mustOpenStore(t)
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 30
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 5, common.HexToAddress("0x1111111111111111111111111111111111111111"), 1, 100, 1),
	}

	qs := &QueryServer{Store: s, Adapter: adapter, Chain: "0xvault", Running: nil}
	r := gin.New()
	qs.Register(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/reconcile", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["assets"]; !ok {
		t.Fatalf("body missing assets: %v", body)
	}
}
```

> 需 import `"github.com/ethereum/go-ethereum/common"`（test 里用到）。

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run TestQueryServer -v`
Expected: FAIL — `undefined: QueryServer`

- [ ] **Step 3: 实现 QueryServer**

```go
package indexer

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

// QueryServer exposes read-only history queries and the reconcile endpoint. It
// is assembled by main and mounted onto the existing gin engine.
type QueryServer struct {
	Store   *Store
	Adapter services.BlockchainAdapter
	Chain   string
	Running *ledger.Ledger // optional: the running ledger to reconcile against
}

// Register mounts the /v1 read routes. Read endpoints are unauthenticated
// (consistent with subproject 4).
func (q *QueryServer) Register(r gin.IRouter) {
	v1 := r.Group("/v1")
	v1.GET("/trades", q.trades)
	v1.GET("/fund-events", q.fundEvents)
	v1.GET("/roots", q.roots)
	v1.POST("/reconcile", q.reconcile)
}

func (q *QueryServer) trades(c *gin.Context) {
	market, _ := strconv.ParseUint(c.DefaultQuery("market", "0"), 10, 32)
	account := c.DefaultQuery("account", "")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	out, err := q.Store.QueryTrades(uint32(market), account, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (q *QueryServer) fundEvents(c *gin.Context) {
	account := c.DefaultQuery("account", "")
	asset, _ := strconv.ParseUint(c.DefaultQuery("asset", "0"), 10, 32)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	out, err := q.Store.QueryFundEvents(account, uint32(asset), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (q *QueryServer) roots(c *gin.Context) {
	from, _ := strconv.Atoi(c.DefaultQuery("from", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	out, err := q.Store.QueryRoots(from, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (q *QueryServer) reconcile(c *gin.Context) {
	if q.Adapter == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reconcile requires an adapter"})
		return
	}
	rebuilt, err := Rebuild(q.Adapter, q.Chain, 1)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if q.Running == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reconcile requires the running ledger"})
		return
	}
	deltas, err := CompareTotals(rebuilt, q.Running)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	consistent := true
	for _, d := range deltas {
		if !d.Match {
			consistent = false
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"assets": deltas, "consistent": consistent})
}
```

> query.go import 需含 `"net/http"`、`"strconv"`、`"github.com/gin-gonic/gin"`、
> `"web3-amm-dex-backend/ledger"`、`"web3-amm-dex-backend/services"`。

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./indexer/ -run TestQueryServer -v`
Expected: PASS（2 个测试全过）

- [ ] **Step 5: config 加 IndexerConfig**

在 `backend/config/config.go` 追加：

```go
// IndexerConfig configures the history indexer (subproject 5).
type IndexerConfig struct {
	DBPath       string
	PollInterval int64
}

// LoadIndexerConfig reads indexer settings from the environment.
func LoadIndexerConfig() IndexerConfig {
	cfg := IndexerConfig{PollInterval: 5}
	if v := os.Getenv("INDEXER_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("INDEXER_POLL_INTERVAL"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.PollInterval = n
		}
	}
	return cfg
}
```

> `config.go` 已 import `os`、`strconv`。

- [ ] **Step 6: main.go 装配 + 挂路由**

`startSettlementStack` 已返回 `*services.SettlementStack`，其 `Adapter` / `Bus` / `Ledger` / `Registry` 字段
公开可用；合约地址与确认数从 `config.LoadSettlementConfig()` 取。在 `main.go` 里，`r := gin.Default()` 之后、
挂完 gateway 之后，装配索引器并挂路由：

```go
	// Mount the history indexer's read-only query routes and reconcile endpoint.
	var istore *indexer.Store
	if settlement != nil {
		sc := config.LoadSettlementConfig()
		ic := config.LoadIndexerConfig()
		istore, err = indexer.OpenStore(ic.DBPath)
		if err != nil {
			log.Fatalf("Failed to open indexer store: %v", err)
		}
		idxSvc := indexer.New(istore, settlement.Bus, settlement.Adapter, sc.ContractAddress, sc.Confirmations)
		idxSvc.SetPollInterval(time.Duration(ic.PollInterval) * time.Second)
		qs := &indexer.QueryServer{
			Store:   istore,
			Adapter: settlement.Adapter,
			Chain:   sc.ContractAddress,
			Running: settlement.Ledger,
		}
		qs.Register(r)
		settlement.Registry.Register(idxSvc)
		if err := idxSvc.Initialize(services.Config{
			SettlementAddress:     sc.ContractAddress,
			RequiredConfirmations: sc.Confirmations,
		}); err != nil {
			log.Fatalf("Failed to initialize indexer: %v", err)
		}
		if err := idxSvc.Start(); err != nil {
			log.Fatalf("Failed to start indexer: %v", err)
		}
		log.Printf("indexer mounted at /v1/trades, /v1/fund-events, /v1/roots, /v1/reconcile")
	}
```

> `main.go` 的 import 需新增 `"time"`、`"web3-amm-dex-backend/indexer"`。`istore` 是 `var` 声明（不是 `:=`），
> 因为要在 shutdown 块里关闭它。`settlement.Registry.Register(idxSvc)` 使 `idxSvc` 随 stack 的
> `StopAll` 一起停止；`Start` 在这里手动调用，因为 `StartAll` 已经在 `startSettlementStack` 里执行过了。

shutdown 块里，在 `settlement.Close()` 之前关闭 `istore`：

```go
	if istore != nil {
		if err := istore.Close(); err != nil {
			log.Printf("indexer store shutdown error: %v", err)
		}
	}
	if settlement != nil {
		if err := settlement.Close(); err != nil {
			log.Printf("settlement stack shutdown error: %v", err)
		}
	}
```

- [ ] **Step 7: 全量编译 + 测试**

Run: `cd D:\code\web3\backend && go build ./... && go test ./... -count=1`
Expected: 全部 `ok`

- [ ] **Step 8: 提交**

```bash
git add backend/indexer/query.go backend/indexer/query_test.go backend/config/config.go backend/main.go
git commit -m "feat(indexer): query routes and main wiring"
```

---

## Task 7: README 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 追加索引器章节**

在 README 的 Order Book Gateway 章节后追加：

```markdown
## History Indexer (subproject 5)

The indexer persists trades, on-chain funding events, and state roots to bbolt,
and can rebuild an independent ledger from chain events to reconcile against the
running ledger.

### Query API (read-only, unauthenticated)

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/v1/trades?market=&account=&limit=` | Trade history |
| GET    | `/v1/fund-events?account=&asset=&limit=` | Deposit/withdrawal/exit history |
| GET    | `/v1/roots?from=&limit=` | State root history |
| POST   | `/v1/reconcile` | Rebuild from chain and reconcile per-asset totals |

### Configuration

| Key | Default | Description |
|-----|---------|-------------|
| `INDEXER_DB_PATH` | (required for persistence) | bbolt file path |
| `INDEXER_POLL_INTERVAL` | 5 | chain-event poll interval (seconds) |

### Residual trust assumptions

Reconciliation compares **per-asset totals** only: matching legs are zero-sum and
absent from the chain, so per-account balances differ between a rebuilt ledger
and the running ledger. Per-account reconciliation requires per-trade Merkle
proofs on-chain (deferred).
```

- [ ] **Step 2: 提交**

```bash
git add README.md
git commit -m "docs(indexer): document the history indexer and query API"
```

---

## 最终验证

- [ ] **Step 1: Go 全量**

Run: `cd D:\code\web3\backend && go build ./... && go vet ./... && go test ./... -count=1`
Expected: 全部 `ok`

- [ ] **Step 2: 合约回归**

Run: `cd D:\code\web3 && npx hardhat test`
Expected: 33 passing

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-10-18-orderbook-dex-indexer.md`.

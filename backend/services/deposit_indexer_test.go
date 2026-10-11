package services

import (
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"web3-amm-dex-backend/ledger"
)

// waitUntil polls cond until it is true or the timeout elapses. It lets a test
// observe progress made by a background service goroutine without sleeping
// blindly for a fixed period.
func waitUntil(t *testing.T, what string, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// countingIndexerAdapter wraps the mock and counts GetBlockNumber calls, which
// every PollOnce performs, so tests can observe real poll activity.
type countingIndexerAdapter struct {
	*MockBlockchainAdapter
	mu    sync.Mutex
	calls int
}

func (c *countingIndexerAdapter) GetBlockNumber() (uint64, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.MockBlockchainAdapter.GetBlockNumber()
}

func (c *countingIndexerAdapter) pollCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// waitStopped blocks until the indexer's loop goroutine has fully exited, so a
// test may safely mutate the shared mock without racing an in-flight poll.
func waitStopped(t *testing.T, idx *DepositIndexer) {
	t.Helper()
	waitUntil(t, "indexer goroutine to exit", func() bool { return idx.run.activeLoops() == 0 }, time.Second)
}

func depositLog(t *testing.T, block uint64, account common.Address, assetID uint32, amount string, seq uint64) Log {
	t.Helper()
	topic0 := crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)")).Hex()
	topic1 := common.BytesToHash(account.Bytes()).Hex()
	topic2 := common.BigToHash(common.Big1.SetUint64(uint64(assetID))).Hex()

	amtBytes, err := hex.DecodeString(amount)
	if err != nil {
		t.Fatalf("bad amount hex: %v", err)
	}
	data := make([]byte, 64)
	copy(data[32-len(amtBytes):32], amtBytes)
	seqB := common.BigToHash(common.Big1.SetUint64(seq))
	copy(data[32:64], seqB.Bytes())

	return Log{
		Address:     "0xvault",
		Topics:      []string{topic0, topic1, topic2},
		Data:        data,
		BlockNumber: block,
		TxHash:      "0xtx" + hex.EncodeToString(common.BigToHash(common.Big1.SetUint64(block)).Bytes()),
		LogIndex:    uint(block),
	}
}

func TestDepositIndexerCreditsOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	adapter.Logs = []Log{depositLog(t, 5, account, 1, "64", 1)} // 0x64 == 100

	l := ledger.New()
	idx := NewDepositIndexer(adapter, l, "0xvault", 1)

	if err := idx.PollOnce(); err != nil {
		t.Fatalf("poll: %v", err)
	}
	var a ledger.AccountID
	copy(a[:], account.Bytes())
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}

	// second poll must not double credit
	if err := idx.PollOnce(); err != nil {
		t.Fatalf("poll2: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 after re-poll", got)
	}

	// A fresh indexer over the same ledger has a fresh cursor (nextBlock=1), so
	// it re-observes the same log through GetLogs -> decodeDeposit -> Apply on
	// the identical "txHash:logIndex" ref. The ledger's Ref-key dedupe (the layer
	// that also protects a process restart) must prevent a second credit.
	fresh := NewDepositIndexer(adapter, l, "0xvault", 1)
	if err := fresh.PollOnce(); err != nil {
		t.Fatalf("poll fresh: %v", err)
	}
	if got := l.Balance(a, 1).Uint64(); got != 100 {
		t.Fatalf("balance = %d, want 100 after fresh indexer re-poll", got)
	}
}

func TestDepositIndexerRespectsConfirmations(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x2222222222222222222222222222222222222222")
	adapter.Logs = []Log{depositLog(t, 19, account, 1, "0a", 1)} // 0x0a == 10

	l := ledger.New()
	idx := NewDepositIndexer(adapter, l, "0xvault", 5) // needs block <= 15

	if err := idx.PollOnce(); err != nil {
		t.Fatalf("poll: %v", err)
	}
	var a ledger.AccountID
	copy(a[:], account.Bytes())
	if got := l.Balance(a, 1).Uint64(); got != 0 {
		t.Fatalf("balance = %d, want 0 (unconfirmed)", got)
	}
}

// TestDepositIndexerRestartDoesNotPanic drives a full Start -> Stop -> Start ->
// Stop cycle. The buggy implementation closes the stop channel on the first
// Stop without recreating it, so the second Stop closes an already-closed
// channel and panics.
func TestDepositIndexerRestartDoesNotPanic(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	idx := NewDepositIndexer(adapter, ledger.New(), "0xvault", 1)
	idx.interval = 5 * time.Millisecond

	for i := 0; i < 2; i++ {
		if err := idx.Start(); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if err := idx.Stop(); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		waitStopped(t, idx)
	}
}

// TestDepositIndexerRestartResumesPolling proves a restarted indexer actually
// polls again: the buggy implementation hands the restarted goroutine an
// already-closed stop channel, so it returns immediately and never observes the
// new deposit.
func TestDepositIndexerRestartResumesPolling(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	first := common.HexToAddress("0x3333333333333333333333333333333333333333")
	adapter.Logs = []Log{depositLog(t, 5, first, 1, "0a", 1)} // 0x0a == 10

	l := ledger.New()
	idx := NewDepositIndexer(adapter, l, "0xvault", 1)
	idx.interval = 5 * time.Millisecond

	var a1 ledger.AccountID
	copy(a1[:], first.Bytes())

	if err := idx.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitUntil(t, "first deposit credited", func() bool { return l.Balance(a1, 1).Uint64() == 10 }, 2*time.Second)
	if err := idx.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStopped(t, idx)

	// A new confirmed event appears only after the restart.
	second := common.HexToAddress("0x4444444444444444444444444444444444444444")
	adapter.BlockNumber = 25
	adapter.Logs = append(adapter.Logs, depositLog(t, 20, second, 1, "14", 2)) // 0x14 == 20
	var a2 ledger.AccountID
	copy(a2[:], second.Bytes())

	if err := idx.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer idx.Stop()
	waitUntil(t, "post-restart deposit credited", func() bool { return l.Balance(a2, 1).Uint64() == 20 }, 2*time.Second)
}

// TestDepositIndexerStopIsIdempotent checks the two Stop edge cases: before the
// service ever started, and twice in a row.
func TestDepositIndexerStopIsIdempotent(t *testing.T) {
	idx := NewDepositIndexer(NewMockBlockchainAdapter(), ledger.New(), "0xvault", 1)

	if err := idx.Stop(); err != nil {
		t.Fatalf("stop before start: %v", err)
	}
	if err := idx.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := idx.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := idx.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// TestDepositIndexerRepeatedStartIsNoop proves a second Start while running
// does not arm a second goroutine: one tick therefore triggers exactly one
// poll.
func TestDepositIndexerRepeatedStartIsNoop(t *testing.T) {
	counter := &countingIndexerAdapter{MockBlockchainAdapter: NewMockBlockchainAdapter()}
	counter.BlockNumber = 20

	idx := NewDepositIndexer(counter, ledger.New(), "0xvault", 1)
	idx.interval = 5 * time.Millisecond

	if err := idx.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := idx.Start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := idx.run.activeLoops(); got != 1 {
		t.Fatalf("active loops after repeated Start = %d, want 1", got)
	}
	waitUntil(t, "a poll from the single goroutine", func() bool { return counter.pollCount() >= 1 }, time.Second)

	if err := idx.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStopped(t, idx)
}

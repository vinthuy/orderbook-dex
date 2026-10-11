package services

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestStateAnchorOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// latestRootIndex() -> 1
	resp := make([]byte, 32)
	resp[31] = 1
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 9
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(42), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)
	rec, err := anchor.AnchorOnce()
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if rec.RootIndex != 1 || rec.Version != l.Version() {
		t.Fatalf("record = %+v, want rootIndex 1 version %d", rec, l.Version())
	}
	if rec.Root != l.Root() {
		t.Fatal("anchored root != ledger root")
	}
	got, ok := anchor.RootIndexForVersion(rec.Version)
	if !ok || got != 1 {
		t.Fatalf("RootIndexForVersion = %d, %v", got, ok)
	}
}

func TestStateAnchorServiceCommitError(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.Error = &errSentinel{}

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	anchor := NewStateAnchorService(svc, l)
	rec, err := anchor.AnchorOnce()
	if err == nil {
		t.Fatal("AnchorOnce: expected error from adapter, got nil")
	}
	if rec != nil {
		t.Fatalf("AnchorOnce: expected nil record on error, got %+v", rec)
	}
	if len(anchor.records) != 0 {
		t.Fatalf("AnchorOnce: stored %d records on error, want 0", len(anchor.records))
	}
}

func TestStateAnchorServiceDefaultInterval(t *testing.T) {
	l := ledger.New()
	svc, _ := NewSettlementService(NewMockBlockchainAdapter(), l, "0x00000000000000000000000000000000000000aa")
	anchor := NewStateAnchorService(svc, l)
	if anchor.interval != 30*time.Second {
		t.Fatalf("default interval = %v, want 30s", anchor.interval)
	}
}

func TestStateAnchorServiceRootIndexForVersionNotFound(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	resp := make([]byte, 32)
	resp[31] = 1
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, _ := NewSettlementService(adapter, l, contract)
	anchor := NewStateAnchorService(svc, l)
	if _, err := anchor.AnchorOnce(); err != nil {
		t.Fatalf("AnchorOnce: %v", err)
	}
	// A version that was never anchored must report ok == false.
	if got, ok := anchor.RootIndexForVersion(l.Version() + 1); ok {
		t.Fatalf("RootIndexForVersion(unanchored) = %d, true; want ok=false", got)
	}
}

type errSentinel struct{}

func (e *errSentinel) Error() string { return "sentinel" }

// TestStateAnchorServiceRestartDoesNotPanic drives a full Start -> Stop ->
// Start -> Stop cycle; the buggy implementation panics on the second Stop by
// closing an already-closed stop channel.
func TestStateAnchorServiceRestartDoesNotPanic(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = latestRootIndexResponse(1)

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)
	anchor.interval = 5 * time.Millisecond

	for i := 0; i < 2; i++ {
		if err := anchor.Start(); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if err := anchor.Stop(); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		waitUntil(t, "anchor goroutine to exit", func() bool { return anchor.run.activeLoops() == 0 }, time.Second)
	}
}

// TestStateAnchorServiceRestartResumesAnchoring proves a restarted anchor keeps
// committing: the buggy implementation's restarted goroutine returns instantly
// on the stale closed channel, so no second RootRecord is ever produced.
func TestStateAnchorServiceRestartResumesAnchoring(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = latestRootIndexResponse(1)

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)
	anchor.interval = 5 * time.Millisecond

	recordCount := func() int {
		anchor.mu.Lock()
		defer anchor.mu.Unlock()
		return len(anchor.records)
	}

	if err := anchor.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitUntil(t, "first RootRecord", func() bool { return recordCount() >= 1 }, 2*time.Second)
	if err := anchor.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitUntil(t, "anchor goroutine to exit", func() bool { return anchor.run.activeLoops() == 0 }, time.Second)

	if err := anchor.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer anchor.Stop()
	waitUntil(t, "second RootRecord after restart", func() bool { return recordCount() >= 2 }, 2*time.Second)
}

// TestStateAnchorServiceStopIsIdempotent checks Stop before any Start and two
// consecutive Stop calls.
func TestStateAnchorServiceStopIsIdempotent(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	svc, err := NewSettlementService(adapter, ledger.New(), contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, ledger.New())

	if err := anchor.Stop(); err != nil {
		t.Fatalf("stop before start: %v", err)
	}
	if err := anchor.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := anchor.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := anchor.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// TestStateAnchorServiceRepeatedStartIsNoop proves a second Start while running
// does not arm a second goroutine (so one tick commits exactly once).
func TestStateAnchorServiceRepeatedStartIsNoop(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = latestRootIndexResponse(1)

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)
	anchor.interval = 5 * time.Millisecond

	if err := anchor.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := anchor.Start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := anchor.run.activeLoops(); got != 1 {
		t.Fatalf("active loops after repeated Start = %d, want 1", got)
	}
	if err := anchor.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitUntil(t, "anchor goroutine to exit", func() bool { return anchor.run.activeLoops() == 0 }, time.Second)
}

// latestRootIndexResponse builds a 32-byte latestRootIndex() return value.
func latestRootIndexResponse(n uint64) []byte {
	resp := make([]byte, 32)
	for i := 0; i < 8; i++ {
		resp[31-i] = byte(n >> (8 * i))
	}
	return resp
}

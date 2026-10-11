package services

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

// stateRootCommittedTopic0 is keccak256("StateRootCommitted(uint256,bytes32,uint64)").
var stateRootCommittedTopic0 = crypto.Keccak256Hash([]byte("StateRootCommitted(uint256,bytes32,uint64)"))

// stateRootCommittedLog builds the log commitStateRoot emits on success:
// rootIndex is indexed (Topics[1]); (root, timestamp) are the data.
func stateRootCommittedLog(contract string, rootIndex uint64, root [32]byte) *Log {
	data := make([]byte, 64)
	copy(data[0:32], root[:])
	return &Log{
		Address: common.HexToAddress(contract).Hex(),
		Topics: []string{
			stateRootCommittedTopic0.Hex(),
			common.BigToHash(new(big.Int).SetUint64(rootIndex)).Hex(),
		},
		Data: data,
	}
}

// minedCommitReceipt is a status-1 receipt carrying a StateRootCommitted event.
func minedCommitReceipt(contract string, rootIndex uint64, root [32]byte) *Receipt {
	return &Receipt{Status: 1, Logs: []*Log{stateRootCommittedLog(contract, rootIndex, root)}}
}

// TestStateAnchorRevertedCommitRecordsNothing covers requirement (a): a commit
// whose receipt has Status 0 must surface an error and must not record any
// RootRecord / byVer entry. The lagging view is set deliberately: an
// implementation that read latestRootIndex() after a reverted commit would
// happily record it.
func TestStateAnchorRevertedCommitRecordsNothing(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = latestRootIndexResponse(9)
	adapter.Receipts[adapter.SendTxHash] = &Receipt{Status: 0}

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)

	rec, err := anchor.AnchorOnce()
	if err == nil {
		t.Fatalf("AnchorOnce: expected error for a reverted commit, got record %+v", rec)
	}
	if rec != nil {
		t.Fatalf("AnchorOnce: expected nil record on error, got %+v", rec)
	}
	if len(anchor.records) != 0 {
		t.Fatalf("records = %d, want 0", len(anchor.records))
	}
	if len(anchor.byVer) != 0 {
		t.Fatalf("byVer = %d, want 0", len(anchor.byVer))
	}
}

// TestStateAnchorOnceUsesEventRootIndexWhenViewLags is the key regression test:
// latestRootIndex() still reports the PRE-commit index (5) while the mined
// receipt's StateRootCommitted event carries the real index (6). The recorded
// rootIndex must come from the decoded event, never the lagging view.
func TestStateAnchorOnceUsesEventRootIndexWhenViewLags(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = latestRootIndexResponse(5)

	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 9
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(42), Ref: "d1"})
	root := l.Root()
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 6, root)

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)

	rec, err := anchor.AnchorOnce()
	if err != nil {
		t.Fatalf("AnchorOnce: %v", err)
	}
	if rec.RootIndex != 6 {
		t.Fatalf("recorded rootIndex = %d, want 6 (from the StateRootCommitted event, not the lagging latestRootIndex view)", rec.RootIndex)
	}
	got, ok := anchor.RootIndexForVersion(rec.Version)
	if !ok || got != 6 {
		t.Fatalf("RootIndexForVersion = %d, %v, want 6, true", got, ok)
	}
}

func TestStateAnchorOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"

	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 9
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(42), Ref: "d1"})

	// (b) The mined receipt's StateRootCommitted event carries the index the
	// anchor must record.
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 1, l.Root())

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

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 1, l.Root())

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

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 1, l.Root())

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

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 1, l.Root())

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

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 1, l.Root())

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

// TestStateAnchorPendingReceiptTimesOut covers requirement (d): a receipt that
// never gets mined must fail with a timeout and record nothing.
func TestStateAnchorPendingReceiptTimesOut(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// The tx is broadcast but never mined.
	adapter.PendingReceipts[adapter.SendTxHash] = 1 << 30

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	// Tiny injected bounds keep the test fast.
	if err := svc.Initialize(Config{ReceiptTimeoutSeconds: 1, ReceiptPollMillis: 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)

	rec, err := anchor.AnchorOnce()
	if err == nil {
		t.Fatalf("AnchorOnce: expected timeout error, got record %+v", rec)
	}
	if rec != nil {
		t.Fatalf("AnchorOnce: expected nil record on timeout, got %+v", rec)
	}
	if len(anchor.records) != 0 {
		t.Fatalf("records = %d, want 0", len(anchor.records))
	}
	if len(anchor.byVer) != 0 {
		t.Fatalf("byVer = %d, want 0", len(anchor.byVer))
	}
	if adapter.ReceiptCalls < 2 {
		t.Fatalf("GetTransactionReceipt calls = %d, want >= 2 (must keep polling)", adapter.ReceiptCalls)
	}
}

// TestStateAnchorEventRootMismatchErrors covers requirement (e): the event's
// decoded root must equal the submitted root, otherwise the receipt belongs to
// some other commit and must not be recorded.
func TestStateAnchorEventRootMismatchErrors(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	var otherRoot [32]byte
	otherRoot[31] = 0xff
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 1, otherRoot)

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	anchor := NewStateAnchorService(svc, l)

	rec, err := anchor.AnchorOnce()
	if err == nil {
		t.Fatalf("AnchorOnce: expected error for mismatched event root, got record %+v", rec)
	}
	if rec != nil {
		t.Fatalf("AnchorOnce: expected nil record on error, got %+v", rec)
	}
	if len(anchor.records) != 0 {
		t.Fatalf("records = %d, want 0", len(anchor.records))
	}
}

// latestRootIndexResponse builds a 32-byte latestRootIndex() return value.
func latestRootIndexResponse(n uint64) []byte {
	resp := make([]byte, 32)
	for i := 0; i < 8; i++ {
		resp[31-i] = byte(n >> (8 * i))
	}
	return resp
}

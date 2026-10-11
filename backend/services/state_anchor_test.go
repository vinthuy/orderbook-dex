package services

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestStateAnchorServiceAnchorOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// latestRootIndex() returns uint256 5
	resp := make([]byte, 32)
	resp[31] = 5
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(100), Ref: "dep:1"})
	version := l.Version()

	svc, err := NewSettlementService(adapter, l, contract)
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	anchor := NewStateAnchorService(l, svc, 30*time.Second)
	idx, err := anchor.AnchorOnce()
	if err != nil {
		t.Fatalf("AnchorOnce: %v", err)
	}
	if idx != 5 {
		t.Fatalf("rootIndex = %d, want 5", idx)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("sent %d txs, want 1", len(adapter.Sent))
	}
	if got, ok := anchor.VersionForRoot(5); !ok || got != version {
		t.Fatalf("version for root 5 = %d (ok=%v), want %d", got, ok, version)
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

	anchor := NewStateAnchorService(l, svc, time.Second)
	if _, err := anchor.AnchorOnce(); err == nil {
		t.Fatal("AnchorOnce: expected error from adapter, got nil")
	}
}

func TestStateAnchorServiceDefaultInterval(t *testing.T) {
	l := ledger.New()
	svc, _ := NewSettlementService(NewMockBlockchainAdapter(), l, "0x00000000000000000000000000000000000000aa")
	anchor := NewStateAnchorService(l, svc, 0)
	if anchor.interval != 30*time.Second {
		t.Fatalf("default interval = %v, want 30s", anchor.interval)
	}
}

type errSentinel struct{}

func (e *errSentinel) Error() string { return "sentinel" }

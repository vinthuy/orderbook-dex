package services

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestSettlementServiceCommitRootEncodesCalldata(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	svc, err := NewSettlementService(adapter, ledger.New(), "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var root [32]byte
	root[31] = 7
	if _, err := svc.CommitRoot(root); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx, got %d", len(adapter.Sent))
	}
	data := adapter.Sent[0].Data
	if len(data) != 4+32 {
		t.Fatalf("calldata len = %d, want 36", len(data))
	}
	if data[35] != 7 {
		t.Fatalf("root not encoded into calldata")
	}
}

func TestSettlementServiceLatestRootIndex(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// latestRootIndex() returns uint256 3
	resp := make([]byte, 32)
	resp[31] = 3
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	svc, _ := NewSettlementService(adapter, ledger.New(), contract)
	got, err := svc.LatestRootIndex()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got != 3 {
		t.Fatalf("rootIndex = %d, want 3", got)
	}
}

func TestSettlementServiceWithdrawBuildsProofCalldata(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, _ := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(100), 1); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx")
	}
	// selector for withdraw(address,uint32,uint256,uint256,uint256,bytes32[])
	data := adapter.Sent[0].Data
	if len(data) < 4 {
		t.Fatalf("no selector")
	}
	// account address must appear in the ABI-encoded args
	if !bytes.Contains(data, acc[:]) {
		t.Fatalf("account not encoded in calldata")
	}
	if new(big.Int).SetBytes(data[4:36]).Sign() == 0 {
		t.Fatalf("account word should be non-zero")
	}
}

// TestSettlementServiceWithdrawEncodesAccountWord strengthens the brief's
// withdraw test: with the brief's degenerate values (account == 0x..01 and
// assetId == 1) the two leading ABI words are byte-identical, so
// bytes.Contains(data, acc[:]) cannot tell "account encoded in slot 0" from
// "assetId encoded in slot 0". Using distinctive account bytes makes the
// assertion non-vacuous: the account word must land exactly in the first slot.
func TestSettlementServiceWithdrawEncodesAccountWord(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[0] = 0x11
	acc[19] = 0x22
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(100), 1); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx")
	}
	data := adapter.Sent[0].Data
	// selector + 6 static args (account, assetId, balance, amount, rootIndex,
	// proof offset) + the empty proof's length word.
	if want := 4 + 6*32 + 32; len(data) != want {
		t.Fatalf("calldata len = %d, want %d", len(data), want)
	}
	wantAccount := make([]byte, 32)
	copy(wantAccount[12:], acc[:])
	if !bytes.Equal(data[4:36], wantAccount) {
		t.Fatalf("account word = %x, want %x", data[4:36], wantAccount)
	}
	wantAsset := make([]byte, 32)
	wantAsset[31] = 1
	if !bytes.Equal(data[36:68], wantAsset) {
		t.Fatalf("assetId word = %x, want %x", data[36:68], wantAsset)
	}
}

// TestSettlementServiceWithdrawGuardBlocksMismatchedBalance verifies the
// load-bearing guard: when the ledger balance differs from the balance being
// published, Withdraw must refuse before any transaction is submitted, with the
// brief's verbatim error message.
func TestSettlementServiceWithdrawGuardBlocksMismatchedBalance(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Ledger says 100 but the caller publishes 99: must be rejected.
	_, err = svc.Withdraw(acc, 1, uint256.NewInt(99), uint256.NewInt(99), 1)
	if err == nil {
		t.Fatalf("expected balance mismatch error")
	}
	if want := "withdraw: balance mismatch (ledger=100 arg=99)"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if len(adapter.Sent) != 0 {
		t.Fatalf("guard must not submit a tx, got %d", len(adapter.Sent))
	}
}

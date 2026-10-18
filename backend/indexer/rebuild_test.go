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

	rebuilt, err := Rebuild(adapter, "0xvault", 1, 1)
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

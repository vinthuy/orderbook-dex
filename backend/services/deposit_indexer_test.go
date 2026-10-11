package services

import (
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"web3-amm-dex-backend/ledger"
)

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

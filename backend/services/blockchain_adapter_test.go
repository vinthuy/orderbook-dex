package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBlockchainAdapter_GetBlockNumber(t *testing.T) {
	adapter := NewMockBlockchainAdapter()

	blockNumber, err := adapter.GetBlockNumber()

	assert.NoError(t, err)
	assert.Greater(t, blockNumber, uint64(0))
}

func TestBlockchainAdapter_CallContract(t *testing.T) {
	adapter := NewMockBlockchainAdapter()

	result, err := adapter.CallContract(
		"0x1234567890123456789012345678901234567890",
		[]byte{0x01, 0x02, 0x03},
		100,
	)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestBlockchainAdapter_GetLogs(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.Logs = []Log{
		{Address: "0xvault", BlockNumber: 5, TxHash: "0xaaa", LogIndex: 0},
		{Address: "0xvault", BlockNumber: 10, TxHash: "0xbbb", LogIndex: 1},
	}
	logs, err := adapter.GetLogs(1, 7, "0xvault", nil)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].BlockNumber != 5 {
		t.Fatalf("got %d logs, want exactly the block-5 one", len(logs))
	}
}

func TestBlockchainAdapter_RecordsSent(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	tx := &Transaction{To: "0xvault", Data: []byte{0x01}, GasLimit: 100000}
	if _, err := adapter.SendTransaction(tx); err != nil {
		t.Fatalf("SendTransaction: %v", err)
	}
	if len(adapter.Sent) != 1 || adapter.Sent[0] != tx {
		t.Fatalf("Sent not recorded")
	}
}

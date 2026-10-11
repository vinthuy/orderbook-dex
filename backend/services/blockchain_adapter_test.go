package services

import (
	"bytes"
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

// TestBlockchainAdapter_CallHandlerBySelector proves CallHandler can distinguish
// two methods on the same contract address, which the address-keyed
// ContractResponses map cannot.
func TestBlockchainAdapter_CallHandlerBySelector(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.ContractResponses["0xabc"] = []byte{0xaa} // must be ignored
	adapter.CallHandler = func(contract string, data []byte) ([]byte, error) {
		if contract != "0xabc" {
			t.Fatalf("contract = %q, want 0xabc", contract)
		}
		if len(data) < 4 {
			t.Fatalf("data too short: %d", len(data))
		}
		switch data[0] {
		case 0x01:
			return []byte{0x11}, nil
		case 0x02:
			return []byte{0x22}, nil
		}
		return nil, nil
	}

	got1, err := adapter.CallContract("0xabc", []byte{0x01, 0, 0, 0}, 0)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	got2, err := adapter.CallContract("0xabc", []byte{0x02, 0, 0, 0}, 0)
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if !bytes.Equal(got1, []byte{0x11}) || !bytes.Equal(got2, []byte{0x22}) {
		t.Fatalf("CallHandler not used per method: %x %x", got1, got2)
	}
}

// TestBlockchainAdapter_CallContractFallsBackToContractResponses keeps the
// pre-existing address-keyed behaviour working when CallHandler is nil.
func TestBlockchainAdapter_CallContractFallsBackToContractResponses(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.ContractResponses["0xabc"] = []byte{0xaa}
	got, err := adapter.CallContract("0xabc", []byte{0x01}, 0)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !bytes.Equal(got, []byte{0xaa}) {
		t.Fatalf("got %x, want aa (ContractResponses fallback)", got)
	}
}

// TestBlockchainAdapter_PendingReceiptProgression proves a tx can report
// pending ((nil, nil)) a configured number of times before it is mined.
func TestBlockchainAdapter_PendingReceiptProgression(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.PendingReceipts["0xtx"] = 2
	adapter.Receipts["0xtx"] = &Receipt{Status: 1, TxHash: "0xtx"}

	for i := 0; i < 2; i++ {
		r, err := adapter.GetTransactionReceipt("0xtx")
		if err != nil || r != nil {
			t.Fatalf("call %d: got (%v, %v), want pending (nil, nil)", i, r, err)
		}
	}
	r, err := adapter.GetTransactionReceipt("0xtx")
	if err != nil {
		t.Fatalf("mined call: %v", err)
	}
	if r == nil {
		t.Fatalf("mined call: got nil, want the receipt")
	}
	if adapter.ReceiptCalls != 3 {
		t.Fatalf("ReceiptCalls = %d, want 3", adapter.ReceiptCalls)
	}
}

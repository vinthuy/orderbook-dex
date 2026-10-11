package services

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// testTxHash is a syntactically valid 32-byte hash used by the JSON-RPC tests.
var testTxHash = "0x" + strings.Repeat("ab", 32)

// newEVMAdapterForRPC points a REAL EVMAdapter at a throwaway httptest JSON-RPC
// server. No live node is involved: NewEVMAdapter only dials the URL lazily. The
// operator key is generated per call so no real private key ever appears in the
// tests (crypto.HexToECDSA only needs it to be syntactically valid).
func newEVMAdapterForRPC(t *testing.T, handler http.HandlerFunc) *EVMAdapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	adapter, err := NewEVMAdapter(srv.URL, "0x"+hex.EncodeToString(crypto.FromECDSA(key)), 1)
	if err != nil {
		t.Fatalf("new evm adapter: %v", err)
	}
	return adapter
}

// jsonRPCHandler answers eth_getTransactionReceipt with a raw JSON `result`
// payload. Pass "null" for an unknown/pending tx, exactly what a node returns.
func jsonRPCHandler(t *testing.T, result string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Method != "eth_getTransactionReceipt" {
			t.Errorf("unexpected JSON-RPC method %q", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}
}

// minedReceiptResultJSON is a node-shaped receipt with status 1, blockNumber 42
// and a single log whose data is 0xdeadbeef.
func minedReceiptResultJSON(t *testing.T) (result string, logAddr string) {
	t.Helper()
	logAddr = "0x00000000000000000000000000000000000000aa"
	blockHash := "0x" + strings.Repeat("11", 32)
	result = fmt.Sprintf(`{
  "transactionHash": %q,
  "transactionIndex": "0x2",
  "blockHash": %q,
  "blockNumber": "0x2a",
  "from": "0x00000000000000000000000000000000000000bb",
  "to": %q,
  "cumulativeGasUsed": "0x5208",
  "gasUsed": "0x5208",
  "contractAddress": null,
  "logs": [{
    "address": %q,
    "topics": [%q, %q],
    "data": "0xdeadbeef",
    "blockNumber": "0x2a",
    "transactionHash": %q,
    "transactionIndex": "0x2",
    "blockHash": %q,
    "logIndex": "0x0",
    "removed": false
  }],
  "logsBloom": "0x%s",
  "status": "0x1",
  "effectiveGasPrice": "0x1",
  "type": "0x0"
}`,
		testTxHash,
		blockHash,
		logAddr,
		logAddr,
		stateRootCommittedTopic0.Hex(),
		common.BigToHash(big.NewInt(7)).Hex(),
		testTxHash,
		blockHash,
		strings.Repeat("0", 512),
	)
	return result, logAddr
}

// TestEVMAdapterGetTransactionReceiptNotFoundIsPending is the regression test
// for the HIGH finding: a real node answers an unknown (still-mempool) tx with
// result:null, which ethclient turns into ethereum.NotFound. The adapter must
// normalise that to (nil, nil) so the caller can keep polling instead of
// aborting on the first poll.
func TestEVMAdapterGetTransactionReceiptNotFoundIsPending(t *testing.T) {
	adapter := newEVMAdapterForRPC(t, jsonRPCHandler(t, "null"))

	receipt, err := adapter.GetTransactionReceipt(testTxHash)
	if err != nil {
		t.Fatalf("unknown tx must map to (nil, nil), got error: %v", err)
	}
	if receipt != nil {
		t.Fatalf("receipt = %+v, want nil", receipt)
	}
}

// TestEVMAdapterGetTransactionReceiptMapsMinedReceipt proves the not-found
// special case does not swallow real data: a mined receipt is mapped field for
// field.
func TestEVMAdapterGetTransactionReceiptMapsMinedReceipt(t *testing.T) {
	result, logAddr := minedReceiptResultJSON(t)
	adapter := newEVMAdapterForRPC(t, jsonRPCHandler(t, result))

	receipt, err := adapter.GetTransactionReceipt(testTxHash)
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if receipt == nil {
		t.Fatal("receipt = nil, want the mined receipt")
	}
	if receipt.Status != 1 {
		t.Fatalf("Status = %d, want 1", receipt.Status)
	}
	if receipt.BlockNumber != 42 {
		t.Fatalf("BlockNumber = %d, want 42", receipt.BlockNumber)
	}
	if want := "0x" + strings.Repeat("11", 32); receipt.BlockHash != want {
		t.Fatalf("BlockHash = %q, want %q", receipt.BlockHash, want)
	}
	if receipt.TxHash != testTxHash {
		t.Fatalf("TxHash = %q, want %q", receipt.TxHash, testTxHash)
	}
	if receipt.GasUsed != 0x5208 {
		t.Fatalf("GasUsed = %d, want 0x5208", receipt.GasUsed)
	}
	if len(receipt.Logs) != 1 {
		t.Fatalf("len(Logs) = %d, want 1", len(receipt.Logs))
	}
	lg := receipt.Logs[0]
	// common.Address.Hex() applies EIP-55 checksumming, so compare against that.
	if want := common.HexToAddress(logAddr).Hex(); lg.Address != want {
		t.Fatalf("log address = %q, want %q", lg.Address, want)
	}
	if len(lg.Topics) != 2 || lg.Topics[0] != stateRootCommittedTopic0.Hex() {
		t.Fatalf("log topics = %v, want topic0 %s", lg.Topics, stateRootCommittedTopic0.Hex())
	}
	if !bytes.Equal(lg.Data, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("log data = %x, want deadbeef", lg.Data)
	}
	if lg.TxHash != testTxHash {
		t.Fatalf("log TxHash = %q, want %q", lg.TxHash, testTxHash)
	}
}

// TestEVMAdapterGetTransactionReceiptPropagatesRPCError guards the other half of
// the contract: only NotFound is normalised. A genuine RPC failure must surface.
func TestEVMAdapterGetTransactionReceiptPropagatesRPCError(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"boom"}}`, req.ID)
	}
	adapter := newEVMAdapterForRPC(t, handler)

	_, err := adapter.GetTransactionReceipt(testTxHash)
	if err == nil {
		t.Fatal("expected an error for a genuine RPC failure")
	}
	if errors.Is(err, ethereum.NotFound) {
		t.Fatalf("genuine RPC error must not be reported as NotFound: %v", err)
	}
}

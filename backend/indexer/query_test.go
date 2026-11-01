package indexer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

func TestQueryServerTrades(t *testing.T) {
	s := mustOpenStore(t)
	_ = s.PutTrade(TradeRecord{TradeID: 1, Market: 1, Price: 5, Qty: "1", Notional: "5", Maker: "0xaa", Taker: "0xbb", MakerSide: "sell"})

	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 20

	qs := &QueryServer{Store: s, Adapter: adapter, Chain: "0xvault"}
	r := gin.New()
	qs.Register(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/trades", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var trades []TradeRecord
	if err := json.Unmarshal(w.Body.Bytes(), &trades); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(trades))
	}
}

func TestQueryServerReconcile(t *testing.T) {
	s := mustOpenStore(t)
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 30
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 5, common.HexToAddress("0x1111111111111111111111111111111111111111"), 1, 100, 1),
	}

	qs := &QueryServer{Store: s, Adapter: adapter, Chain: "0xvault", Running: ledger.New(), Confirmations: 1}
	r := gin.New()
	qs.Register(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/reconcile", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["assets"]; !ok {
		t.Fatalf("body missing assets: %v", body)
	}
}

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ob, _ := testOrderBook(t)
	g, err := New(ob, nil, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func doJSON(t *testing.T, g *Gateway, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	g.Register(r)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListMarkets(t *testing.T) {
	g := newTestGateway(t)
	w := doJSON(t, g, http.MethodGet, "/v1/markets", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var markets []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &markets); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(markets) != 1 || markets[0]["symbol"] != "ETH/USDT" {
		t.Fatalf("markets = %v", markets)
	}
}

func TestBookDepth(t *testing.T) {
	g := newTestGateway(t)
	w := doJSON(t, g, http.MethodGet, "/v1/book/1?depth=5", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var v depthView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Market != 1 {
		t.Fatalf("market = %d, want 1", v.Market)
	}
}

func TestPlaceOrderRejectsBadSignature(t *testing.T) {
	g := newTestGateway(t)
	body := `{"order":{"market":1,"side":0,"type":0,"tif":0,"price":1,"qty":"1","maxNotional":"0","nonce":1,"expiry":9999999999},"signature":"0x00"}`
	w := doJSON(t, g, http.MethodPost, "/v1/orders", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401, body = %s", w.Code, w.Body.String())
	}
}

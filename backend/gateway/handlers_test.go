package gateway

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ob, _, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{})
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

func TestPlaceOrderSignedLimitBuy(t *testing.T) {
	ob, l, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	v := NewVerifier(13700, "", "")
	o := SignedOrder{
		Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1,
		Qty: "1", MaxNotional: "", Nonce: 1,
		Expiry: uint64(time.Now().Add(time.Hour).Unix()),
	}
	signer, sig := signOrder(t, v, o)
	account := ledger.AccountID(signer)
	if err := l.Apply(ledger.DepositOp{Account: account, Asset: 1, Amount: uint256.NewInt(1000), Ref: "dep1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	body := fmt.Sprintf(`{"order":{"market":1,"side":0,"type":0,"tif":0,"price":1,"qty":"1","maxNotional":"","nonce":1,"expiry":%d},"signature":"0x%s"}`, o.Expiry, hex.EncodeToString(sig))
	w := doJSON(t, g, http.MethodPost, "/v1/orders", body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200, body = %s", w.Code, w.Body.String())
	}
}

func TestPlaceOrderMalformedQtyReturns400(t *testing.T) {
	g := newTestGateway(t)
	body := `{"order":{"market":1,"side":0,"type":0,"tif":0,"price":1,"qty":"abc","maxNotional":"0","nonce":1,"expiry":9999999999},"signature":"0x00"}`
	w := doJSON(t, g, http.MethodPost, "/v1/orders", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

// signedOrderBody builds the JSON wire body for a signed order.
func signedOrderBody(o SignedOrder, sig []byte) string {
	return fmt.Sprintf(`{"order":{"market":%d,"side":%d,"type":%d,"tif":%d,"price":%d,"qty":"%s","maxNotional":"%s","nonce":%d,"expiry":%d},"signature":"0x%s"}`,
		o.Market, o.Side, o.Type, o.TIF, o.Price, o.Qty, o.MaxNotional, o.Nonce, o.Expiry, hex.EncodeToString(sig))
}

func TestNewRejectsNilBus(t *testing.T) {
	ob, _, _ := testOrderBook(t)
	if _, err := New(ob, nil, Config{}); err == nil {
		t.Fatal("New with nil bus should error")
	}
}

func TestPlaceOrderNonceReusedReturns409(t *testing.T) {
	ob, l, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	v := NewVerifier(13700, "", "")
	expiry := uint64(time.Now().Add(time.Hour).Unix())
	o1 := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "1", MaxNotional: "", Nonce: 1, Expiry: expiry}
	o2 := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "2", MaxNotional: "", Nonce: 1, Expiry: expiry}
	signer, sigs := signOrders(t, v, o1, o2)
	account := ledger.AccountID(signer)
	if err := l.Apply(ledger.DepositOp{Account: account, Asset: 1, Amount: uint256.NewInt(1000), Ref: "dep1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	_ = doJSON(t, g, http.MethodPost, "/v1/orders", signedOrderBody(o1, sigs[0]))
	w2 := doJSON(t, g, http.MethodPost, "/v1/orders", signedOrderBody(o2, sigs[1]))
	if w2.Code != http.StatusConflict {
		t.Fatalf("second order code = %d, want 409, body = %s", w2.Code, w2.Body.String())
	}
}

func TestPlaceOrderRateLimitedReturns429(t *testing.T) {
	ob, l, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{Rate: 0.001, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	v := NewVerifier(13700, "", "")
	expiry := uint64(time.Now().Add(time.Hour).Unix())
	o1 := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "1", MaxNotional: "", Nonce: 1, Expiry: expiry}
	o2 := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "1", MaxNotional: "", Nonce: 2, Expiry: expiry}
	signer, sigs := signOrders(t, v, o1, o2)
	account := ledger.AccountID(signer)
	if err := l.Apply(ledger.DepositOp{Account: account, Asset: 1, Amount: uint256.NewInt(1000), Ref: "dep1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	_ = doJSON(t, g, http.MethodPost, "/v1/orders", signedOrderBody(o1, sigs[0]))
	w2 := doJSON(t, g, http.MethodPost, "/v1/orders", signedOrderBody(o2, sigs[1]))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second order code = %d, want 429, body = %s", w2.Code, w2.Body.String())
	}
}

func TestPlaceOrderInsufficientBalanceReturns422(t *testing.T) {
	ob, _, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	v := NewVerifier(13700, "", "")
	o := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "1", MaxNotional: "", Nonce: 1, Expiry: uint64(time.Now().Add(time.Hour).Unix())}
	_, sig := signOrder(t, v, o)
	// No deposit: the buy must fail with insufficient balance.

	w := doJSON(t, g, http.MethodPost, "/v1/orders", signedOrderBody(o, sig))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "insufficient_balance" {
		t.Fatalf("error = %v, want insufficient_balance", resp["error"])
	}
}

func TestCancelOrderNotOwnerReturns403(t *testing.T) {
	ob, l, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	v := NewVerifier(13700, "", "")
	expiry := uint64(time.Now().Add(time.Hour).Unix())

	// Account A places a resting limit buy.
	o := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "1", MaxNotional: "", Nonce: 1, Expiry: expiry}
	signerA, sigA := signOrder(t, v, o)
	accountA := ledger.AccountID(signerA)
	if err := l.Apply(ledger.DepositOp{Account: accountA, Asset: 1, Amount: uint256.NewInt(1000), Ref: "depA"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	w1 := doJSON(t, g, http.MethodPost, "/v1/orders", signedOrderBody(o, sigA))
	if w1.Code != http.StatusOK {
		t.Fatalf("place code = %d, body = %s", w1.Code, w1.Body.String())
	}
	var placed struct {
		Order struct {
			ID uint64 `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &placed); err != nil {
		t.Fatalf("unmarshal place response: %v", err)
	}
	if placed.Order.ID == 0 {
		t.Fatal("placed order id was 0")
	}

	// Account B (different key) tries to cancel A's order.
	c := SignedCancel{Market: 1, OrderID: placed.Order.ID, Nonce: 1, Expiry: expiry}
	_, sigB := signCancel(t, v, c)
	body := fmt.Sprintf(`{"cancel":{"market":1,"orderId":%d,"nonce":1,"expiry":%d},"signature":"0x%s"}`, placed.Order.ID, expiry, hex.EncodeToString(sigB))
	w2 := doJSON(t, g, http.MethodDelete, fmt.Sprintf("/v1/orders/%d", placed.Order.ID), body)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("cancel code = %d, want 403, body = %s", w2.Code, w2.Body.String())
	}
}

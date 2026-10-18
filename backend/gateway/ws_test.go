package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

func mkMarket(id matching.MarketID) *matching.MarketID { return &id }
func mkAcct(n byte) *ledger.AccountID                  { a := acct(n); return &a }

func TestSubscriptionMatches(t *testing.T) {
	m := mkMarket(1)
	a := mkAcct(1)

	cases := []struct {
		name    string
		sub     subscription
		market  *matching.MarketID
		account *ledger.AccountID
		want    bool
	}{
		{"all", subscription{}, mkMarket(1), mkAcct(1), true},
		{"market match", subscription{market: m}, mkMarket(1), mkAcct(9), true},
		{"market mismatch", subscription{market: m}, mkMarket(2), mkAcct(1), false},
		{"account match", subscription{account: a}, mkMarket(9), mkAcct(1), true},
		{"account mismatch", subscription{account: a}, mkMarket(1), mkAcct(2), false},
		{"market+account match", subscription{market: m, account: a}, mkMarket(1), mkAcct(1), true},
		{"market+account account mismatch", subscription{market: m, account: a}, mkMarket(1), mkAcct(2), false},
	}
	for _, c := range cases {
		if got := c.sub.matches(c.market, c.account); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWSPushesOrderToSubscriber(t *testing.T) {
	ob, _, bus := testOrderBook(t)
	g, err := New(ob, bus, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	srv := httptest.NewServer(g.Router())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]interface{}{"op": "subscribe"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Give the server's reader loop time to register the subscription before
	// publishing, so the frame is not lost to a torn subscription.
	time.Sleep(100 * time.Millisecond)

	// Publish an order event directly to the same bus, verifying the hub forwards it.
	var a ledger.AccountID
	a[19] = 1
	o := &matching.Order{
		ID: 1, Seq: 1, Market: 1, Account: a, Side: matching.SideBuy,
		Type: matching.TypeLimit, TIF: matching.TIFGTC, Price: 1,
		Qty: uint256.NewInt(1), Filled: uint256.NewInt(0), Status: matching.StatusNew,
	}
	_ = bus.Publish(services.Event{
		Topic: services.TopicOrderBookOrder,
		Data:  services.OrderEvent{Order: o, Status: matching.StatusNew},
	})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(msg), `"order"`) {
		t.Fatalf("expected an order frame, got %s", msg)
	}
}

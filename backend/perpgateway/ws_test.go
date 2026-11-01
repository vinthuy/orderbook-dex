package perpgateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
	"web3-amm-dex-backend/services"
)

// ledgerAccountHex builds a synthetic account from one byte, for tests that need
// a second party.
func ledgerAccountHex(b byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = b
	return a
}

// servicesPriceOptions returns zero-valued price freshness options (defaults).
func servicesPriceOptions() services.PriceOptions { return services.PriceOptions{} }

// dialPerp opens a WebSocket client against the gateway's /ws/perp route.
func dialPerp(t *testing.T, f *fixture) *websocketConn {
	t.Helper()
	srv := httptest.NewServer(f.router)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/perp"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &websocketConn{c: c}
}

type websocketConn struct {
	c interface {
		WriteJSON(v any) error
		ReadJSON(v any) error
		SetReadDeadline(t time.Time) error
	}
}

func (w *websocketConn) subscribe(t *testing.T, market uint32) {
	t.Helper()
	if err := w.c.WriteJSON(map[string]any{"op": "subscribe", "market": market}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// readFrame reads one frame with a deadline so a broken test fails fast.
func (w *websocketConn) readFrame(t *testing.T) map[string]any {
	t.Helper()
	_ = w.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var out map[string]any
	if err := w.c.ReadJSON(&out); err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

func TestWSSubscribePushesInitialDepth(t *testing.T) {
	f := newFixture(t)
	ws := dialPerp(t, f)
	ws.subscribe(t, 1)

	fr := ws.readFrame(t)
	if fr["type"] != "depth" {
		t.Fatalf("want an initial depth frame, got %v", fr)
	}
	data, ok := fr["data"].(map[string]any)
	if !ok {
		t.Fatalf("bad depth payload: %v", fr["data"])
	}
	if uint32(data["market"].(float64)) != 1 {
		t.Fatalf("wrong market in depth: %v", data)
	}
}

func TestWSPushesTradeOnMatch(t *testing.T) {
	f := newFixture(t)
	ws := dialPerp(t, f)
	ws.subscribe(t, 1)
	_ = ws.readFrame(t) // initial depth

	// Two accounts: the fixture's own key rests a short, and a second funded key
	// takes it.
	if _, err := f.svc.Place(perp.PlaceRequest{Market: 1, Account: f.account, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("place short: %v", err)
	}
	taker := ledgerAccountHex(0xAB)
	if err := f.ledger.Apply(ledger.DepositOp{Account: taker, Asset: 1,
		Amount: uint256.NewInt(1_000_000), Ref: "taker-fund"}); err != nil {
		t.Fatalf("fund taker: %v", err)
	}
	if _, err := f.svc.Place(perp.PlaceRequest{Market: 1, Account: taker, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("place long: %v", err)
	}

	// The hub pushes trade then depth; read until a trade arrives.
	for i := 0; i < 5; i++ {
		fr := ws.readFrame(t)
		if fr["type"] == "trade" {
			return
		}
	}
	t.Fatal("no trade frame received")
}

func TestWSUnsubscribeStopsFrames(t *testing.T) {
	f := newFixture(t)
	ws := dialPerp(t, f)
	ws.subscribe(t, 1)
	_ = ws.readFrame(t) // initial depth

	if err := ws.c.WriteJSON(map[string]any{"op": "unsubscribe"}); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	// Give the read loop a moment to process the message before publishing.
	time.Sleep(50 * time.Millisecond)

	if _, err := f.svc.Place(perp.PlaceRequest{Market: 1, Account: f.account, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("place: %v", err)
	}

	_ = ws.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var out map[string]any
	if err := ws.c.ReadJSON(&out); err == nil {
		t.Fatalf("received a frame after unsubscribe: %v", out)
	}
}

func TestWSAccountFilter(t *testing.T) {
	f := newFixture(t)
	ws := dialPerp(t, f)
	if err := ws.c.WriteJSON(map[string]any{"op": "subscribe", "account": ledgerAccountHex(0xCD).Hex()}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// An order for a different account must not reach this client.
	if _, err := f.svc.Place(perp.PlaceRequest{Market: 1, Account: f.account, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatalf("place: %v", err)
	}

	_ = ws.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var out map[string]any
	if err := ws.c.ReadJSON(&out); err == nil {
		t.Fatalf("account-filtered client received a foreign frame: %v", out)
	}
}

func TestWSMarkFrame(t *testing.T) {
	f := newFixture(t)
	ws := dialPerp(t, f)
	ws.subscribe(t, 1)
	_ = ws.readFrame(t) // initial depth

	src := &movablePrice{price: 100}
	f.svc.SetPriceSource(src, servicesPriceOptions())
	f.svc.TickForTest(time.Unix(1000, 0))

	// The first tick produces a mark frame.
	for i := 0; i < 5; i++ {
		fr := ws.readFrame(t)
		if fr["type"] == "mark" {
			return
		}
	}
	t.Fatal("no mark frame received")
}

// movablePrice is a PriceSource whose value the test controls.
type movablePrice struct{ price perp.Price }

func (m *movablePrice) Index(perp.MarketID) (perp.Price, bool) { return m.price, m.price != 0 }

var _ = json.Marshal


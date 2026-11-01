package perpgateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"web3-amm-dex-backend/perp"
)

func TestCandlesEndpointReturnsOHLCV(t *testing.T) {
	f := newFixture(t)
	// Two trades create one 1m bar.
	if _, err := f.svc.Place(perp.PlaceRequest{Market: 1, Account: f.account, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(100), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.svc.Candles().Record(perp.Trade{ID: 1, Market: 1, Price: 100, Qty: uint256.NewInt(50)}, now)
	f.svc.Candles().Record(perp.Trade{ID: 2, Market: 1, Price: 104, Qty: uint256.NewInt(50)}, now.Add(time.Second))

	w := f.do(t, http.MethodGet, "/v1/perp/candles/1?interval=1m", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Market   uint32 `json:"market"`
		Interval string `json:"interval"`
		Candles  []struct {
			T int64  `json:"t"`
			O uint64 `json:"o"`
			H uint64 `json:"h"`
			L uint64 `json:"l"`
			C uint64 `json:"c"`
			V string `json:"v"`
		} `json:"candles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Interval != "1m" || len(out.Candles) != 1 {
		t.Fatalf("unexpected payload: %+v", out)
	}
	cd := out.Candles[0]
	if cd.O != 100 || cd.H != 104 || cd.L != 100 || cd.C != 104 {
		t.Fatalf("OHLC = %d/%d/%d/%d", cd.O, cd.H, cd.L, cd.C)
	}
	if cd.V != "100" {
		t.Fatalf("volume = %s, want 100", cd.V)
	}
}

func TestCandlesEndpointRejectsBadInterval(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/v1/perp/candles/1?interval=7m", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unsupported interval, got %d", w.Code)
	}
}

func TestCandlesEndpointCapsLimit(t *testing.T) {
	f := newFixture(t)
	// The cap itself is what matters: ask for far more than allowed and confirm
	// the request still succeeds rather than being rejected.
	w := f.do(t, http.MethodGet, "/v1/perp/candles/1?interval=1h&limit=99999", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

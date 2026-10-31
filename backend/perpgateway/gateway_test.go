package perpgateway

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gin-gonic/gin"
	"github.com/holiman/uint256"

	spotgateway "web3-amm-dex-backend/gateway"
	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
	"web3-amm-dex-backend/services"
)

type fixture struct {
	gw      *Gateway
	svc     *services.PerpOrderBookService
	ledger  *ledger.Ledger
	router  http.Handler
	key     *ecdsa.PrivateKey
	account ledger.AccountID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
	}}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	account := ledger.AccountID(crypto.PubkeyToAddress(key.PublicKey))
	if err := l.Apply(ledger.DepositOp{Account: account, Asset: 1,
		Amount: uint256.NewInt(1_000_000), Ref: "d1"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	bus := services.NewEventBus()
	svc, err := services.NewPerpOrderBookService(markets, l, holds, mustJournal(t), bus)
	if err != nil {
		t.Fatalf("NewPerpOrderBookService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	// The gateway must share the SAME bus the service publishes to, or the
	// WebSocket hub would never see an event.
	gw, err := New(svc, bus, Config{ChainID: 13700, Rate: 0, Burst: 0})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	return &fixture{gw: gw, svc: svc, ledger: l, router: gw.Router(), key: key, account: account}
}

func mustJournal(t *testing.T) *services.PerpJournal {
	t.Helper()
	j, err := services.NewPerpJournal("")
	if err != nil {
		t.Fatalf("NewPerpJournal: %v", err)
	}
	return j
}

// sign produces a 65-byte signature over hash with the fixture's key.
func (f *fixture) sign(t *testing.T, hash []byte) hexutil.Bytes {
	t.Helper()
	sig, err := crypto.Sign(hash, f.key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return hexutil.Bytes(sig)
}

// signedOrder builds a signed envelope for this fixture's account.
func (f *fixture) signedOrder(t *testing.T, o SignedOrder) SignedOrderEnvelope {
	t.Helper()
	hash, err := f.gw.verifier.hashOrder(o)
	if err != nil {
		t.Fatalf("hashOrder: %v", err)
	}
	return SignedOrderEnvelope{Order: o, Signature: f.sign(t, hash)}
}

func (f *fixture) do(t *testing.T, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func (f *fixture) order() SignedOrder {
	return SignedOrder{
		Market: 1, Side: uint8(perp.SideLong), Type: uint8(perp.TypeLimit),
		TIF: uint8(perp.TIFGTC), Price: 100, Qty: "100",
		Leverage: 10, Nonce: 1, Expiry: uint64(unixNow()) + 600,
	}
}

func TestPlaceOrderSucceeds(t *testing.T) {
	f := newFixture(t)
	body, _ := json.Marshal(f.signedOrder(t, f.order()))
	w := f.do(t, http.MethodPost, "/v1/perp/orders", body)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if _, ok := f.svc.Engine().Position(1, f.account); ok {
		t.Fatal("a resting order must not create a position")
	}
}

func TestPlaceOrderRejectsExpired(t *testing.T) {
	f := newFixture(t)
	o := f.order()
	o.Expiry = uint64(unixNow()) - 1
	body, _ := json.Marshal(f.signedOrder(t, o))
	w := f.do(t, http.MethodPost, "/v1/perp/orders", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestPlaceOrderRejectsReusedNonce(t *testing.T) {
	f := newFixture(t)
	body, _ := json.Marshal(f.signedOrder(t, f.order()))
	if w := f.do(t, http.MethodPost, "/v1/perp/orders", body); w.Code != http.StatusOK {
		t.Fatalf("first order: %d %s", w.Code, w.Body.String())
	}
	// Same nonce, different price: the signature is valid but the nonce is spent.
	o := f.order()
	o.Price = 101
	body, _ = json.Marshal(f.signedOrder(t, o))
	w := f.do(t, http.MethodPost, "/v1/perp/orders", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSpotSignatureCannotSignPerpOrder is the domain-separation guarantee. It is
// asserted through the LEDGER, not the status code: a spot-domain signature over
// the same field values recovers a different address here (ECDSA recovery always
// yields some key), and that address owns no funds, so the order cannot affect
// the real account. The important invariant is that the victim's position and
// balance are untouched.
func TestSpotSignatureCannotSignPerpOrder(t *testing.T) {
	f := newFixture(t)

	spotVerifier := spotgateway.NewVerifier(13700, spotgateway.DefaultDomainName, spotgateway.DefaultDomainVersion)
	spotOrder := spotgateway.SignedOrder{
		Market: 1, Side: 0, Type: 0, TIF: 0, Price: 100, Qty: "100",
		MaxNotional: "0", Nonce: 1, Expiry: uint64(unixNow()) + 600,
	}
	spotHash, err := hashSpotLike(spotVerifier, spotOrder)
	if err != nil {
		t.Fatalf("spot hash: %v", err)
	}
	env := SignedOrderEnvelope{Order: f.order(), Signature: f.sign(t, spotHash)}

	beforeBalance := new(uint256.Int).Set(f.ledger.Balance(f.account, 1))
	body, _ := json.Marshal(env)
	w := f.do(t, http.MethodPost, "/v1/perp/orders", body)

	// The order must NOT be applied to the real account either way.
	if _, ok := f.svc.Engine().Position(1, f.account); ok {
		t.Fatal("a spot signature must not open a position for the victim")
	}
	if after := f.ledger.Balance(f.account, 1); after.Cmp(beforeBalance) != 0 {
		t.Fatalf("victim balance changed: %s -> %s", beforeBalance, after)
	}
	// And it must never be accepted as the victim's order.
	if w.Code == http.StatusOK {
		t.Fatalf("a spot signature must not be accepted on the perp domain: %s", w.Body.String())
	}
}

// hashSpotLike exposes the spot digest for a structurally similar order. It uses
// the spot verifier's own hashing, so the resulting signature is valid only on
// the spot domain.
func hashSpotLike(v *spotgateway.Verifier, o spotgateway.SignedOrder) ([]byte, error) {
	env := spotgateway.SignedOrderEnvelope{Order: o, Signature: bytes.Repeat([]byte{0}, 65)}
	return v.OrderDigest(env)
}

// TestPlaceOrderRejectsMalformedSignature covers the structural checks: a blob
// that is not a well-formed signature must be refused outright rather than
// resolving to some random account.
func TestPlaceOrderRejectsMalformedSignature(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		sig  []byte
	}{
		{"wrong length", bytes.Repeat([]byte{1}, 64)},
		{"zero r", append(append(bytes.Repeat([]byte{0}, 32), bytes.Repeat([]byte{2}, 32)...), 1)},
		{"zero s", append(append(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{0}, 32)...), 1)},
		{"bad v", append(bytes.Repeat([]byte{1}, 64), 9)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := f.signedOrder(t, f.order())
			env.Signature = hexutil.Bytes(tc.sig)
			body, _ := json.Marshal(env)
			w := f.do(t, http.MethodPost, "/v1/perp/orders", body)
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusBadRequest {
				t.Fatalf("want 401/400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBookDepth(t *testing.T) {
	f := newFixture(t)
	body, _ := json.Marshal(f.signedOrder(t, f.order()))
	if w := f.do(t, http.MethodPost, "/v1/perp/orders", body); w.Code != http.StatusOK {
		t.Fatalf("place: %d %s", w.Code, w.Body.String())
	}
	w := f.do(t, http.MethodGet, "/v1/perp/book/1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("depth: %d", w.Code)
	}
	var d depthView
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Bids) != 1 || d.Bids[0].Price != 100 {
		t.Fatalf("bad bids: %+v", d.Bids)
	}
}

func TestListMarkets(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/v1/perp/markets", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("markets: %d", w.Code)
	}
	var ms []marketView
	if err := json.Unmarshal(w.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ms) != 1 || ms[0].Symbol != "ETH-PERP" || ms[0].MaxLeverage != 10 {
		t.Fatalf("bad markets: %+v", ms)
	}
}

func TestAccountBalances(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/v1/perp/accounts/"+f.account.Hex()+"/balances", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("balances: %d", w.Code)
	}
	var bs []balanceView
	if err := json.Unmarshal(w.Body.Bytes(), &bs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bs) != 2 {
		t.Fatalf("want base+quote balances, got %d", len(bs))
	}
}

func TestMarkPriceUnavailableWithoutSource(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/v1/perp/mark/1", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without a price source, got %d", w.Code)
	}
}

func TestMarkPriceWithSource(t *testing.T) {
	f := newFixture(t)
	src := services.NewStaticPriceSource()
	src.Set(1, 100)
	f.svc.SetPriceSource(src, services.PriceOptions{})
	f.svc.TickForTest(time.Now())

	w := f.do(t, http.MethodGet, "/v1/perp/mark/1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		MarkPrice uint64 `json:"markPrice"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.MarkPrice != 100 {
		t.Fatalf("want 100, got %d", out.MarkPrice)
	}
}

func TestCancelRequiresMatchingOrderID(t *testing.T) {
	f := newFixture(t)
	hash, err := f.gw.verifier.hashCancel(SignedCancel{Market: 1, OrderID: 5, Nonce: 2, Expiry: uint64(unixNow()) + 600})
	if err != nil {
		t.Fatalf("hashCancel: %v", err)
	}
	env := SignedCancelEnvelope{
		Cancel:    SignedCancel{Market: 1, OrderID: 5, Nonce: 2, Expiry: uint64(unixNow()) + 600},
		Signature: f.sign(t, hash),
	}
	body, _ := json.Marshal(env)
	// Path says 6, payload says 5.
	w := f.do(t, http.MethodDelete, "/v1/perp/orders/6", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestPositionsRouteEmpty(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/v1/perp/positions/"+f.account.Hex(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("positions: %d", w.Code)
	}
	if got := bytes.TrimSpace(w.Body.Bytes()); string(got) != "[]" {
		t.Fatalf("want empty array, got %s", got)
	}
}

func TestRouteNamespacingAvoidsSpotCollision(t *testing.T) {
	f := newFixture(t)

	// Build one engine that mounts a spot-style route and the perp group.
	r := gin.New()
	spot := r.Group("/v1")
	spot.GET("/markets", func(c *gin.Context) { c.JSON(200, gin.H{"spot": true}) })
	f.gw.Register(r)

	req := httptest.NewRequest(http.MethodGet, "/v1/markets", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("spot")) {
		t.Fatalf("spot route shadowed: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/perp/markets", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("perp route missing: %d %s", rec.Code, rec.Body.String())
	}
}

// bigIntZero keeps math/big referenced in tests that only compare strings.
var _ = big.NewInt

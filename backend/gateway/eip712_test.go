package gateway

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// signOrder builds a verifier, hashes the order with it, and signs with key so
// the test exercises the full hash -> sign -> recover round trip.
func signOrder(t *testing.T, v *Verifier, o SignedOrder) (common.Address, []byte) {
	t.Helper()
	h, err := v.hashOrder(o)
	if err != nil {
		t.Fatalf("hashOrder: %v", err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return crypto.PubkeyToAddress(key.PublicKey), sig
}

// signOrders signs each order with one fresh key and returns the signer address
// and the per-order signatures. It lets a test exercise several requests from
// the same account (signOrder mints a new key per call).
func signOrders(t *testing.T, v *Verifier, orders ...SignedOrder) (common.Address, [][]byte) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sigs := make([][]byte, len(orders))
	for i, o := range orders {
		h, err := v.hashOrder(o)
		if err != nil {
			t.Fatalf("hashOrder: %v", err)
		}
		sig, err := crypto.Sign(h, key)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		sigs[i] = sig
	}
	return crypto.PubkeyToAddress(key.PublicKey), sigs
}

// signCancel hashes and signs a cancel payload, mirroring signOrder.
func signCancel(t *testing.T, v *Verifier, c SignedCancel) (common.Address, []byte) {
	t.Helper()
	h, err := v.hashCancel(c)
	if err != nil {
		t.Fatalf("hashCancel: %v", err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return crypto.PubkeyToAddress(key.PublicKey), sig
}

func TestVerifierRecoverOrder(t *testing.T) {
	v := NewVerifier(13700, "", "")
	o := SignedOrder{
		Market: 1, Side: 0, Type: 0, TIF: 0, Price: 100,
		Qty: "10", MaxNotional: "0", Nonce: 1, Expiry: uint64(time.Now().Add(time.Hour).Unix()),
	}
	want, sig := signOrder(t, v, o)

	got, err := v.RecoverOrder(SignedOrderEnvelope{Order: o, Signature: sig})
	if err != nil {
		t.Fatalf("RecoverOrder: %v", err)
	}
	if got != want {
		t.Fatalf("recovered %s, want %s", got.Hex(), want.Hex())
	}
}

func TestVerifierRecoverOrderTamperedFieldFails(t *testing.T) {
	v := NewVerifier(13700, "", "")
	o := SignedOrder{
		Market: 1, Side: 0, Type: 0, TIF: 0, Price: 100,
		Qty: "10", MaxNotional: "0", Nonce: 1, Expiry: uint64(time.Now().Add(time.Hour).Unix()),
	}
	want, sig := signOrder(t, v, o)

	o.Price = 101 // tamper after signing
	got, err := v.RecoverOrder(SignedOrderEnvelope{Order: o, Signature: sig})
	if err != nil {
		t.Fatalf("RecoverOrder (tampered): %v", err)
	}
	if got == want {
		t.Fatalf("tampered order recovered the original signer %s; want a different address", got.Hex())
	}
}

func TestVerifierRecoverOrderBadQty(t *testing.T) {
	v := NewVerifier(13700, "", "")
	o := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "not-a-number", Nonce: 1, Expiry: 1}
	if _, err := v.RecoverOrder(SignedOrderEnvelope{Order: o, Signature: []byte{}}); err == nil {
		t.Fatal("expected error for a malformed qty")
	}
}

func TestVerifierRecoverCancel(t *testing.T) {
	v := NewVerifier(13700, "", "")
	c := SignedCancel{Market: 1, OrderID: 42, Nonce: 1, Expiry: uint64(time.Now().Add(time.Hour).Unix())}
	h, err := v.hashCancel(c)
	if err != nil {
		t.Fatalf("hashCancel: %v", err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	want := crypto.PubkeyToAddress(key.PublicKey)
	got, err := v.RecoverCancel(SignedCancelEnvelope{Cancel: c, Signature: sig})
	if err != nil {
		t.Fatalf("RecoverCancel: %v", err)
	}
	if got != want {
		t.Fatalf("recovered %s, want %s", got.Hex(), want.Hex())
	}
}

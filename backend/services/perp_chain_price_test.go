package services

import (
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"strings"

	"web3-amm-dex-backend/perp"
)

// fakeCaller implements just CallContract well enough for the price source.
//
// It pre-encodes the return payload once at construction: abi packing at call
// time makes the fake depend on the ABI machinery under concurrent tests, and a
// fake should only ever test the subject, not its own helper.
type fakeCaller struct {
	BlockchainAdapter
	payload []byte
	err     error
	calls   int
}

func newFakeCaller(price, updatedAt uint64) *fakeCaller {
	a, err := abi.JSON(strings.NewReader(chainPriceSourceABI))
	if err != nil {
		panic("chainPriceSourceABI is invalid: " + err.Error())
	}
	// Encode the OUTPUT tuple, which is what an eth_call returns. Packing the
	// inputs here would look like a successful call but decode to nothing.
	payload, err := a.Methods["getPrice"].Outputs.Pack(price, updatedAt)
	if err != nil {
		panic("fake oracle payload: " + err.Error())
	}
	return &fakeCaller{payload: payload}
}

func (f *fakeCaller) CallContract(string, []byte, uint64) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.payload, nil
}
func TestChainPriceSourceReturnsFreshPrice(t *testing.T) {
	fc := newFakeCaller(2000, uint64(time.Now().Unix()))
	src, err := NewChainPriceSource(fc, "0x0000000000000000000000000000000000000001", 60, time.Millisecond)
	if err != nil {
		t.Fatalf("NewChainPriceSource: %v", err)
	}
	p, ok := src.Index(1)
	if !ok || p != 2000 {
		t.Fatalf("want (2000, true), got (%d, %v)", p, ok)
	}
}

// TestChainPriceSourceTreatsStaleAsNoPrice is the security-relevant behaviour: a
// stale on-chain price must read as "no price" so the matcher skips liquidation.
func TestChainPriceSourceTreatsStaleAsNoPrice(t *testing.T) {
	fc := newFakeCaller(2000, 1)
	src, err := NewChainPriceSource(fc, "0x0000000000000000000000000000000000000001", 60, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := src.Index(1); ok {
		t.Fatalf("a stale price must not be usable, got %d", p)
	}
}

func TestChainPriceSourceTreatsZeroAsNoPrice(t *testing.T) {
	fc := newFakeCaller(0, uint64(time.Now().Unix()))
	src, err := NewChainPriceSource(fc, "0x0000000000000000000000000000000000000001", 60, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.Index(1); ok {
		t.Fatal("a zero price must not be usable")
	}
}

// TestChainPriceSourceCachesWithinRefresh pins that a tick polling every market
// does not issue an RPC call per lookup.
func TestChainPriceSourceCachesWithinRefresh(t *testing.T) {
	fc := newFakeCaller(2000, uint64(time.Now().Unix()))
	src, err := NewChainPriceSource(fc, "0x0000000000000000000000000000000000000001", 60, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		src.Index(1)
	}
	if fc.calls != 1 {
		t.Fatalf("want 1 RPC call within the refresh window, got %d", fc.calls)
	}
}

// TestChainPriceSourceSurvivesATransientRPCFailure pins that one failed call does
// not immediately blank a price; the tracker's own staleness bound still applies.
func TestChainPriceSourceSurvivesATransientRPCFailure(t *testing.T) {
	fc := newFakeCaller(2000, uint64(time.Now().Unix()))
	// A long refresh window so the retry falls inside the 3x survival window.
	src, err := NewChainPriceSource(fc, "0x0000000000000000000000000000000000000001", 60, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := src.Index(1); !ok || p != 2000 {
		t.Fatalf("seed: got (%d, %v)", p, ok)
	}

	fc.err = errors.New("rpc down")
	time.Sleep(250 * time.Millisecond) // past the refresh window, inside 3x
	if p, ok := src.Index(1); !ok || p != 2000 {
		t.Fatalf("a transient failure should keep serving the last price, got (%d, %v)", p, ok)
	}
	if src.LastError() == nil {
		t.Fatal("LastError should report the RPC failure")
	}

	// Past 3x the refresh, the source stops serving the stale value.
	time.Sleep(400 * time.Millisecond)
	if p, ok := src.Index(1); ok {
		t.Fatalf("a prolonged failure must stop serving the cached price, got %d", p)
	}
}

func TestChainPriceSourceRequiresAdapterAndContract(t *testing.T) {
	if _, err := NewChainPriceSource(nil, "0x1", 60, 0); err == nil {
		t.Fatal("want an error without an adapter")
	}
	fc := newFakeCaller(0, 0)
	if _, err := NewChainPriceSource(fc, "", 60, 0); err == nil {
		t.Fatal("want an error without a contract address")
	}
}

var _ perp.PriceSource = (*ChainPriceSource)(nil)

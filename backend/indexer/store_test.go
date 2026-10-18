package indexer

import (
	"testing"

	"go.etcd.io/bbolt"
)

func mustOpenStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStorePutTradeIdempotent(t *testing.T) {
	s := mustOpenStore(t)
	tr := TradeRecord{TradeID: 1, Market: 1, Price: 100, Qty: "1", Notional: "100", Maker: "0xaa", Taker: "0xbb", MakerSide: "sell"}
	if err := s.PutTrade(tr); err != nil {
		t.Fatalf("PutTrade: %v", err)
	}
	// Same key twice must not create a duplicate.
	tr2 := tr
	tr2.Notional = "999"
	if err := s.PutTrade(tr2); err != nil {
		t.Fatalf("PutTrade again: %v", err)
	}
	got, err := s.QueryTrades(0, "", 10)
	if err != nil {
		t.Fatalf("QueryTrades: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (idempotent overwrite)", len(got))
	}
	if got[0].Notional != "999" {
		t.Fatalf("notional = %s, want 999 (last write wins)", got[0].Notional)
	}
}

func TestStoreFundEventIdempotent(t *testing.T) {
	s := mustOpenStore(t)
	e := FundEvent{Kind: "deposit", TxHash: "0xtx", LogIndex: 0, Block: 5, Account: "0xaa", Asset: 1, Amount: "100", Seq: 1}
	if err := s.PutFundEvent(e); err != nil {
		t.Fatalf("PutFundEvent: %v", err)
	}
	if err := s.PutFundEvent(e); err != nil {
		t.Fatalf("PutFundEvent again: %v", err)
	}
	got, err := s.QueryFundEvents("", 0, 10)
	if err != nil {
		t.Fatalf("QueryFundEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}

func TestStoreCursorRoundTrip(t *testing.T) {
	s := mustOpenStore(t)
	if got, _ := s.LastProcessedBlock(); got != 0 {
		t.Fatalf("initial cursor = %d, want 0", got)
	}
	if err := s.SetLastProcessedBlock(42); err != nil {
		t.Fatalf("SetLastProcessedBlock: %v", err)
	}
	if got, _ := s.LastProcessedBlock(); got != 42 {
		t.Fatalf("cursor = %d, want 42", got)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/index.db"
	s, _ := OpenStore(path)
	_ = s.PutRootEvent(RootEvent{RootIndex: 1, Root: "0xrr", Block: 7})
	_ = s.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	roots, err := s2.QueryRoots(0, 100)
	if err != nil {
		t.Fatalf("QueryRoots: %v", err)
	}
	if len(roots) != 1 || roots[0].RootIndex != 1 {
		t.Fatalf("roots = %v, want exactly root 1", roots)
	}
}

// Keep bbolt imported for the doc reference even if unused here.
var _ = bbolt.DefaultOptions

package indexer

import (
	"testing"
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

func TestQueryFundEventsSortedByBlockDesc(t *testing.T) {
	s := mustOpenStore(t)
	// Insert in a deliberately non-chronological order.
	_ = s.PutFundEvent(FundEvent{Kind: "deposit", TxHash: "0xa", LogIndex: 0, Block: 5, Account: "0xaa", Asset: 1, Amount: "1"})
	_ = s.PutFundEvent(FundEvent{Kind: "deposit", TxHash: "0xb", LogIndex: 0, Block: 9, Account: "0xaa", Asset: 1, Amount: "2"})
	_ = s.PutFundEvent(FundEvent{Kind: "deposit", TxHash: "0xc", LogIndex: 0, Block: 7, Account: "0xaa", Asset: 1, Amount: "3"})

	got, err := s.QueryFundEvents("", 0, 10)
	if err != nil {
		t.Fatalf("QueryFundEvents: %v", err)
	}
	if len(got) != 3 || got[0].Block != 9 || got[1].Block != 7 || got[2].Block != 5 {
		t.Fatalf("events = %v, want blocks [9 7 5] newest first", got)
	}
}

func TestStoreTradeKeysSortNumerically(t *testing.T) {
	s := mustOpenStore(t)
	for _, id := range []uint64{9, 10, 11, 2} {
		_ = s.PutTrade(TradeRecord{TradeID: id, Market: 1, Price: 1, Qty: "1", Notional: "1", Maker: "0xaa", Taker: "0xbb", MakerSide: "sell"})
	}
	got, err := s.QueryTrades(0, "", 10)
	if err != nil {
		t.Fatalf("QueryTrades: %v", err)
	}
	// newest first => 11, 10, 9, 2
	want := []uint64{11, 10, 9, 2}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	for i, w := range want {
		if got[i].TradeID != w {
			t.Fatalf("got[%d].TradeID = %d, want %d (order %v)", i, got[i].TradeID, w, got)
		}
	}
}

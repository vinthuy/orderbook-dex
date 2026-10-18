package indexer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

func tradeLog(t *testing.T, kind string, block uint64, account common.Address, assetID, val uint64, seq uint64) services.Log {
	t.Helper()
	topic0 := crypto.Keccak256Hash([]byte(kind)).Hex()
	topic1 := common.BytesToHash(account.Bytes()).Hex()
	topic2 := common.BigToHash(new(big.Int).SetUint64(assetID)).Hex()
	data := make([]byte, 64)
	amt := new(big.Int).SetUint64(val).Bytes()
	copy(data[32-len(amt):32], amt)
	seqB := common.BigToHash(new(big.Int).SetUint64(seq))
	copy(data[32:64], seqB.Bytes())
	return services.Log{
		Address:     "0xvault",
		Topics:      []string{topic0, topic1, topic2},
		Data:        data,
		BlockNumber: block,
		TxHash:      "0xtx" + new(big.Int).SetUint64(block).String(),
		LogIndex:    uint(block),
	}
}

func TestIndexerOnTradeWritesHistory(t *testing.T) {
	s := mustOpenStore(t)
	bus := services.NewEventBus()
	idx := New(s, bus, nil, "0xvault", 1)

	if err := idx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer idx.Stop()

	var a, b ledger.AccountID
	a[19] = 1
	b[19] = 2
	tr := matching.Trade{
		ID: 7, Market: 1, Price: 100,
		Qty: uint256.NewInt(1), Notional: uint256.NewInt(100),
		Maker: a, Taker: b, MakerSide: matching.SideSell,
	}
	_ = bus.Publish(services.Event{Topic: services.TopicOrderBookTrade, Data: services.TradeEvent{Trade: tr}})

	got, err := s.QueryTrades(0, "", 10)
	if err != nil {
		t.Fatalf("QueryTrades: %v", err)
	}
	if len(got) != 1 || got[0].TradeID != 7 {
		t.Fatalf("trades = %v, want exactly trade 7", got)
	}
}

func TestIndexerPollOnceWritesEventsAndAdvancesCursor(t *testing.T) {
	s := mustOpenStore(t)
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 5, account, 1, 100, 1),
		tradeLog(t, "Withdrawal(address,uint32,uint256,uint256)", 10, account, 1, 30, 2),
	}

	idx := New(s, nil, adapter, "0xvault", 1)
	if err := idx.PollOnce(); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	events, err := s.QueryFundEvents("", 0, 10)
	if err != nil {
		t.Fatalf("QueryFundEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if got, _ := s.LastProcessedBlock(); got != 19 {
		t.Fatalf("cursor = %d, want 19", got)
	}
}

func TestIndexerOnTradeWithNoSubscriberStillWrites(t *testing.T) {
	// onTrade must write even when the indexer's own Start was never called
	// (direct subscription registration is part of Start; here we call the
	// handler method directly to decouple from lifecycle).
	s := mustOpenStore(t)
	idx := New(s, nil, nil, "0xvault", 1)
	var a ledger.AccountID
	a[19] = 9
	tr := matching.Trade{ID: 1, Market: 1, Price: 1, Qty: uint256.NewInt(1), Notional: uint256.NewInt(1), Maker: a, Taker: a, MakerSide: matching.SideBuy}
	if err := idx.handleTrade(services.Event{Data: services.TradeEvent{Trade: tr}}); err != nil {
		t.Fatalf("handleTrade: %v", err)
	}
	got, _ := s.QueryTrades(0, "", 10)
	if len(got) != 1 {
		t.Fatalf("trades = %d, want 1", len(got))
	}
}

func TestIndexerPollRespectsConfirmations(t *testing.T) {
	s := mustOpenStore(t)
	adapter := services.NewMockBlockchainAdapter()
	adapter.BlockNumber = 20
	account := common.HexToAddress("0x2222222222222222222222222222222222222222")
	adapter.Logs = []services.Log{
		tradeLog(t, "Deposit(address,uint32,uint256,uint256)", 19, account, 1, 10, 1),
	}
	idx := New(s, nil, adapter, "0xvault", 5) // only blocks <= 15 confirmed
	if err := idx.PollOnce(); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	events, _ := s.QueryFundEvents("", 0, 10)
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0 (unconfirmed)", len(events))
	}
}

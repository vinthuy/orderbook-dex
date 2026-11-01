package gateway

import (
	"sync"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

// Sequencer is the memory serialization layer: one mutex forces all writes into
// the order book through a single lane, and a monotonic counter assigns each
// request an ID. The request ID is observational only (for the state machine
// and logs) — the kernel still assigns OrderID/TradeID from the instruction
// order, so replay determinism is untouched (spec §4.5).
type Sequencer struct {
	mu  sync.Mutex
	seq uint64
	ob  *services.OrderBookService
}

func NewSequencer(ob *services.OrderBookService) *Sequencer {
	return &Sequencer{ob: ob}
}

// Place serializes and forwards a place request, returning the result and the
// request ID.
func (s *Sequencer) Place(req matching.PlaceRequest) (matching.Result, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	res, err := s.ob.Place(req)
	return res, s.seq, err
}

// Cancel serializes and forwards a cancel request, returning the result and the
// request ID.
func (s *Sequencer) Cancel(market matching.MarketID, a ledger.AccountID, id matching.OrderID) (matching.Result, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	res, err := s.ob.Cancel(market, a, id)
	return res, s.seq, err
}

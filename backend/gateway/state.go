package gateway

import (
	"sync"

	"web3-amm-dex-backend/matching"
)

// RequestState is the gateway's view of a request's lifecycle. It tracks only
// admission — whether the request was accepted into the order book or rejected
// before reaching it. The order's matching status (new/filled/cancelled) is the
// kernel's business and is not duplicated here.
type RequestState uint8

const (
	StateReceived RequestState = iota // accepted for processing, not yet forwarded
	StateAccepted                     // forwarded to the order book
	StateRejected                     // signature/validation/limit failure
)

// RequestRecord is one request's lifecycle record.
type RequestRecord struct {
	RequestID uint64
	State     RequestState
	OrderID   matching.OrderID // set when StateAccepted produced/rested an order
	Err       string           // rejection reason when StateRejected
}

// StateMachine is an in-memory map of request IDs to their lifecycle state,
// plus an orderID -> requestID index for cancel/query back-linking. It is not
// persisted: a crash rebuilds the order book from the instruction WAL and the
// gateway drops its request records (harmless — they are observational only).
type StateMachine struct {
	mu       sync.Mutex
	requests map[uint64]RequestRecord
	byOrder  map[matching.OrderID]uint64
}

func NewStateMachine() *StateMachine {
	return &StateMachine{
		requests: make(map[uint64]RequestRecord),
		byOrder:  make(map[matching.OrderID]uint64),
	}
}

// Begin records a request as received. The requestID is allocated by the
// sequencer (spec §4.5/§4.6) — the state machine never issues IDs.
func (sm *StateMachine) Begin(requestID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.requests[requestID] = RequestRecord{RequestID: requestID, State: StateReceived}
}

// Accept marks the request accepted and links it to the order it produced.
func (sm *StateMachine) Accept(requestID uint64, orderID matching.OrderID) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.requests[requestID] = RequestRecord{RequestID: requestID, State: StateAccepted, OrderID: orderID}
	if orderID != 0 {
		sm.byOrder[orderID] = requestID
	}
}

// Reject marks the request rejected with a reason.
func (sm *StateMachine) Reject(requestID uint64, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.requests[requestID] = RequestRecord{RequestID: requestID, State: StateRejected, Err: msg}
}

// Get returns the record for a request ID.
func (sm *StateMachine) Get(requestID uint64) (RequestRecord, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	r, ok := sm.requests[requestID]
	return r, ok
}

// RequestForOrder returns the record that produced (or last touched) an order.
func (sm *StateMachine) RequestForOrder(orderID matching.OrderID) (RequestRecord, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	requestID, ok := sm.byOrder[orderID]
	if !ok {
		return RequestRecord{}, false
	}
	r, ok := sm.requests[requestID]
	return r, ok
}

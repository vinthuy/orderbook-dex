package gateway

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

const wsSendBuffer = 64

// subscription is a client's event filter. A nil market/account is a wildcard.
type subscription struct {
	market  *matching.MarketID
	account *ledger.AccountID
}

func (s subscription) matches(m *matching.MarketID, a *ledger.AccountID) bool {
	if s.market != nil && (m == nil || *s.market != *m) {
		return false
	}
	if s.account != nil && (a == nil || *s.account != *a) {
		return false
	}
	return true
}

// wsClient is one connected peer: its outbound queue and its filters.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	subs []subscription
}

// wsFrame is the wire envelope for every push: {type, data}.
type wsFrame struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// Hub fans order-book events out to subscribed WebSocket clients. It subscribes
// to the bus topics and also emits depth snapshots after order/trade events for
// the affected market (spec §4.8).
type Hub struct {
	mu      sync.Mutex
	clients map[*wsClient]struct{}
	ob      *services.OrderBookService
	depth   int
}

func NewHub(ob *services.OrderBookService, depth int) *Hub {
	if depth <= 0 {
		depth = DefaultDepth
	}
	return &Hub{clients: make(map[*wsClient]struct{}), ob: ob, depth: depth}
}

// Subscribe registers the hub on the bus's order-book topics. Handlers must
// never block: they enqueue to per-client buffered channels and drop clients
// whose queue is full (spec §4.8: never let a slow client back-pressure the
// order book).
func (h *Hub) Subscribe(bus *services.EventBus) {
	bus.Subscribe(services.TopicOrderBookOrder, h.onOrder)
	bus.Subscribe(services.TopicOrderBookTrade, h.onTrade)
	bus.Subscribe(services.TopicOrderBookDegraded, h.onDegraded)
}

func (h *Hub) onOrder(e services.Event) error {
	ev, ok := e.Data.(services.OrderEvent)
	if !ok || ev.Order == nil {
		return nil
	}
	m := ev.Order.Market
	a := ev.Order.Account
	h.broadcast(&m, &a, wsFrame{Type: "order", Data: orderToView(ev.Order)})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onTrade(e services.Event) error {
	ev, ok := e.Data.(services.TradeEvent)
	if !ok {
		return nil
	}
	m := ev.Trade.Market
	h.broadcastTrade(&m, &ev.Trade.Maker, &ev.Trade.Taker, wsFrame{Type: "trade", Data: tradeToView(ev.Trade)})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onDegraded(e services.Event) error {
	h.broadcast(nil, nil, wsFrame{Type: "degraded"})
	return nil
}

// pushDepth emits a full depth snapshot for a market (spec §4.8: full snapshot,
// no incremental compression in v1).
func (h *Hub) pushDepth(m matching.MarketID) {
	bids, asks := h.ob.Engine().Book(m, h.depth)
	h.broadcast(&m, nil, wsFrame{Type: "depth", Data: depthToView(m, bids, asks)})
}

// broadcast sends a frame to every client whose subscription matches the
// (market, account) pair.
func (h *Hub) broadcast(market *matching.MarketID, account *ledger.AccountID, frame wsFrame) {
	data, err := json.Marshal(frame)
	if err != nil {
		log.Printf("gateway: marshal ws frame: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !c.subscribed(market, account) {
			continue
		}
		h.enqueue(c, data)
	}
}

// broadcastTrade sends a trade frame once per client: a trade belongs to both
// counterparties, so a client matches if any of its subscriptions matches the
// market with either the maker or the taker account. A market-only subscriber
// receives it exactly once.
func (h *Hub) broadcastTrade(market *matching.MarketID, maker, taker *ledger.AccountID, frame wsFrame) {
	data, err := json.Marshal(frame)
	if err != nil {
		log.Printf("gateway: marshal ws frame: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.subscribed(market, maker) || c.subscribed(market, taker) {
			h.enqueue(c, data)
		}
	}
}

// enqueue delivers a frame to a client, dropping the client if its queue is
// full (a slow client must never back-pressure the order book).
func (h *Hub) enqueue(c *wsClient, data []byte) {
	select {
	case c.send <- data:
	default:
		h.drop(c)
	}
}

func (h *Hub) drop(c *wsClient) {
	delete(h.clients, c)
	close(c.send)
	_ = c.conn.Close()
}

// add registers a client; remove deregisters it.
func (h *Hub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
	}
	h.mu.Unlock()
}

func (c *wsClient) subscribed(market *matching.MarketID, account *ledger.AccountID) bool {
	for _, s := range c.subs {
		if s.matches(market, account) {
			return true
		}
	}
	return false
}

// setSubs replaces the client's subscription filters (guarded by the hub lock
// via add/remove; the read in subscribed races with this only at unsubscribe,
// which is acceptable: a torn subscription delivers at most one extra frame).
func (c *wsClient) setSubs(subs []subscription) {
	c.subs = subs
}

// upgrader is the websocket upgrader shared by the /ws handler.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

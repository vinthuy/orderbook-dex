package perpgateway

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
	"web3-amm-dex-backend/services"
)

const wsSendBuffer = 64

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	// Browser clients are expected; the gateway does not use cookies, so it has
	// no cross-origin authority to protect.
	CheckOrigin: func(*http.Request) bool { return true },
}

// subscription narrows what a client receives. A nil field means "everything".
type subscription struct {
	market  *perp.MarketID
	account *ledger.AccountID
}

func (s subscription) matches(m *perp.MarketID, a *ledger.AccountID) bool {
	if s.market != nil && (m == nil || *m != *s.market) {
		return false
	}
	if s.account != nil && (a == nil || *a != *s.account) {
		return false
	}
	return true
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	mu   sync.Mutex
	subs []subscription
}

func (c *wsClient) addSub(s subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs = append(c.subs, s)
}

func (c *wsClient) setSubs(s []subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs = s
}

// wanted reports whether any subscription wants this (market, account) pair.
// With no subscriptions at all the client receives nothing, which matches the
// spot gateway's "explicit subscribe" contract.
func (c *wsClient) wanted(m *perp.MarketID, a *ledger.AccountID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		if s.matches(m, a) {
			return true
		}
	}
	return false
}

// frame is one outbound push.
type frame struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub fans out perp events to subscribed WebSocket clients.
type Hub struct {
	perp  *services.PerpOrderBookService
	depth int

	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

// NewHub builds a hub that pushes depth snapshots of the given size.
func NewHub(p *services.PerpOrderBookService, depth int) *Hub {
	if depth <= 0 {
		depth = DefaultDepth
	}
	return &Hub{perp: p, depth: depth, clients: make(map[*wsClient]struct{})}
}

// Subscribe wires the hub to the perp service's event topics.
func (h *Hub) Subscribe(bus *services.EventBus) {
	bus.Subscribe(services.TopicPerpTrade, h.onTrade)
	bus.Subscribe(services.TopicPerpPosition, h.onPosition)
	bus.Subscribe(services.TopicPerpMark, h.onMark)
	bus.Subscribe(services.TopicPerpFunding, h.onFunding)
	bus.Subscribe(services.TopicPerpLiquidation, h.onLiquidation)
	bus.Subscribe(services.TopicPerpDegraded, h.onDegraded)
	bus.Subscribe(services.TopicPerpBadDebt, h.onBadDebt)
}

func (h *Hub) add(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

func (h *Hub) remove(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
}

// broadcast delivers one frame to every client whose subscription matches.
func (h *Hub) broadcast(m *perp.MarketID, a *ledger.AccountID, f frame) {
	payload, err := json.Marshal(f)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !c.wanted(m, a) {
			continue
		}
		select {
		case c.send <- payload:
		default:
			// A slow client must not stall the matcher: drop the frame. The
			// client still receives the next depth snapshot, which is the
			// authoritative catch-up view.
		}
	}
}

// pushDepth sends a fresh depth snapshot for a market.
func (h *Hub) pushDepth(m perp.MarketID) {
	longs, shorts := h.perp.Engine().Book(m, h.depth)
	h.broadcast(&m, nil, frame{Type: "depth", Data: depthToView(m, longs, shorts)})
}

func (h *Hub) onTrade(e services.Event) error {
	t, ok := e.Data.(perp.Trade)
	if !ok {
		return nil
	}
	m := t.Market
	h.broadcast(&m, nil, frame{Type: "trade", Data: tradeToView(t)})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onPosition(e services.Event) error {
	o, ok := e.Data.(*perp.Order)
	if !ok || o == nil {
		return nil
	}
	m := o.Market
	h.broadcast(&m, &o.Account, frame{Type: "order", Data: orderToView(o)})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onMark(e services.Event) error {
	u, ok := e.Data.(services.MarkUpdate)
	if !ok {
		return nil
	}
	m := u.Market
	h.broadcast(&m, nil, frame{Type: "mark", Data: gin.H{
		"market": m, "index": u.Index, "mark": u.Mark, "at": u.At,
	}})
	return nil
}

func (h *Hub) onFunding(e services.Event) error {
	r, ok := e.Data.(services.FundingResult)
	if !ok {
		return nil
	}
	m := r.Market
	h.broadcast(&m, nil, frame{Type: "funding", Data: gin.H{
		"market": m, "index": r.Index, "rate": r.Rate, "mark": r.Mark,
		"legs": len(r.Legs), "skips": len(r.Skips),
	}})
	return nil
}

func (h *Hub) onLiquidation(e services.Event) error {
	ev, ok := e.Data.(services.LiquidationEvent)
	if !ok {
		return nil
	}
	m := ev.Market
	h.broadcast(&m, &ev.Account, frame{Type: "liquidation", Data: gin.H{
		"market": m, "account": ev.Account.Hex(), "size": uintString(ev.Size),
		"mark": ev.Mark, "equity": bigString(ev.Equity), "fee": uintString(ev.Fee),
		"partial": ev.Partial, "at": ev.At,
	}})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onDegraded(e services.Event) error {
	h.broadcast(nil, nil, frame{Type: "degraded", Data: gin.H{"detail": "perp service degraded"}})
	return nil
}

func (h *Hub) onBadDebt(e services.Event) error {
	ev, ok := e.Data.(services.BadDebtEvent)
	if !ok {
		return nil
	}
	h.broadcast(nil, &ev.Account, frame{Type: "baddebt", Data: gin.H{
		"account": ev.Account.Hex(), "asset": ev.Asset, "amount": uintString(ev.Amount),
	}})
	return nil
}

// serveWS upgrades the connection and pumps frames until the client leaves.
// Inbound messages are {"op":"subscribe","market":N,"account":"0x..."} or
// {"op":"unsubscribe"}.
func (g *Gateway) serveWS(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // Upgrade already wrote an error response
	}
	client := &wsClient{conn: conn, send: make(chan []byte, wsSendBuffer)}
	g.hub.add(client)
	defer g.hub.remove(client)

	go func() {
		for f := range client.send {
			if err := conn.WriteMessage(websocket.TextMessage, f); err != nil {
				return
			}
		}
	}()

	for {
		var msg struct {
			Op      string  `json:"op"`
			Market  *uint32 `json:"market"`
			Account *string `json:"account"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Op {
		case "subscribe":
			var sub subscription
			if msg.Market != nil {
				m := perp.MarketID(*msg.Market)
				sub.market = &m
			}
			if msg.Account != nil {
				a, err := ledger.AccountFromHex(*msg.Account)
				if err != nil {
					continue
				}
				sub.account = &a
			}
			client.addSub(sub)
			// Send an immediate depth snapshot so the client is not blind until
			// the next trade.
			if sub.market != nil {
				longs, shorts := g.perp.Engine().Book(*sub.market, g.cfg.Depth)
				if payload, err := json.Marshal(frame{
					Type: "depth", Data: depthToView(*sub.market, longs, shorts),
				}); err == nil {
					select {
					case client.send <- payload:
					default:
					}
				}
			}
		case "unsubscribe":
			client.setSubs(nil)
		}
	}
}

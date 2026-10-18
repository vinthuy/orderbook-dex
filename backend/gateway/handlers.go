package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

// --- JSON view types (uint256 as decimal strings) -------------------------

type orderView struct {
	ID          uint64 `json:"id"`
	Seq         uint64 `json:"seq"`
	Market      uint32 `json:"market"`
	Account     string `json:"account"`
	Side        string `json:"side"`
	Type        string `json:"type"`
	TIF         string `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"maxNotional,omitempty"`
	Filled      string `json:"filled"`
	Status      string `json:"status"`
}

type tradeView struct {
	ID         uint64 `json:"id"`
	Market     uint32 `json:"market"`
	Price      uint64 `json:"price"`
	Qty        string `json:"qty"`
	Notional   string `json:"notional"`
	MakerOrder uint64 `json:"makerOrder"`
	TakerOrder uint64 `json:"takerOrder"`
	Maker      string `json:"maker"`
	Taker      string `json:"taker"`
	MakerSide  string `json:"makerSide"`
}

type levelView struct {
	Price uint64 `json:"price"`
	Total string `json:"total"`
}

type depthView struct {
	Market uint32      `json:"market"`
	Bids   []levelView `json:"bids"`
	Asks   []levelView `json:"asks"`
}

type balanceView struct {
	Asset     uint32 `json:"asset"`
	Balance   string `json:"balance"`
	Available string `json:"available"`
	Held      string `json:"held"`
}

func orderToView(o *matching.Order) orderView {
	if o == nil {
		return orderView{}
	}
	v := orderView{
		ID:      uint64(o.ID),
		Seq:     uint64(o.Seq),
		Market:  uint32(o.Market),
		Account: o.Account.Hex(),
		Side:    o.Side.String(),
		Type:    o.Type.String(),
		TIF:     o.TIF.String(),
		Price:   uint64(o.Price),
		Status:  o.Status.String(),
	}
	if o.Qty != nil {
		v.Qty = o.Qty.Dec()
	}
	if o.MaxNotional != nil && !o.MaxNotional.IsZero() {
		v.MaxNotional = o.MaxNotional.Dec()
	}
	if o.Filled != nil {
		v.Filled = o.Filled.Dec()
	}
	return v
}

func tradeToView(t matching.Trade) tradeView {
	v := tradeView{
		ID:         uint64(t.ID),
		Market:     uint32(t.Market),
		Price:      uint64(t.Price),
		MakerOrder: uint64(t.MakerOrder),
		TakerOrder: uint64(t.TakerOrder),
		Maker:      t.Maker.Hex(),
		Taker:      t.Taker.Hex(),
		MakerSide:  t.MakerSide.String(),
	}
	if t.Qty != nil {
		v.Qty = t.Qty.Dec()
	}
	if t.Notional != nil {
		v.Notional = t.Notional.Dec()
	}
	return v
}

func depthToView(m matching.MarketID, bids, asks []matching.LevelView) depthView {
	v := depthView{Market: uint32(m)}
	for _, l := range bids {
		total := "0"
		if l.Total != nil {
			total = l.Total.Dec()
		}
		v.Bids = append(v.Bids, levelView{Price: uint64(l.Price), Total: total})
	}
	for _, l := range asks {
		total := "0"
		if l.Total != nil {
			total = l.Total.Dec()
		}
		v.Asks = append(v.Asks, levelView{Price: uint64(l.Price), Total: total})
	}
	return v
}

// --- handlers --------------------------------------------------------------

func (g *Gateway) placeOrder(c *gin.Context) {
	var env SignedOrderEnvelope
	if err := c.ShouldBindJSON(&env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}
	if env.Order.Expiry <= uint64(time.Now().Unix()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_expired"})
		return
	}
	signer, err := g.verifier.RecoverOrder(env)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	account := ledger.AccountID(signer)
	if !g.limiter.Allow(account) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
		return
	}
	if err := g.nonce.CheckAndAdvance(account, env.Order.Nonce); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "nonce_reused"})
		return
	}

	req, err := orderToPlaceRequest(account, env.Order)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}

	res, requestID, err := g.seq.Place(req)
	g.state.Begin(requestID)
	if err != nil {
		g.state.Reject(requestID, err)
		code, codeErr := mapOrderBookError(err)
		c.JSON(code, gin.H{"error": codeErr, "requestId": requestID, "detail": err.Error()})
		return
	}
	var orderID matching.OrderID
	if res.Order != nil {
		orderID = res.Order.ID
	}
	g.state.Accept(requestID, orderID)
	c.JSON(http.StatusOK, gin.H{
		"requestId": requestID,
		"order":     orderToView(res.Order),
		"trades":    tradesToViews(res.Trades),
	})
}

func (g *Gateway) cancelOrder(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	var env SignedCancelEnvelope
	if err := c.ShouldBindJSON(&env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}
	if env.Cancel.Expiry <= uint64(time.Now().Unix()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_expired"})
		return
	}
	if env.Cancel.OrderID != orderID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": "orderId in path and payload must match"})
		return
	}
	signer, err := g.verifier.RecoverCancel(env)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	account := ledger.AccountID(signer)
	if !g.limiter.Allow(account) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
		return
	}
	if err := g.nonce.CheckAndAdvance(account, env.Cancel.Nonce); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "nonce_reused"})
		return
	}

	res, requestID, err := g.seq.Cancel(matching.MarketID(env.Cancel.Market), account, matching.OrderID(orderID))
	g.state.Begin(requestID)
	if err != nil {
		g.state.Reject(requestID, err)
		code, codeErr := mapOrderBookError(err)
		c.JSON(code, gin.H{"error": codeErr, "requestId": requestID, "detail": err.Error()})
		return
	}
	var oid matching.OrderID
	if res.Order != nil {
		oid = res.Order.ID
	}
	g.state.Accept(requestID, oid)
	c.JSON(http.StatusOK, gin.H{"requestId": requestID, "order": orderToView(res.Order)})
}

func (g *Gateway) getOrder(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	o, ok := g.ob.Engine().Order(matching.OrderID(orderID))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	c.JSON(http.StatusOK, orderToView(o))
}

func (g *Gateway) listMarkets(c *gin.Context) {
	markets := g.ob.Engine().Markets()
	out := make([]gin.H, 0, len(markets))
	for _, m := range markets {
		item := gin.H{
			"id":           uint32(m.ID),
			"symbol":       m.Symbol,
			"base":         uint32(m.Base),
			"quote":        uint32(m.Quote),
			"baseDecimals": m.BaseDecimals,
		}
		if m.MinQty != nil {
			item["minQty"] = m.MinQty.Dec()
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, out)
}

func (g *Gateway) bookDepth(c *gin.Context) {
	market, err := strconv.ParseUint(c.Param("market"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	depth := g.cfg.Depth
	if v := c.Query("depth"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			depth = n
		}
	}
	bids, asks := g.ob.Engine().Book(matching.MarketID(market), depth)
	c.JSON(http.StatusOK, depthToView(matching.MarketID(market), bids, asks))
}

func (g *Gateway) accountBalances(c *gin.Context) {
	account, err := ledger.AccountFromHex(c.Param("account"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	// Report every asset referenced by a configured market (base or quote).
	seen := map[ledger.AssetID]bool{}
	for _, m := range g.ob.Engine().Markets() {
		seen[m.Base] = true
		seen[m.Quote] = true
	}
	out := make([]balanceView, 0, len(seen))
	for asset := range seen {
		avail := g.ob.Holds().Available(account, asset)
		held := g.ob.Holds().Held(account, asset)
		// balance = available + held (available is clamped to 0, and in normal
		// operation balance >= held, so this recovers the total without touching
		// the ledger directly).
		balance := new(uint256.Int).Add(avail, held)
		out = append(out, balanceView{
			Asset:     uint32(asset),
			Balance:   balance.Dec(),
			Available: avail.Dec(),
			Held:      held.Dec(),
		})
	}
	c.JSON(http.StatusOK, out)
}

// --- helpers ---------------------------------------------------------------

func orderToPlaceRequest(account ledger.AccountID, o SignedOrder) (matching.PlaceRequest, error) {
	qty, err := parseUint(o.Qty)
	if err != nil {
		return matching.PlaceRequest{}, err
	}
	maxNotional, err := parseUint(o.MaxNotional)
	if err != nil {
		return matching.PlaceRequest{}, err
	}
	return matching.PlaceRequest{
		Market:      matching.MarketID(o.Market),
		Account:     account,
		Side:        matching.Side(o.Side),
		Type:        matching.Type(o.Type),
		TIF:         matching.TIF(o.TIF),
		Price:       matching.Price(o.Price),
		Qty:         qty,
		MaxNotional: maxNotional,
	}, nil
}

func tradesToViews(ts []matching.Trade) []tradeView {
	out := make([]tradeView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tradeToView(t))
	}
	return out
}

// mapOrderBookError maps order-book/ledger errors to (http code, error key).
func mapOrderBookError(err error) (int, string) {
	switch {
	case errors.Is(err, services.ErrServiceDegraded):
		return http.StatusServiceUnavailable, "service_degraded"
	case errors.Is(err, ledger.ErrInsufficientBalance):
		return http.StatusUnprocessableEntity, "insufficient_balance"
	case errors.Is(err, matching.ErrBadRequest),
		errors.Is(err, matching.ErrUnknownMarket),
		errors.Is(err, matching.ErrAmountTooLarge):
		return http.StatusBadRequest, "order_rejected"
	case errors.Is(err, matching.ErrNotOwner):
		return http.StatusForbidden, "not_owner"
	case errors.Is(err, matching.ErrOrderNotFound):
		return http.StatusNotFound, "not_found"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// serveWS upgrades the connection, registers the client, and pumps outbound
// frames until the client disconnects. Inbound messages are subscription
// commands {op, market?, account?} / {op:"unsubscribe"}.
func (g *Gateway) serveWS(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // Upgrade writes its own error response
	}
	client := &wsClient{conn: conn, send: make(chan []byte, wsSendBuffer)}
	g.hub.add(client)
	defer func() { g.hub.remove(client); _ = conn.Close() }()

	// writer pump
	go func() {
		for frame := range client.send {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
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
				m := matching.MarketID(*msg.Market)
				sub.market = &m
			}
			if msg.Account != nil {
				a, err := ledger.AccountFromHex(*msg.Account)
				if err != nil {
					continue
				}
				sub.account = &a
			}
			client.setSubs(append(client.subs, sub))
		case "unsubscribe":
			client.setSubs(nil)
		}
	}
}

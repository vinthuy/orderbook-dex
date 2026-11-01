package perpgateway

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/gateway"
	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
	"web3-amm-dex-backend/services"
)

// placeOrder verifies the signature, rate limit, and nonce, then submits the
// order. Every rejection happens before the kernel is touched.
func (g *Gateway) placeOrder(c *gin.Context) {
	var env SignedOrderEnvelope
	if err := c.ShouldBindJSON(&env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}
	if env.Order.Expiry <= uint64(unixNow()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_expired"})
		return
	}
	signer, err := g.verifier.RecoverOrder(env)
	if err != nil {
		if errors.Is(err, ErrBadAmount) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	account := accountID(signer)
	if !g.limiter.Allow(account) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
		return
	}
	if err := g.nonce.CheckAndAdvance(account, env.Order.Nonce); err != nil {
		if errors.Is(err, gateway.ErrNonceReused) {
			c.JSON(http.StatusConflict, gin.H{"error": "nonce_reused"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal", "detail": err.Error()})
		return
	}

	req, err := orderToPlaceRequest(account, env.Order)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}

	res, err := g.perp.Place(req)
	if err != nil {
		code, key := mapPerpError(err)
		c.JSON(code, gin.H{"error": key, "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"order":  orderToView(res.Order),
		"trades": tradesToViews(res.Trades),
	})
}

// cancelOrder verifies a signed cancel and removes the resting order.
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
	if env.Cancel.Expiry <= uint64(unixNow()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_expired"})
		return
	}
	if uint64(env.Cancel.OrderID) != orderID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": "orderId in path and payload must match"})
		return
	}
	signer, err := g.verifier.RecoverCancel(env)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	account := accountID(signer)
	if !g.limiter.Allow(account) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
		return
	}
	if err := g.nonce.CheckAndAdvance(account, env.Cancel.Nonce); err != nil {
		if errors.Is(err, gateway.ErrNonceReused) {
			c.JSON(http.StatusConflict, gin.H{"error": "nonce_reused"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal", "detail": err.Error()})
		return
	}

	res, err := g.perp.Cancel(perp.MarketID(env.Cancel.Market), account, perp.OrderID(orderID))
	if err != nil {
		code, key := mapPerpError(err)
		c.JSON(code, gin.H{"error": key, "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": orderToView(res.Order)})
}

func (g *Gateway) getOrder(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	o, ok := g.perp.Engine().Order(perp.OrderID(orderID))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	c.JSON(http.StatusOK, orderToView(o))
}

func (g *Gateway) listMarkets(c *gin.Context) {
	markets := g.perp.Engine().Markets()
	out := make([]marketView, 0, len(markets))
	for _, m := range markets {
		out = append(out, marketToView(m))
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
	longs, shorts := g.perp.Engine().Book(perp.MarketID(market), depth)
	c.JSON(http.StatusOK, depthToView(perp.MarketID(market), longs, shorts))
}

// accountPositions returns every open position for an account across markets.
func (g *Gateway) accountPositions(c *gin.Context) {
	account, err := ledger.AccountFromHex(c.Param("account"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	out := make([]positionView, 0)
	for _, m := range g.perp.Engine().Markets() {
		p, ok := g.perp.Engine().Position(m.ID, account)
		if !ok {
			continue
		}
		mark, hasMark := g.perp.MarkPrice(m.ID)
		out = append(out, positionToView(*p, mark, hasMark, m))
	}
	c.JSON(http.StatusOK, out)
}

// accountPosition returns one position, or 404 when the account has none.
func (g *Gateway) accountPosition(c *gin.Context) {
	account, err := ledger.AccountFromHex(c.Param("account"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	market, err := strconv.ParseUint(c.Param("market"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	id := perp.MarketID(market)
	p, ok := g.perp.Engine().Position(id, account)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	var m perp.Market
	for _, mm := range g.perp.Engine().Markets() {
		if mm.ID == id {
			m = mm
			break
		}
	}
	mark, hasMark := g.perp.MarkPrice(id)
	c.JSON(http.StatusOK, positionToView(*p, mark, hasMark, m))
}

// accountBalances reports every asset a perp market references.
func (g *Gateway) accountBalances(c *gin.Context) {
	account, err := ledger.AccountFromHex(c.Param("account"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	seen := map[ledger.AssetID]bool{}
	for _, m := range g.perp.Engine().Markets() {
		seen[m.Base] = true
		seen[m.Quote] = true
	}
	out := make([]balanceView, 0, len(seen))
	for asset := range seen {
		avail := g.perp.Holds().Available(account, asset)
		held := g.perp.Holds().Held(account, asset)
		out = append(out, balanceView{
			Asset: uint32(asset), Balance: new(uint256.Int).Add(avail, held).Dec(),
			Available: avail.Dec(), Held: held.Dec(),
		})
	}
	c.JSON(http.StatusOK, out)
}

// markPrice returns the market's current mark price and index price.
func (g *Gateway) markPrice(c *gin.Context) {
	market, err := strconv.ParseUint(c.Param("market"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	id := perp.MarketID(market)
	mark, ok := g.perp.MarkPrice(id)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no_mark_price"})
		return
	}
	resp := gin.H{"market": uint32(id), "markPrice": uint64(mark)}
	if index, ok := g.perp.IndexPrice(id); ok {
		resp["indexPrice"] = uint64(index)
	}
	c.JSON(http.StatusOK, resp)
}

// fundingState returns the last funding round's summary for a market.
func (g *Gateway) fundingState(c *gin.Context) {
	market, err := strconv.ParseUint(c.Param("market"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	st, ok := g.perp.FundingState(perp.MarketID(market))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no_funding_round"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"market":      uint32(st.Market),
		"rate":        st.Rate,
		"markPrice":   uint64(st.Mark),
		"lastIndex":   st.LastIndex,
		"lastRoundAt": st.LastRoundAt,
		"totalDebt":   uintString(st.TotalDebt),
	})
}

// --- views ----------------------------------------------------------------

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
	Leverage    uint32 `json:"leverage"`
	Filled      string `json:"filled"`
	Status      string `json:"status"`
}

func orderToView(o *perp.Order) orderView {
	if o == nil {
		return orderView{}
	}
	v := orderView{
		ID: uint64(o.ID), Seq: uint64(o.Seq), Market: uint32(o.Market),
		Account: o.Account.Hex(), Side: o.Side.String(), Type: o.Type.String(),
		TIF: o.TIF.String(), Price: uint64(o.Price), Leverage: o.Leverage,
		Status: o.Status.String(),
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

func tradeToView(t perp.Trade) tradeView {
	v := tradeView{
		ID: uint64(t.ID), Market: uint32(t.Market), Price: uint64(t.Price),
		MakerOrder: uint64(t.MakerOrder), TakerOrder: uint64(t.TakerOrder),
		Maker: t.Maker.Hex(), Taker: t.Taker.Hex(), MakerSide: t.MakerSide.String(),
	}
	if t.Qty != nil {
		v.Qty = t.Qty.Dec()
	}
	if t.Notional != nil {
		v.Notional = t.Notional.Dec()
	}
	return v
}

func tradesToViews(ts []perp.Trade) []tradeView {
	out := make([]tradeView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tradeToView(t))
	}
	return out
}

type levelView struct {
	Price uint64 `json:"price"`
	Total string `json:"total"`
}

type depthView struct {
	Market uint32      `json:"market"`
	// Bids are resting longs (they bid base); asks are resting shorts.
	Bids []levelView `json:"bids"`
	Asks []levelView `json:"asks"`
}

func depthToView(m perp.MarketID, longs, shorts []perp.LevelView) depthView {
	v := depthView{Market: uint32(m)}
	for _, l := range longs {
		v.Bids = append(v.Bids, levelView{Price: uint64(l.Price), Total: uintString(l.Total)})
	}
	for _, l := range shorts {
		v.Asks = append(v.Asks, levelView{Price: uint64(l.Price), Total: uintString(l.Total)})
	}
	return v
}

// --- helpers --------------------------------------------------------------

func orderToPlaceRequest(account ledger.AccountID, o SignedOrder) (perp.PlaceRequest, error) {
	qty, err := parseUint(o.Qty)
	if err != nil {
		return perp.PlaceRequest{}, err
	}
	req := perp.PlaceRequest{
		Market: perp.MarketID(o.Market), Account: account,
		Side: perp.Side(o.Side), Type: perp.Type(o.Type), TIF: perp.TIF(o.TIF),
		Price: perp.Price(o.Price), Qty: qty, Leverage: o.Leverage,
	}
	// An absent cap is nil, not zero: the kernel distinguishes "no cap" from
	// "cap of zero" (the latter rejects every market order).
	if v, err := parseUint(o.MaxNotional); err != nil {
		return perp.PlaceRequest{}, err
	} else if !v.IsZero() {
		req.MaxNotional = v
	}
	return req, nil
}

// mapPerpError maps perp/ledger errors to (http code, error key).
func mapPerpError(err error) (int, string) {
	switch {
	case errors.Is(err, services.ErrPerpServiceDegraded):
		return http.StatusServiceUnavailable, "service_degraded"
	case errors.Is(err, ledger.ErrInsufficientBalance):
		return http.StatusUnprocessableEntity, "insufficient_balance"
	case errors.Is(err, perp.ErrBadRequest), errors.Is(err, perp.ErrUnknownMarket),
		errors.Is(err, perp.ErrAmountTooLarge):
		return http.StatusUnprocessableEntity, "order_rejected"
	case errors.Is(err, perp.ErrNotOwner):
		return http.StatusForbidden, "not_owner"
	case errors.Is(err, perp.ErrOrderNotFound):
		return http.StatusNotFound, "not_found"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

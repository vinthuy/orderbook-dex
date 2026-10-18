package perp

import (
	"fmt"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// PlaceRequest is an untrusted order request. Leverage is the user-selected
// leverage (<= market max) for opening/increasing a position.
type PlaceRequest struct {
	Market      MarketID
	Account     ledger.AccountID
	Side        Side
	Type        Type
	TIF         TIF
	Price       Price
	Qty         *uint256.Int
	MaxNotional *uint256.Int
	Leverage    uint32
}

// MarginRelease tells the service layer to unfreeze margin (quote).
type MarginRelease struct {
	Account ledger.AccountID
	Amount  *uint256.Int
}

// PnLSettlement tells the service layer to realize a profit (Deposit) or loss
// (Withdraw) in the quote asset when a position is reduced. TradeID is the
// producing fill's id, used as the idempotency key perp:pnl:<tradeID>.
type PnLSettlement struct {
	Market  MarketID
	TradeID TradeID
	Account ledger.AccountID
	Profit  *uint256.Int // quote; positive = credit, negative = debit (magnitude)
}

// Trade is one fill between a resting maker and an incoming taker.
type Trade struct {
	ID         TradeID
	Market     MarketID
	Price      Price
	Qty        *uint256.Int
	Notional   *uint256.Int
	MakerOrder OrderID
	TakerOrder OrderID
	Maker      ledger.AccountID
	Taker      ledger.AccountID
	MakerSide  Side
}

// Result is the outcome of Place/Cancel.
type Result struct {
	Order    *Order
	Trades   []Trade
	Releases []MarginRelease
	PnL      []PnLSettlement
}

// LevelView is a price-level snapshot for Book.
type LevelView struct {
	Price Price
	Total *uint256.Int
}

// Engine is a deterministic multi-market perpetual matching kernel.
type Engine struct {
	markets   []Market
	byID      map[MarketID]Market
	books     map[MarketID]*book
	all       map[OrderID]*Order
	positions map[MarketID]map[ledger.AccountID]*Position
	nextID    OrderID
	nextSeq   Seq
	nextTrade TradeID
}

func NewEngine(markets []Market) (*Engine, error) {
	e := &Engine{
		byID:      make(map[MarketID]Market, len(markets)),
		books:     make(map[MarketID]*book, len(markets)),
		all:       make(map[OrderID]*Order),
		positions: make(map[MarketID]map[ledger.AccountID]*Position),
	}
	seenSymbol := make(map[string]struct{}, len(markets))
	for _, m := range markets {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if _, dup := e.byID[m.ID]; dup {
			return nil, fmt.Errorf("perp: duplicate market id %d", m.ID)
		}
		if _, dup := seenSymbol[m.Symbol]; dup {
			return nil, fmt.Errorf("perp: duplicate market symbol %q", m.Symbol)
		}
		cp := m
		cp.MinQty = cloneInt(m.MinQty)
		cp.MaxQty = cloneInt(m.MaxQty)
		e.markets = append(e.markets, cp)
		e.byID[cp.ID] = cp
		e.books[cp.ID] = newBook(cp)
		e.positions[cp.ID] = make(map[ledger.AccountID]*Position)
		seenSymbol[cp.Symbol] = struct{}{}
	}
	return e, nil
}

func (e *Engine) Markets() []Market {
	out := make([]Market, len(e.markets))
	copy(out, e.markets)
	for i := range out {
		out[i].MinQty = cloneInt(out[i].MinQty)
		out[i].MaxQty = cloneInt(out[i].MaxQty)
	}
	return out
}

// Position returns a copy of an account's position in a market, and whether it
// exists.
func (e *Engine) Position(m MarketID, a ledger.AccountID) (*Position, bool) {
	byAcct := e.positions[m]
	if byAcct == nil {
		return nil, false
	}
	p, ok := byAcct[a]
	if !ok {
		return nil, false
	}
	return p.clone(), true
}

// OpenMargins returns the total frozen margin per (account, quote asset) that
// the service layer must hold: every position's Margin plus every resting
// order's reserved remainder. Without the order leg a restart would under-freeze
// resting orders, overstating available balance and making a later Cancel
// release more than is held (ErrHoldUnderflow).
//
// Margin is frozen in the market's quote asset, so two markets that share a
// quote asset aggregate into one hold bucket per account. Zero amounts are
// skipped so the map stays canonical.
func (e *Engine) OpenMargins() map[ledger.HoldKey]*uint256.Int {
	out := make(map[ledger.HoldKey]*uint256.Int)
	add := func(key ledger.HoldKey, amount *uint256.Int) {
		if amount == nil || amount.IsZero() {
			return
		}
		cur := out[key]
		if cur == nil {
			cur = uint256.NewInt(0)
			out[key] = cur
		}
		cur.Add(cur, amount)
	}
	for m, byAcct := range e.positions {
		mk := e.byID[m]
		for _, p := range byAcct {
			add(ledger.HoldKey{Account: p.Account, Asset: mk.Quote}, p.Margin)
		}
	}
	for _, o := range e.all {
		if !o.IsOpen() {
			continue
		}
		mk := e.byID[o.Market]
		add(ledger.HoldKey{Account: o.Account, Asset: mk.Quote}, o.reserved)
	}
	return out
}

// RequiredMargin computes the order-level freeze a request must pre-reserve to
// open/increase its position, without changing state. It validates exactly as
// Place does.
func (e *Engine) RequiredMargin(req PlaceRequest) (*uint256.Int, error) {
	m, err := e.validate(req)
	if err != nil {
		return nil, err
	}
	return e.orderMarginCeil(m, req)
}

// orderMarginCeil derives the order-level freeze. A limit order uses its price;
// a market order (price 0) uses its MaxNotional cap (spec §4.4).
func (e *Engine) orderMarginCeil(m Market, req PlaceRequest) (*uint256.Int, error) {
	if req.Type == TypeMarket {
		if req.MaxNotional == nil {
			return nil, fmt.Errorf("%w: market order requires MaxNotional", ErrBadRequest)
		}
		return m.marginCeilAmount(req.MaxNotional, req.Leverage)
	}
	return m.marginCeil(req.Qty, req.Price, req.Leverage)
}

func (e *Engine) validate(req PlaceRequest) (Market, error) {
	m, ok := e.byID[req.Market]
	if !ok {
		return Market{}, ErrUnknownMarket
	}
	if req.Side != SideLong && req.Side != SideShort {
		return Market{}, fmt.Errorf("%w: bad side %d", ErrBadRequest, req.Side)
	}
	if req.Type != TypeLimit && req.Type != TypeMarket {
		return Market{}, fmt.Errorf("%w: bad type %d", ErrBadRequest, req.Type)
	}
	if req.TIF != TIFGTC && req.TIF != TIFIOC {
		return Market{}, fmt.Errorf("%w: bad tif %d", ErrBadRequest, req.TIF)
	}
	if req.Type == TypeLimit {
		if req.Price == 0 {
			return Market{}, fmt.Errorf("%w: limit order requires price > 0", ErrBadRequest)
		}
	} else {
		// Market order: no order price, so margin is derived from MaxNotional,
		// which is therefore required and must be > 0. A non-zero price is
		// rejected so an attacker cannot depress the derived margin.
		if req.Price != 0 {
			return Market{}, fmt.Errorf("%w: market order must have price 0", ErrBadRequest)
		}
		if req.MaxNotional == nil || req.MaxNotional.Sign() <= 0 {
			return Market{}, fmt.Errorf("%w: market order requires MaxNotional > 0", ErrBadRequest)
		}
		if req.TIF == TIFGTC {
			return Market{}, fmt.Errorf("%w: market order cannot be GTC", ErrBadRequest)
		}
	}
	if req.Qty == nil || req.Qty.Sign() == 0 {
		return Market{}, fmt.Errorf("%w: qty must be > 0", ErrBadRequest)
	}
	if req.Leverage < 1 || req.Leverage > m.MaxLeverage {
		return Market{}, fmt.Errorf("%w: leverage %d not in [1,%d]", ErrBadRequest, req.Leverage, m.MaxLeverage)
	}
	if maxQty := m.maxQty(); req.Qty.Cmp(maxQty) > 0 {
		return Market{}, fmt.Errorf("%w: qty %s exceeds max %s", ErrAmountTooLarge, req.Qty, maxQty)
	}
	if m.MinQty != nil && req.Qty.Cmp(m.MinQty) < 0 {
		return Market{}, fmt.Errorf("%w: qty %s below min %s", ErrBadRequest, req.Qty, m.MinQty)
	}
	return m, nil
}

// Place validates, assigns ID/Seq, matches against the opposite side, updates
// positions, and returns the resulting margin releases and PnL settlements. A
// validation error returns before any mutation.
func (e *Engine) Place(req PlaceRequest) (Result, error) {
	m, err := e.validate(req)
	if err != nil {
		return Result{}, err
	}
	margin, err := e.orderMarginCeil(m, req)
	if err != nil {
		return Result{}, err
	}

	e.nextID++
	e.nextSeq++
	o := &Order{
		ID:          e.nextID,
		Seq:         e.nextSeq,
		Market:      m.ID,
		Account:     req.Account,
		Side:        req.Side,
		Type:        req.Type,
		TIF:         req.TIF,
		Price:       req.Price,
		Qty:         cloneInt(req.Qty),
		MaxNotional: cloneInt(req.MaxNotional),
		Leverage:    req.Leverage,
		Filled:      uint256.NewInt(0),
		Status:      StatusNew,
	}
	o.reserved = cloneInt(margin)
	e.all[o.ID] = o

	bk := e.books[m.ID]
	// Opposing side of the book: a long order matches resting shorts (asks);
	// a short order matches resting longs (bids).
	opp := bk.asks
	if o.Side == SideShort {
		opp = bk.bids
	}

	var (
		trades   []Trade
		releases []MarginRelease
		pnl      []PnLSettlement
		spent    = uint256.NewInt(0) // market-order quote already committed
	)

	for o.Remaining().Sign() > 0 {
		maker := opp.bestOrder()
		if maker == nil {
			break
		}
		price := maker.Price

		if o.Type == TypeLimit {
			if o.Side == SideLong && price > o.Price {
				break
			}
			if o.Side == SideShort && price < o.Price {
				break
			}
		}

		// STP: an account never trades with itself.
		if maker.Account == o.Account {
			bk.cancel(maker)
			maker.Status = StatusCancelled
			e.releaseResidual(maker, &releases)
			continue
		}

		qty := minUint(o.Remaining(), maker.Remaining())
		var notional *uint256.Int
		if o.Type == TypeMarket {
			notional = m.Notional(qty, price)
			if o.MaxNotional != nil {
				left := new(uint256.Int).Sub(o.MaxNotional, spent)
				if notional.Cmp(left) > 0 {
					allowed := new(uint256.Int).Mul(left, m.scale())
					allowed.Div(allowed, uint256.NewInt(uint64(price)))
					qty = minUint(qty, allowed)
					if qty.IsZero() {
						break
					}
					notional = m.Notional(qty, price)
				}
			}
		} else {
			notional = m.Notional(qty, price)
		}

		e.nextTrade++
		trades = append(trades, Trade{
			ID:         e.nextTrade,
			Market:     m.ID,
			Price:      price,
			Qty:        cloneInt(qty),
			Notional:   cloneInt(notional),
			MakerOrder: maker.ID,
			TakerOrder: o.ID,
			Maker:      maker.Account,
			Taker:      o.Account,
			MakerSide:  maker.Side,
		})

		o.Filled.Add(o.Filled, qty)
		bk.reduce(maker, qty)
		if maker.Remaining().IsZero() {
			maker.Status = StatusFilled
		} else {
			maker.Status = StatusPartiallyFilled
		}

		// Update the taker's position. The taker's own order's reserved margin
		// covers its opening leg; margin is derived from o.Price/o.Leverage.
		e.applyFill(m, o, qty, price, e.nextTrade, &releases, &pnl)
		// Update the maker's position. The maker's resting order's reserved
		// margin covers its opening leg, at its own price/leverage.
		e.applyFill(m, maker, qty, price, e.nextTrade, &releases, &pnl)

		if maker.Remaining().IsZero() {
			e.releaseResidual(maker, &releases)
		}

		if o.Type == TypeMarket {
			spent.Add(spent, notional)
		}
	}

	switch {
	case o.Remaining().IsZero():
		o.Status = StatusFilled
		e.releaseResidual(o, &releases)
	case o.Type == TypeMarket || o.TIF == TIFIOC:
		o.Status = StatusCancelled
		e.releaseResidual(o, &releases)
	default:
		if o.Filled.IsZero() {
			o.Status = StatusNew
		} else {
			o.Status = StatusPartiallyFilled
		}
		bk.add(o)
	}

	return Result{Order: o.clone(), Trades: trades, Releases: releases, PnL: pnl}, nil
}

// marginPrice is the price used for position-level margin: the order's own
// price for limit orders, or the fill price for market orders (whose Price is 0).
func marginPrice(o *Order, fillPrice Price) Price {
	if o.Type == TypeMarket {
		return fillPrice
	}
	return o.Price
}

// applyFill updates one account's position for a fill of qty base units at
// fillPrice, in the direction of the given order o (Long = buying base, Short =
// selling base). It transfers the opening leg's floor margin from the order's
// reserved freeze into the position, realizes PnL on the closing leg, and
// releases margin on closes. The order's leverage and its margin price
// (o.Price for limit orders, the fill price for market orders) drive margin,
// per spec §4.4; the fill price drives PnL and the entry price.
func (e *Engine) applyFill(m Market, o *Order, qty *uint256.Int, fillPrice Price, tradeID TradeID, releases *[]MarginRelease, pnl *[]PnLSettlement) {
	byAcct := e.positions[m.ID]
	pos := byAcct[o.Account]

	if pos == nil || pos.Side == o.Side {
		// Opening or increasing: transfer floor margin from the order's reserved
		// freeze to the position. SubOverflow must never fire because floor is
		// subadditive and the reserved amount was ceil(whole order) — but panic
		// loudly rather than silently wrap if it ever does.
		margin, err := m.marginFloor(qty, marginPrice(o, fillPrice), o.Leverage)
		if err != nil {
			panic("perp: applyFill margin: " + err.Error())
		}
		next, underflow := new(uint256.Int).SubOverflow(o.reserved, margin)
		if underflow {
			panic(fmt.Sprintf("perp: applyFill reserve underflow: order %d reserved %s < margin %s", o.ID, o.reserved, margin))
		}
		o.reserved = next

		addQtyPrice := new(uint256.Int).Mul(qty, uint256.NewInt(uint64(fillPrice)))

		if pos == nil {
			byAcct[o.Account] = &Position{
				Market:     m.ID,
				Account:    o.Account,
				Side:       o.Side,
				Size:       cloneInt(qty),
				EntryPrice: fillPrice,
				Margin:     cloneInt(margin),
				Leverage:   o.Leverage,
				qtyPrice:   cloneInt(addQtyPrice),
			}
			return
		}
		// Increase same side: exact quantity-weighted average entry price
		// floor(Σ(qty·price)/Σqty), accumulated in pos.qtyPrice so repeated
		// increases never drift from the integer average (spec §4.6.2).
		pos.qtyPrice.Add(pos.qtyPrice, addQtyPrice)
		pos.Size.Add(pos.Size, qty)
		pos.EntryPrice = Price(new(uint256.Int).Div(pos.qtyPrice, pos.Size).Uint64())
		pos.Margin.Add(pos.Margin, margin)
		pos.Leverage = o.Leverage
		return
	}

	// Reducing or flipping: opposite side. No order-reserved transfer here —
	// a close never consumes the order's own opening freeze.
	prevSize := cloneInt(pos.Size)
	closed := minUint(pos.Size, qty)

	// Reduce the cumulative qty·price proportionally so EntryPrice stays
	// consistent (floor(qtyPrice/size) unchanged) on a later increase.
	pos.qtyPrice.Sub(pos.qtyPrice, new(uint256.Int).Mul(closed, uint256.NewInt(uint64(pos.EntryPrice))))

	// Realize PnL on the closed portion against the entry price.
	entryNotional := m.Notional(closed, pos.EntryPrice)
	fillNotional := m.Notional(closed, fillPrice)
	var profit *uint256.Int
	if pos.Side == SideLong {
		profit = new(uint256.Int).Sub(fillNotional, entryNotional)
	} else {
		profit = new(uint256.Int).Sub(entryNotional, fillNotional)
	}
	*pnl = append(*pnl, PnLSettlement{Market: m.ID, TradeID: tradeID, Account: o.Account, Profit: profit})

	// Release margin proportionally to the closed size.
	release := new(uint256.Int).Mul(pos.Margin, closed)
	release.Div(release, prevSize)
	*releases = append(*releases, MarginRelease{Account: o.Account, Amount: cloneInt(release)})
	pos.Margin.Sub(pos.Margin, release)

	pos.Size.Sub(pos.Size, closed)
	if pos.Size.IsZero() {
		delete(byAcct, o.Account)
	}

	// Any remainder beyond the closed size flips the direction and opens a fresh
	// position, transferring floor margin from the order's reserved freeze.
	remainder := new(uint256.Int).Sub(qty, closed)
	if remainder.Sign() > 0 {
		margin, err := m.marginFloor(remainder, marginPrice(o, fillPrice), o.Leverage)
		if err != nil {
			panic("perp: applyFill flip margin: " + err.Error())
		}
		next, underflow := new(uint256.Int).SubOverflow(o.reserved, margin)
		if underflow {
			panic(fmt.Sprintf("perp: applyFill flip reserve underflow: order %d reserved %s < margin %s", o.ID, o.reserved, margin))
		}
		o.reserved = next
		byAcct[o.Account] = &Position{
			Market:     m.ID,
			Account:    o.Account,
			Side:       o.Side,
			Size:       cloneInt(remainder),
			EntryPrice: fillPrice,
			Margin:     cloneInt(margin),
			Leverage:   o.Leverage,
			qtyPrice:   new(uint256.Int).Mul(remainder, uint256.NewInt(uint64(fillPrice))),
		}
	}
}

// releaseResidual appends a release for an order's remaining reserved margin.
func (e *Engine) releaseResidual(o *Order, out *[]MarginRelease) {
	if o.reserved == nil || o.reserved.Sign() == 0 {
		return
	}
	*out = append(*out, MarginRelease{Account: o.Account, Amount: cloneInt(o.reserved)})
	o.reserved = uint256.NewInt(0)
}

// Cancel removes one of the caller's resting orders and releases its margin.
func (e *Engine) Cancel(m MarketID, account ledger.AccountID, orderID OrderID) (Result, error) {
	if _, ok := e.byID[m]; !ok {
		return Result{}, ErrUnknownMarket
	}
	o, ok := e.all[orderID]
	if !ok || o.Market != m || !o.IsOpen() {
		return Result{}, ErrOrderNotFound
	}
	if o.Account != account {
		return Result{}, ErrNotOwner
	}
	e.books[o.Market].cancel(o)
	o.Status = StatusCancelled
	var releases []MarginRelease
	e.releaseResidual(o, &releases)
	return Result{Order: o.clone(), Releases: releases}, nil
}

// Order returns a copy of a known order.
func (e *Engine) Order(orderID OrderID) (*Order, bool) {
	o, ok := e.all[orderID]
	if !ok {
		return nil, false
	}
	return o.clone(), true
}

// Book returns up to depth price levels per side (longs descending, shorts
// ascending).
func (e *Engine) Book(m MarketID, depth int) (longs, shorts []LevelView) {
	bk, ok := e.books[m]
	if !ok || depth <= 0 {
		return nil, nil
	}
	return levelsOf(bk.bids, depth), levelsOf(bk.asks, depth)
}

func levelsOf(s *side, depth int) []LevelView {
	n := len(s.prices)
	if n > depth {
		n = depth
	}
	out := make([]LevelView, 0, n)
	for _, p := range s.prices[:n] {
		out = append(out, LevelView{Price: p, Total: cloneInt(s.levels[p].total)})
	}
	return out
}

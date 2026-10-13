package matching

import (
	"errors"
	"fmt"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Sentinel errors. Callers should use errors.Is against these.
var (
	ErrUnknownMarket = errors.New("matching: unknown market")
	ErrBadRequest    = errors.New("matching: invalid order")
	ErrOrderNotFound = errors.New("matching: order not found")
	ErrNotOwner      = errors.New("matching: not order owner")
	// ErrAmountTooLarge reports that a request would overflow the uint256
	// arithmetic the kernel uses to compute notional amounts, or that its qty
	// exceeds the market's bound. It is a rejection, never a wrap.
	ErrAmountTooLarge = errors.New("matching: notional overflows")
)

// Trade is one fill between a resting maker and an incoming taker.
type Trade struct {
	ID         TradeID
	Market     MarketID
	Price      Price
	Qty        *uint256.Int // base
	Notional   *uint256.Int // floor(Qty*Price/10^BaseDecimals), quote
	MakerOrder OrderID
	TakerOrder OrderID
	Maker      ledger.AccountID
	Taker      ledger.AccountID
	MakerSide  Side
}

// PlaceRequest is an untrusted order request. Qty and MaxNotional are inputs;
// the engine copies them and never retains the caller's pointers.
type PlaceRequest struct {
	Market      MarketID
	Account     ledger.AccountID
	Side        Side
	Type        Type
	TIF         TIF
	Price       Price
	Qty         *uint256.Int
	MaxNotional *uint256.Int
}

// HoldRelease tells the caller to unfreeze Amount of Asset for Account. One
// fill produces one release per leg; the two legs of a trade are never merged
// into a single entry.
type HoldRelease struct {
	Account ledger.AccountID
	Asset   ledger.AssetID
	Amount  *uint256.Int
}

// Result is the outcome of Place/Cancel.
type Result struct {
	Order    *Order
	Trades   []Trade
	Releases []HoldRelease
}

// LevelView is a price-level snapshot for Book.
type LevelView struct {
	Price Price
	Total *uint256.Int
}

// Engine is a deterministic multi-market matching kernel. It owns no I/O and no
// clock: replaying the same instructions into a fresh Engine yields the same
// order IDs, trade IDs, trades and book.
type Engine struct {
	markets   []Market
	byID      map[MarketID]Market
	books     map[MarketID]*book
	all       map[OrderID]*Order // every order ever created, open or closed
	nextID    OrderID
	nextSeq   Seq
	nextTrade TradeID
}

// NewEngine validates and installs the market set. It fails startup on any
// invalid, duplicate-ID or duplicate-symbol market rather than silently
// skipping it.
func NewEngine(markets []Market) (*Engine, error) {
	e := &Engine{
		byID:  make(map[MarketID]Market, len(markets)),
		books: make(map[MarketID]*book, len(markets)),
		all:   make(map[OrderID]*Order),
	}
	seenSymbol := make(map[string]struct{}, len(markets))
	for _, m := range markets {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if _, dup := e.byID[m.ID]; dup {
			return nil, fmt.Errorf("matching: duplicate market id %d", m.ID)
		}
		if _, dup := seenSymbol[m.Symbol]; dup {
			return nil, fmt.Errorf("matching: duplicate market symbol %q", m.Symbol)
		}
		cp := m
		cp.MinQty = cloneInt(m.MinQty)
		cp.MaxQty = cloneInt(m.MaxQty)
		e.markets = append(e.markets, cp)
		e.byID[cp.ID] = cp
		e.books[cp.ID] = newBook(cp)
		seenSymbol[cp.Symbol] = struct{}{}
	}
	return e, nil
}

// Markets returns a copy of the configured markets, with MinQty detached so a
// caller cannot mutate kernel state.
func (e *Engine) Markets() []Market {
	out := make([]Market, len(e.markets))
	copy(out, e.markets)
	for i := range out {
		out[i].MinQty = cloneInt(out[i].MinQty)
		out[i].MaxQty = cloneInt(out[i].MaxQty)
	}
	return out
}

// RequiredReserve computes the first freeze for req without changing any state:
// limit buy -> (quote, ceil(Qty*Price)); market buy -> (quote, MaxNotional);
// any sell -> (base, Qty). It validates the request exactly as Place does, and
// it derives the amount with the same checked arithmetic, so the freeze it
// reports is exactly the freeze Place charges.
func (e *Engine) RequiredReserve(req PlaceRequest) (ledger.AssetID, *uint256.Int, error) {
	m, err := e.validate(req)
	if err != nil {
		return 0, nil, err
	}
	return e.reserveFor(m, req)
}

// reserveFor derives the first freeze for an already-validated request. It is
// the single source of truth for the reserve: validate, RequiredReserve and
// Place all call it, so a freeze cannot be validated with one arithmetic and
// charged with another. A limit buy's notional is computed with checked
// arithmetic; overflow is ErrAmountTooLarge, never a wrapped reserve.
func (e *Engine) reserveFor(m Market, req PlaceRequest) (ledger.AssetID, *uint256.Int, error) {
	if req.Side == SideBuy {
		if req.Type == TypeMarket {
			return m.Quote, cloneInt(req.MaxNotional), nil
		}
		need, err := m.notionalChecked(req.Qty, req.Price, true)
		if err != nil {
			return 0, nil, err
		}
		return m.Quote, need, nil
	}
	return m.Base, cloneInt(req.Qty), nil
}

// validate returns the market for a legal request, or a sentinel error. It is
// pure: a rejected request must leave the book, the id/seq counters and the
// freeze state untouched.
func (e *Engine) validate(req PlaceRequest) (Market, error) {
	m, ok := e.byID[req.Market]
	if !ok {
		return Market{}, ErrUnknownMarket
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return Market{}, fmt.Errorf("%w: bad side %d", ErrBadRequest, req.Side)
	}
	if req.Type != TypeLimit && req.Type != TypeMarket {
		return Market{}, fmt.Errorf("%w: bad type %d", ErrBadRequest, req.Type)
	}
	if req.TIF != TIFGTC && req.TIF != TIFIOC {
		return Market{}, fmt.Errorf("%w: bad tif %d", ErrBadRequest, req.TIF)
	}
	switch req.Type {
	case TypeLimit:
		if req.Price == 0 {
			return Market{}, fmt.Errorf("%w: limit order requires price > 0", ErrBadRequest)
		}
	case TypeMarket:
		if req.TIF == TIFGTC {
			return Market{}, fmt.Errorf("%w: market order cannot be GTC", ErrBadRequest)
		}
		if req.Side == SideBuy && (req.MaxNotional == nil || req.MaxNotional.Sign() == 0) {
			return Market{}, fmt.Errorf("%w: market buy requires max notional > 0", ErrBadRequest)
		}
	}
	if req.Qty == nil || req.Qty.Sign() == 0 {
		return Market{}, fmt.Errorf("%w: qty must be > 0", ErrBadRequest)
	}
	// Bound Qty so qty*price can never overflow uint256: the freeze derived from
	// an overflowing notional would not cover the fill.
	if maxQty := m.maxQty(); req.Qty.Cmp(maxQty) > 0 {
		return Market{}, fmt.Errorf("%w: qty %s exceeds max %s", ErrAmountTooLarge, req.Qty, maxQty)
	}
	if m.MinQty != nil && req.Qty.Cmp(m.MinQty) < 0 {
		return Market{}, fmt.Errorf("%w: qty %s below min %s", ErrBadRequest, req.Qty, m.MinQty)
	}
	if req.Type == TypeLimit && req.Side == SideBuy {
		// Compute the required reserve with checked arithmetic so an
		// overflowing notional is a rejection, not a quietly wrapped freeze.
		need, err := m.notionalChecked(req.Qty, req.Price, true)
		if err != nil {
			return Market{}, err
		}
		if req.MaxNotional != nil && req.MaxNotional.Cmp(need) < 0 {
			return Market{}, fmt.Errorf("%w: max notional %s below required %s", ErrBadRequest, req.MaxNotional, need)
		}
	}
	return m, nil
}

// Place validates, assigns ID/Seq, matches against the opposite side and either
// rests (GTC) or cancels the remainder (IOC / market). A validation error
// returns before any counter or book mutation, with no releases.
func (e *Engine) Place(req PlaceRequest) (Result, error) {
	m, err := e.validate(req)
	if err != nil {
		return Result{}, err
	}
	// Derive the freeze before touching any counter: the amount validated is
	// exactly the amount charged.
	_, reserve, err := e.reserveFor(m, req)
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
		Filled:      uint256.NewInt(0),
		Status:      StatusNew,
	}
	if o.Type == TypeMarket {
		o.TIF = TIFIOC
	}
	o.reserved = cloneInt(reserve)
	e.all[o.ID] = o

	bk := e.books[m.ID]
	opp := bk.asks
	if o.Side == SideSell {
		opp = bk.bids
	}

	var (
		trades   []Trade
		releases []HoldRelease
		spent    = uint256.NewInt(0) // market-buy quote already committed
	)

	for o.Remaining().Sign() > 0 {
		maker := opp.bestOrder()
		if maker == nil {
			break
		}
		price := maker.Price

		if o.Type == TypeLimit {
			if o.Side == SideBuy && price > o.Price {
				break
			}
			if o.Side == SideSell && price < o.Price {
				break
			}
		}

		// STP: an account never trades with itself. Cancel the older resting
		// order and keep matching; the incoming order is never the one removed.
		if maker.Account == o.Account {
			bk.cancel(maker)
			maker.Status = StatusCancelled
			e.releaseResidual(m, maker, &releases)
			continue
		}

		qty := minUint(o.Remaining(), maker.Remaining())
		var notional *uint256.Int
		if o.Type == TypeMarket && o.Side == SideBuy {
			// qty <= Market.MaxQty (validate bounds it) and price is a uint64,
			// so this Notional cannot overflow: qty*price < 2^128 * 2^64 == 2^192.
			notional = m.Notional(qty, price)
			left := new(uint256.Int).Sub(o.MaxNotional, spent)
			if notional.Cmp(left) > 0 {
				// The budget binds. Here left < notional+1 <= qty*price/scale + 1,
				// and qty <= MaxQty, so left*scale <= qty*price + scale is
				// provably within uint256: the cap cannot wrap.
				allowed := new(uint256.Int).Mul(left, m.scale())
				allowed.Div(allowed, uint256.NewInt(uint64(price)))
				qty = minUint(qty, allowed)
				if qty.IsZero() {
					break
				}
				notional = m.Notional(qty, price)
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
		// reduce removes a fully-filled maker from the book, but the status
		// transition is the engine's job.
		if maker.Remaining().IsZero() {
			maker.Status = StatusFilled
		} else {
			maker.Status = StatusPartiallyFilled
		}

		// One release per leg, in taker-then-maker order.
		e.consumeReserve(m, o, qty, notional, &releases)
		e.consumeReserve(m, maker, qty, notional, &releases)

		// A maker that just left the book must hand back whatever freeze is
		// left. A resting BUY reserves ceil(qty*price/scale) while each fill
		// deducts floor(...), so without this the rounding remainder stays
		// frozen forever. This must run AFTER consumeReserve: releasing first
		// would drop the freeze and then underflow the deduction.
		if maker.Remaining().IsZero() {
			e.releaseResidual(m, maker, &releases)
		}

		if o.Type == TypeMarket && o.Side == SideBuy {
			spent.Add(spent, notional)
		}
	}

	switch {
	case o.Remaining().IsZero():
		o.Status = StatusFilled
		e.releaseResidual(m, o, &releases)
	case o.Type == TypeMarket || o.TIF == TIFIOC:
		o.Status = StatusCancelled
		e.releaseResidual(m, o, &releases)
	default:
		if o.Filled.IsZero() {
			o.Status = StatusNew
		} else {
			o.Status = StatusPartiallyFilled
		}
		bk.add(o)
	}

	return Result{Order: o.clone(), Trades: trades, Releases: releases}, nil
}

// Cancel removes one of the caller's resting orders and releases its remaining
// freeze. Another account's order is ErrNotOwner; an unknown or already-closed
// order is ErrOrderNotFound.
func (e *Engine) Cancel(market MarketID, account ledger.AccountID, orderID OrderID) (Result, error) {
	if _, ok := e.byID[market]; !ok {
		return Result{}, ErrUnknownMarket
	}
	o, ok := e.all[orderID]
	if !ok || o.Market != market || !o.IsOpen() {
		return Result{}, ErrOrderNotFound
	}
	if o.Account != account {
		return Result{}, ErrNotOwner
	}
	e.books[o.Market].cancel(o)
	o.Status = StatusCancelled
	var releases []HoldRelease
	e.releaseResidual(e.byID[o.Market], o, &releases)
	return Result{Order: o.clone(), Releases: releases}, nil
}

// Order returns a copy of a known order (open or closed).
func (e *Engine) Order(orderID OrderID) (*Order, bool) {
	o, ok := e.all[orderID]
	if !ok {
		return nil, false
	}
	return o.clone(), true
}

// OpenReserves aggregates the freezes of every open order per (account, asset).
// It is what the service layer feeds Holds.Reconcile after replaying a WAL.
func (e *Engine) OpenReserves() map[ledger.HoldKey]*uint256.Int {
	out := make(map[ledger.HoldKey]*uint256.Int)
	for _, o := range e.all {
		if !o.IsOpen() || o.reserved.Sign() == 0 {
			continue
		}
		k := ledger.HoldKey{Account: o.Account, Asset: o.holdAsset(e.byID[o.Market])}
		if cur, ok := out[k]; ok {
			out[k] = new(uint256.Int).Add(cur, o.reserved)
			continue
		}
		out[k] = cloneInt(o.reserved)
	}
	return out
}

// Book returns up to depth price levels per side, best price first (bids
// descending, asks ascending). A non-positive depth yields empty views.
func (e *Engine) Book(market MarketID, depth int) (bids, asks []LevelView) {
	bk, ok := e.books[market]
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
	if n == 0 {
		return nil
	}
	out := make([]LevelView, 0, n)
	for _, p := range s.prices[:n] {
		out = append(out, LevelView{Price: p, Total: cloneInt(s.levels[p].total)})
	}
	return out
}

// NextSeq returns the sequence number the next Place will receive. It exists
// for transparency and tests (e.g. deterministic replay comparison).
func (e *Engine) NextSeq() Seq { return e.nextSeq + 1 }

// consumeReserve shrinks an order's freeze by the leg it just gave up (quote
// notional for buys, base qty for sells) and reports it for release. It uses
// checked subtraction: a silent wrap here is exactly what corrupted a reserve
// and drove holds.Release into underflow, so an underflow panics loudly instead
// of poisoning the freeze state. It is unreachable after validate bounds Qty.
func (e *Engine) consumeReserve(m Market, o *Order, qty, notional *uint256.Int, out *[]HoldRelease) {
	amount := notional
	if o.Side == SideSell {
		amount = qty
	}
	if amount == nil || amount.IsZero() {
		return
	}
	if o.reserved == nil {
		panic(fmt.Sprintf("matching: consumeReserve: order %d has nil reserve", o.ID))
	}
	next, underflow := new(uint256.Int).SubOverflow(o.reserved, amount)
	if underflow {
		panic(fmt.Sprintf("matching: consumeReserve underflow: order %d reserved %s < amount %s (internal invariant violation)",
			o.ID, o.reserved, amount))
	}
	o.reserved = next
	*out = append(*out, HoldRelease{Account: o.Account, Asset: o.holdAsset(m), Amount: cloneInt(amount)})
}

// releaseResidual releases whatever freeze is left when an order leaves the
// book, including the buy-side ceil rounding remainder.
func (e *Engine) releaseResidual(m Market, o *Order, out *[]HoldRelease) {
	if o.reserved == nil || o.reserved.Sign() == 0 {
		return
	}
	*out = append(*out, HoldRelease{Account: o.Account, Asset: o.holdAsset(m), Amount: cloneInt(o.reserved)})
	o.reserved = uint256.NewInt(0)
}

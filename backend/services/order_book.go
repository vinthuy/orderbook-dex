package services

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
)

// Event topics published by the OrderBookService (frozen by the plan).
const (
	TopicOrderBookOrder    = "orderbook.order"
	TopicOrderBookTrade    = "orderbook.trade"
	TopicOrderBookDegraded = "orderbook.degraded"
)

// ErrServiceDegraded is returned by Place/Cancel once a ledger apply has
// failed. At that point the book has advanced but the ledger has not, so the
// service refuses all further instructions until a restart replays the
// instruction WAL and reconciles the ledger against it (spec §6.5).
var ErrServiceDegraded = errors.New("services: order book degraded, restart required")

// OrderEvent is published on TopicOrderBookOrder for every accepted order.
type OrderEvent struct {
	Order  *matching.Order
	Status matching.Status
}

// TradeEvent is published on TopicOrderBookTrade for every fill.
type TradeEvent struct {
	Trade matching.Trade
}

// DegradedEvent is published on TopicOrderBookDegraded when the service enters
// fatal degradation.
type DegradedEvent struct {
	Err error
}

// OrderBookService is the host around the pure matching kernel. It owns the
// side effects the kernel deliberately refuses: freezing via ledger.Holds,
// durable instruction logging, ledger settlement, and event publication.
//
// Order of operations is the fund-safety property and must not be reordered:
//
//	RequiredReserve -> holds.Reserve -> journal.Append -> engine.Place ->
//	ledger.Apply (per trade) -> holds.Release -> publish
//
// Freezing first means a rejected order leaves the book untouched; journalling
// before mutation means a crash still has the instruction that caused it; and
// settling the ledger strictly before releasing freezes means the available
// balance is never inflated in the window between the two.
type OrderBookService struct {
	engine  *matching.Engine
	ledger  *ledger.Ledger
	holds   *ledger.Holds
	journal *Journal
	bus     *EventBus

	markets map[matching.MarketID]matching.Market

	mu       sync.Mutex
	fatalErr error

	// beforeRelease, when non-nil, runs after every fill has been settled in the
	// ledger and before any freeze is released. It exists so a test can observe
	// the mid-operation available balance: the ordering guarantee is that this
	// seam never OVERSTATES availability, because the ledger has already been
	// debited while the freeze has not yet been returned. Swapping the two steps
	// would inflate availability by the released amount and let a withdrawal
	// race a transfer into a half-settled trade.
	beforeRelease func()

	interval time.Duration
	run      runner
}

// NewOrderBookService builds the kernel from markets and immediately replays
// the instruction journal so a restart heals the book, the freeze state, and
// any ledger settlement the crash left undone (spec §6.4). The ledger and holds
// are shared with the settlement stack, so withdrawals and orders contend for
// the same available balance.
func NewOrderBookService(markets []matching.Market, l *ledger.Ledger, holds *ledger.Holds, journal *Journal, bus *EventBus) (*OrderBookService, error) {
	if l == nil {
		return nil, errors.New("services: order book requires a ledger")
	}
	if holds == nil {
		return nil, errors.New("services: order book requires holds")
	}
	if journal == nil {
		return nil, errors.New("services: order book requires a journal")
	}
	if bus == nil {
		bus = NewEventBus()
	}
	engine, err := matching.NewEngine(markets)
	if err != nil {
		return nil, err
	}
	byID := make(map[matching.MarketID]matching.Market, len(markets))
	for _, m := range engine.Markets() {
		byID[m.ID] = m
	}
	s := &OrderBookService{
		engine:   engine,
		ledger:   l,
		holds:    holds,
		journal:  journal,
		bus:      bus,
		markets:  byID,
		interval: 30 * time.Second,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

// Name implements services.Service.
func (s *OrderBookService) Name() string { return "OrderBookService" }

// Initialize implements services.Service. The order book carries no tunable
// configuration beyond its constructor; the service is kept in the registry so
// it shares the stack's Start/Stop lifecycle.
func (s *OrderBookService) Initialize(cfg Config) error { return nil }

// Start implements services.Service. The book's durable recovery runs once in
// the constructor; the runner keeps the restart semantics identical to the
// other stack services (deposit indexer, anchor) and leaves room for future
// periodic work such as snapshots.
func (s *OrderBookService) Start() error {
	s.run.start(s.interval, func() {})
	return nil
}

// Stop implements services.Service. It is idempotent.
func (s *OrderBookService) Stop() error {
	s.run.stop()
	return nil
}

// Close stops the runner and releases the instruction journal's file handle.
// It is idempotent.
func (s *OrderBookService) Close() error {
	s.run.stop()
	return s.journal.Close()
}

// Engine exposes the kernel for read-only queries (book depth, order lookup).
func (s *OrderBookService) Engine() *matching.Engine { return s.engine }

// Holds exposes the freeze state this service maintains.
func (s *OrderBookService) Holds() *ledger.Holds { return s.holds }

// Place freezes the required balance, journals the instruction, matches, settles
// every fill in the ledger, releases residual freezes, and publishes events — in
// that order. A validation or funding failure leaves the book untouched.
func (s *OrderBookService) Place(req matching.PlaceRequest) (matching.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return matching.Result{}, s.fatalErr
	}

	asset, required, err := s.engine.RequiredReserve(req)
	if err != nil {
		return matching.Result{}, err
	}

	if err := s.holds.Reserve(req.Account, asset, required); err != nil {
		return matching.Result{}, err
	}

	if err := s.journal.Append(PlaceEntry(req)); err != nil {
		_ = s.holds.Release(req.Account, asset, required)
		return matching.Result{}, fmt.Errorf("services: order book: append journal: %w", err)
	}

	res, err := s.engine.Place(req)
	if err != nil || res.Order == nil || res.Order.Status == matching.StatusRejected {
		_ = s.holds.Release(req.Account, asset, required)
		if err == nil {
			err = fmt.Errorf("%w: order rejected", matching.ErrBadRequest)
		}
		return matching.Result{}, err
	}

	// Ledger settlement strictly BEFORE the freeze releases. Reversing the two
	// would briefly overstate the available balance and let a withdrawal race a
	// transfer into a half-settled trade.
	if err := s.settleTrades(res.Trades); err != nil {
		return matching.Result{}, s.degrade(err)
	}
	if s.beforeRelease != nil {
		s.beforeRelease()
	}
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, r.Asset, r.Amount); err != nil {
			return matching.Result{}, s.degrade(err)
		}
	}

	s.publish(res)
	return res, nil
}

// Cancel journals the instruction, removes the caller's resting order, releases
// its residual freeze, and publishes the order event. It never touches the
// ledger: a cancel moves no funds.
func (s *OrderBookService) Cancel(market matching.MarketID, account ledger.AccountID, orderID matching.OrderID) (matching.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return matching.Result{}, s.fatalErr
	}

	if err := s.journal.Append(CancelEntry(market, account, orderID)); err != nil {
		return matching.Result{}, fmt.Errorf("services: order book: append journal: %w", err)
	}

	res, err := s.engine.Cancel(market, account, orderID)
	if err != nil {
		return matching.Result{}, err
	}
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, r.Asset, r.Amount); err != nil {
			return matching.Result{}, s.degrade(err)
		}
	}

	s.publish(res)
	return res, nil
}

// settleTrades applies each fill's two zero-sum transfers in a single atomic
// Apply, so a trade is never half-recorded.
func (s *OrderBookService) settleTrades(trades []matching.Trade) error {
	for _, t := range trades {
		if err := s.ledger.Apply(s.tradeOps(t)...); err != nil {
			return err
		}
	}
	return nil
}

// tradeOps encodes one fill exactly as spec §6.3 requires: base seller->buyer
// and quote buyer->seller, keyed by the deterministic trade id so a replay of
// the same fill is skipped rather than paid twice.
func (s *OrderBookService) tradeOps(t matching.Trade) []ledger.Op {
	m := s.markets[t.Market]
	buyer, seller := t.Taker, t.Maker
	if t.MakerSide == matching.SideBuy {
		buyer, seller = t.Maker, t.Taker
	}
	base := ledger.TransferOp{
		From:   seller,
		To:     buyer,
		Asset:  m.Base,
		Amount: new(uint256.Int).Set(t.Qty),
		Ref:    tradeBaseRef(t.ID),
	}
	quote := ledger.TransferOp{
		From:   buyer,
		To:     seller,
		Asset:  m.Quote,
		Amount: new(uint256.Int).Set(t.Notional),
		Ref:    tradeQuoteRef(t.ID),
	}
	return []ledger.Op{base, quote}
}

func tradeBaseRef(id matching.TradeID) string  { return fmt.Sprintf("trade:%d:base", id) }
func tradeQuoteRef(id matching.TradeID) string { return fmt.Sprintf("trade:%d:quote", id) }

// publish emits the order event and one trade event per fill.
func (s *OrderBookService) publish(res matching.Result) {
	if res.Order != nil {
		_ = s.bus.Publish(Event{
			Topic: TopicOrderBookOrder,
			Data:  OrderEvent{Order: res.Order, Status: res.Order.Status},
		})
	}
	for _, t := range res.Trades {
		_ = s.bus.Publish(Event{Topic: TopicOrderBookTrade, Data: TradeEvent{Trade: t}})
	}
}

// degrade latches the fatal error and announces it. The cause is wrapped so a
// caller can both match ErrServiceDegraded and read why.
func (s *OrderBookService) degrade(cause error) error {
	if s.fatalErr == nil {
		s.fatalErr = fmt.Errorf("%w: %v", ErrServiceDegraded, cause)
	}
	_ = s.bus.Publish(Event{Topic: TopicOrderBookDegraded, Data: DegradedEvent{Err: cause}})
	return s.fatalErr
}

// recover replays the instruction WAL exactly as the service originally ran it,
// minus the incremental holds.Reserve (the freeze is set once from the kernel's
// derived reserves at the end). Ledger settlement is applied per fill; because
// it is keyed by trade id, fills already settled are skipped and fills the
// crash left unsettled are caught up — the self-healing property.
func (s *OrderBookService) recover() error {
	entries, err := s.journal.Replay()
	if err != nil {
		return fmt.Errorf("services: order book: replay journal: %w", err)
	}
	for _, e := range entries {
		switch e.Kind {
		case kindPlace:
			if e.Place == nil {
				return fmt.Errorf("services: order book: journal place entry %d has no payload", e.Seq)
			}
			res, err := s.engine.Place(e.Place.Request())
			if err != nil {
				continue // deterministically rejected; never took effect
			}
			if err := s.settleTrades(res.Trades); err != nil {
				return fmt.Errorf("services: order book: replay settle: %w", err)
			}
		case kindCancel:
			if e.Cancel == nil {
				return fmt.Errorf("services: order book: journal cancel entry %d has no payload", e.Seq)
			}
			if _, err := s.engine.Cancel(e.Cancel.Market, e.Cancel.Account, e.Cancel.OrderID); err != nil {
				continue // unknown/closed order: replay of a rejected cancel
			}
		default:
			return fmt.Errorf("services: order book: unknown journal kind %q", e.Kind)
		}
	}
	return s.holds.Reconcile(s.engine.OpenReserves())
}

// ParseMarkets parses the ORDERBOOK_MARKETS string (spec §7): markets separated
// by ';', each "ID:SYMBOL:BASE_ASSET:QUOTE_ASSET:BASE_DECIMALS[:MIN_QTY]".
// Any malformed market is a hard error so the process fails to start rather
// than silently trading a subset.
func ParseMarkets(spec string) ([]matching.Market, error) {
	var out []matching.Market
	for _, raw := range strings.Split(spec, ";") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		m, err := parseMarket(part)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("services: ORDERBOOK_MARKETS contains no markets")
	}
	return out, nil
}

func parseMarket(s string) (matching.Market, error) {
	fields := strings.Split(s, ":")
	if len(fields) < 5 || len(fields) > 6 {
		return matching.Market{}, fmt.Errorf("services: market %q: want ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS[:MIN_QTY]", s)
	}
	id, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
	if err != nil {
		return matching.Market{}, fmt.Errorf("services: market %q: bad id: %w", s, err)
	}
	symbol := strings.TrimSpace(fields[1])
	base, err := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 32)
	if err != nil {
		return matching.Market{}, fmt.Errorf("services: market %q: bad base asset: %w", s, err)
	}
	quote, err := strconv.ParseUint(strings.TrimSpace(fields[3]), 10, 32)
	if err != nil {
		return matching.Market{}, fmt.Errorf("services: market %q: bad quote asset: %w", s, err)
	}
	decimals, err := strconv.ParseUint(strings.TrimSpace(fields[4]), 10, 8)
	if err != nil {
		return matching.Market{}, fmt.Errorf("services: market %q: bad base decimals: %w", s, err)
	}
	var minQty *uint256.Int
	if len(fields) == 6 && strings.TrimSpace(fields[5]) != "" {
		minQty, err = uint256.FromDecimal(strings.TrimSpace(fields[5]))
		if err != nil {
			return matching.Market{}, fmt.Errorf("services: market %q: bad min qty: %w", s, err)
		}
	}
	m := matching.Market{
		ID:           matching.MarketID(id),
		Symbol:       symbol,
		Base:         ledger.AssetID(base),
		Quote:        ledger.AssetID(quote),
		BaseDecimals: uint8(decimals),
		MinQty:       minQty,
	}
	if err := m.Validate(); err != nil {
		return matching.Market{}, err
	}
	return m, nil
}

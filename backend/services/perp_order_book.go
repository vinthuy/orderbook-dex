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
	"web3-amm-dex-backend/perp"
)

const (
	TopicPerpTrade    = "perp.trade"
	TopicPerpPosition = "perp.position"
	TopicPerpDegraded = "perp.degraded"

	// perpRunInterval is the poll interval for the runner. 6a has no periodic
	// work, so the runner only keeps the restart semantics identical to the
	// other stack services; the interval is otherwise unused.
	perpRunInterval = 30 * time.Second
)

var ErrPerpServiceDegraded = errors.New("services: perp order book degraded, restart required")

// PerpOrderBookService hosts the perp kernel: it freezes quote margin, journals
// instructions, matches, realizes PnL into the ledger, releases margin, and
// publishes events — in that order.
type PerpOrderBookService struct {
	engine  *perp.Engine
	ledger  *ledger.Ledger
	holds   *ledger.Holds
	journal *PerpJournal
	bus     *EventBus
	markets map[perp.MarketID]perp.Market

	mu       sync.Mutex
	fatalErr error
	run      runner
}

func NewPerpOrderBookService(markets []perp.Market, l *ledger.Ledger, holds *ledger.Holds, journal *PerpJournal, bus *EventBus) (*PerpOrderBookService, error) {
	if l == nil {
		return nil, errors.New("services: perp order book requires a ledger")
	}
	if holds == nil {
		return nil, errors.New("services: perp order book requires holds")
	}
	if journal == nil {
		return nil, errors.New("services: perp order book requires a journal")
	}
	if bus == nil {
		bus = NewEventBus()
	}
	engine, err := perp.NewEngine(markets)
	if err != nil {
		return nil, err
	}
	byID := make(map[perp.MarketID]perp.Market, len(markets))
	for _, m := range engine.Markets() {
		byID[m.ID] = m
	}
	s := &PerpOrderBookService{
		engine: engine, ledger: l, holds: holds, journal: journal, bus: bus, markets: byID,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PerpOrderBookService) Name() string { return "PerpOrderBookService" }

func (s *PerpOrderBookService) Initialize(cfg Config) error { return nil }

func (s *PerpOrderBookService) Start() error {
	s.run.start(perpRunInterval, func() {})
	return nil
}

func (s *PerpOrderBookService) Stop() error {
	s.run.stop()
	return nil
}

func (s *PerpOrderBookService) Close() error {
	s.run.stop()
	return s.journal.Close()
}

func (s *PerpOrderBookService) Engine() *perp.Engine { return s.engine }
func (s *PerpOrderBookService) Holds() *ledger.Holds { return s.holds }

// Place freezes the order-level margin, journals the instruction, matches,
// realizes PnL into the ledger, releases margin, and publishes events — in that
// order. A validation or funding failure leaves positions untouched.
func (s *PerpOrderBookService) Place(req perp.PlaceRequest) (perp.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return perp.Result{}, s.fatalErr
	}

	required, err := s.engine.RequiredMargin(req)
	if err != nil {
		return perp.Result{}, err
	}
	quote := s.markets[req.Market].Quote
	if err := s.holds.Reserve(req.Account, quote, required); err != nil {
		return perp.Result{}, err
	}

	if err := s.journal.AppendPlace(req); err != nil {
		_ = s.holds.Release(req.Account, quote, required)
		return perp.Result{}, fmt.Errorf("services: perp: append journal: %w", err)
	}

	res, err := s.engine.Place(req)
	if err != nil || res.Order == nil || res.Order.Status == perp.StatusRejected {
		_ = s.holds.Release(req.Account, quote, required)
		if err == nil {
			err = fmt.Errorf("%w: order rejected", perp.ErrBadRequest)
		}
		return perp.Result{}, err
	}

	// Realize PnL strictly before releasing margin (spec §5.2 order).
	if err := s.settlePnL(res.PnL); err != nil {
		return perp.Result{}, s.degrade(err)
	}
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, quote, r.Amount); err != nil {
			return perp.Result{}, s.degrade(err)
		}
	}

	s.publish(res)
	return res, nil
}

// Cancel journals the instruction, removes the caller's resting order, releases
// its residual margin, and publishes the position event.
func (s *PerpOrderBookService) Cancel(m perp.MarketID, a ledger.AccountID, id perp.OrderID) (perp.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return perp.Result{}, s.fatalErr
	}
	if err := s.journal.AppendCancel(m, a, id); err != nil {
		return perp.Result{}, fmt.Errorf("services: perp: append journal: %w", err)
	}
	res, err := s.engine.Cancel(m, a, id)
	if err != nil {
		return perp.Result{}, err
	}
	quote := s.markets[m].Quote
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, quote, r.Amount); err != nil {
			return perp.Result{}, s.degrade(err)
		}
	}
	s.publish(res)
	return res, nil
}

// settlePnL realizes each PnL settlement as a DepositOp (profit) or WithdrawOp
// (loss) in the settlement's market quote asset, keyed by trade id so a replay
// is idempotent.
func (s *PerpOrderBookService) settlePnL(pnls []perp.PnLSettlement) error {
	for _, p := range pnls {
		quote := s.markets[p.Market].Quote
		amt := new(uint256.Int).Set(p.Profit)
		if amt.Sign() < 0 {
			amt.Neg(amt)
		}
		if amt.IsZero() {
			continue
		}
		ref := fmt.Sprintf("perp:pnl:%d", p.TradeID)
		if p.Profit.Sign() >= 0 {
			if err := s.ledger.Apply(ledger.DepositOp{Account: p.Account, Asset: quote, Amount: amt, Ref: ref}); err != nil {
				return err
			}
		} else {
			if err := s.ledger.Apply(ledger.WithdrawOp{Account: p.Account, Asset: quote, Amount: amt, Ref: ref}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *PerpOrderBookService) degrade(cause error) error {
	if s.fatalErr == nil {
		s.fatalErr = fmt.Errorf("%w: %v", ErrPerpServiceDegraded, cause)
	}
	_ = s.bus.Publish(Event{Topic: TopicPerpDegraded, Data: cause})
	return s.fatalErr
}

func (s *PerpOrderBookService) publish(res perp.Result) {
	if res.Order != nil {
		_ = s.bus.Publish(Event{Topic: TopicPerpPosition, Data: res.Order})
	}
	for _, t := range res.Trades {
		_ = s.bus.Publish(Event{Topic: TopicPerpTrade, Data: t})
	}
}

// recover replays the instruction WAL, re-settling PnL idempotently and then
// reconciling holds from the kernel's open margins.
func (s *PerpOrderBookService) recover() error {
	ins, err := s.journal.Replay()
	if err != nil {
		return fmt.Errorf("services: perp: replay journal: %w", err)
	}
	for _, in := range ins {
		switch in.Kind {
		case "place":
			res, err := s.engine.Place(*in.Place)
			if err != nil {
				continue // deterministically rejected
			}
			if err := s.settlePnL(res.PnL); err != nil {
				return fmt.Errorf("services: perp: replay settle: %w", err)
			}
		case "cancel":
			if _, err := s.engine.Cancel(in.Cancel.Market, in.Cancel.Account, in.Cancel.OrderID); err != nil {
				continue
			}
		}
	}
	return s.holds.Reconcile(s.engine.OpenMargins())
}

// ParsePerpMarkets parses the PERP_MARKETS string:
//
//	ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]
//
// separated by ';'. A malformed field is an error (an operator typo must not
// silently trade a subset).
func ParsePerpMarkets(s string) ([]perp.Market, error) {
	var out []perp.Market
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		if len(fields) < 6 {
			return nil, fmt.Errorf("services: perp market %q: want ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]", part)
		}
		id, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad id: %w", part, err)
		}
		base, err := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad base: %w", part, err)
		}
		quote, err := strconv.ParseUint(strings.TrimSpace(fields[3]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad quote: %w", part, err)
		}
		decimals, err := strconv.ParseUint(strings.TrimSpace(fields[4]), 10, 8)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad base decimals: %w", part, err)
		}
		leverage, err := strconv.ParseUint(strings.TrimSpace(fields[5]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("services: perp market %q: bad max leverage: %w", part, err)
		}
		m := perp.Market{
			ID: perp.MarketID(id), Symbol: strings.TrimSpace(fields[1]),
			Base: ledger.AssetID(base), Quote: ledger.AssetID(quote),
			BaseDecimals: uint8(decimals), MaxLeverage: uint32(leverage),
		}
		if len(fields) >= 7 && strings.TrimSpace(fields[6]) != "" {
			minQty, err := uint256.FromDecimal(strings.TrimSpace(fields[6]))
			if err != nil {
				return nil, fmt.Errorf("services: perp market %q: bad min qty: %w", part, err)
			}
			m.MinQty = minQty
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

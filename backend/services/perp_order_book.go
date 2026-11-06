package services

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
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

	// 6b topics: derived mark prices and completed funding rounds.
	TopicPerpMark    = "perp.mark"
	TopicPerpFunding = "perp.funding"

	// 6c topics: forced liquidations and bad debt absorbed by the insurance fund.
	TopicPerpLiquidation = "perp.liquidation"
	TopicPerpBadDebt     = "perp.baddebt"
	// TopicPerpADL reports auto-deleveraging of a profitable position.
	TopicPerpADL = "perp.adl"

	// perpRunInterval is the default poll interval for the runner: one price
	// refresh per tick (6b). With no PriceSource it is pure overhead, which is
	// why the interval is configurable.
	perpRunInterval = 5 * time.Second
)

var ErrPerpServiceDegraded = errors.New("services: perp order book degraded, restart required")

// MarkUpdate is published on TopicPerpMark whenever a market's mark price moves.
type MarkUpdate struct {
	Market perp.MarketID
	Index  perp.Price
	Mark   perp.Price
	At     int64
}

// LiquidationEvent is published on TopicPerpLiquidation after a forced close.
type LiquidationEvent struct {
	Market  perp.MarketID
	Account ledger.AccountID
	Size    *uint256.Int
	Mark    perp.Price
	Equity  *big.Int
	Fee     *uint256.Int
	Partial bool
	At      int64
}

// BadDebtEvent is published when a loss exceeds everything the account and the
// insurance fund could cover.
type BadDebtEvent struct {
	Account ledger.AccountID
	Asset   ledger.AssetID
	Amount  *uint256.Int
	At      int64
}

// ADLEvent is published when a profitable position is force-reduced to cover a
// shortfall the insurance fund could not pay.
type ADLEvent struct {
	Market  perp.MarketID
	Account ledger.AccountID
	Qty     *uint256.Int
	Mark    perp.Price
	Ref     string
}

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

	// 6b pricing and funding state. Absent a PriceSource the service behaves
	// exactly like 6a: no marks, no funding rounds, no extra journal records.
	prices      perp.PriceSource
	tracker     *PerpPriceTracker
	funding     map[perp.MarketID]FundingState
	debts       map[fundingDebtKey]*uint256.Int
	fundingOpts FundingOptions
	lastFunding map[perp.MarketID]int64

	// 6c risk state: the account that absorbs insolvency, and the scan cadence.
	insuranceFund   ledger.AccountID
	// feeAccount receives trading fees. Defaults to the insurance fund so fees
	// strengthen the buffer that absorbs bad debt.
	feeAccount      ledger.AccountID
	liquidationOpts LiquidationOptions
	lastLiquidation time.Time
	liquidationScans int

	// badDebts buffers bad-debt events produced under s.mu; they are published
	// after the lock is released (see reportDegraded / publish).
	badDebts []BadDebtEvent
	// adlEvents buffers auto-deleverage events for the same reason.
	adlEvents []ADLEvent
	// candles aggregates OHLCV bars from the trade stream.
	candles *CandleStore
	// monitor is the registered health reporter, if any.
	monitor *PerpMonitor

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
		funding:     make(map[perp.MarketID]FundingState),
		debts:       make(map[fundingDebtKey]*uint256.Int),
		lastFunding: make(map[perp.MarketID]int64),

		insuranceFund:   reservedInsuranceAccount,
		feeAccount:      reservedInsuranceAccount,
		liquidationOpts: LiquidationOptions{Interval: time.Second},

		candles: NewCandleStore(),
	}
	// Feed candles from the same trade stream clients subscribe to, so a chart is
	// always derived from the exact trades the WebSocket served.
	s.candles.Subscribe(bus)
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PerpOrderBookService) Name() string { return "PerpOrderBookService" }

func (s *PerpOrderBookService) Initialize(cfg Config) error { return nil }

func (s *PerpOrderBookService) Start() error {
	s.run.start(s.tickInterval(), func() {
		s.tick(time.Now())
	})
	return nil
}

// tickInterval returns the runner's wake-up cadence: the shortest interval the
// scheduler drives.
func (s *PerpOrderBookService) tickInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tickIntervalLocked()
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

// SetPriceSource installs the index price source used for mark prices and
// funding. Without one, the service keeps the 6a behaviour: MarkPrice reports
// no price and no funding round is ever triggered.
func (s *PerpOrderBookService) SetPriceSource(p perp.PriceSource, opts PriceOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prices = p
	s.tracker = NewPerpPriceTracker(opts.MaxPriceAge, opts.MaxMarkStaleAge)
}

// SetFundingOptions installs the funding scheduler's cadence and caps. It is
// separate from SetPriceSource so callers can tune one without the other.
func (s *PerpOrderBookService) SetFundingOptions(opts FundingOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.Tick <= 0 {
		opts.Tick = perpRunInterval
	}
	s.fundingOpts = opts
	s.run.setInterval(s.tickIntervalLocked())
}

// tick is one scheduler iteration: refresh marks, run the liquidation scan when
// its own interval is due, then run any funding round that is due.
func (s *PerpOrderBookService) tick(now time.Time) {
	s.refreshMarks(now)

	s.mu.Lock()
	liqDue := s.lastLiquidation.IsZero() ||
		now.Sub(s.lastLiquidation) >= s.liquidationInterval()
	if liqDue {
		s.lastLiquidation = now
	}
	s.mu.Unlock()
	if liqDue {
		s.mu.Lock()
		s.liquidationScans++
		s.mu.Unlock()
		s.scanLiquidations(now)
	}

	s.mu.Lock()
	opts := s.fundingOpts
	due := make([]perp.MarketID, 0, len(s.markets))
	for id := range s.markets {
		last, ok := s.lastFunding[id]
		if !ok {
			// Start the clock at the first tick so a freshly started service
			// does not settle funding the instant it boots.
			s.lastFunding[id] = now.Unix()
			continue
		}
		interval := opts.Interval
		if interval <= 0 {
			interval = time.Hour
		}
		if now.Unix()-last >= int64(interval/time.Second) {
			due = append(due, id)
		}
	}
	s.mu.Unlock()

	for _, id := range due {
		interval := opts.Interval
		if interval <= 0 {
			interval = time.Hour
		}
		// Whether or not the round settles (a market with no mark skips), the
		// clock advances: otherwise a market that can never price itself would
		// retry every tick forever.
		if _, _, err := s.RunFundingRound(id, now, interval, opts.MaxRateE18); err != nil {
			continue
		}
		s.mu.Lock()
		s.lastFunding[id] = now.Unix()
		s.mu.Unlock()
	}
}

// PriceOptions configures price freshness. Zero durations mean defaults.
type PriceOptions struct {
	MaxPriceAge     time.Duration
	MaxMarkStaleAge time.Duration
}

// Candles returns the OHLCV store for chart endpoints.
func (s *PerpOrderBookService) Candles() *CandleStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.candles
}

// MarkPrice returns the latest derived mark price for a market.
func (s *PerpOrderBookService) MarkPrice(m perp.MarketID) (perp.Price, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracker == nil {
		return 0, false
	}
	return s.tracker.LastMark(m)
}

// TickForTest runs one scheduler iteration. It is exported for tests that
// exercise the gateway without waiting on the runner's ticker; production code
// drives the same path through Start.
func (s *PerpOrderBookService) TickForTest(now time.Time) { s.tick(now) }

// IndexPrice returns the last observed index price for a market.
func (s *PerpOrderBookService) IndexPrice(m perp.MarketID) (perp.Price, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracker == nil {
		return 0, false
	}
	return s.tracker.IndexPrice(m)
}

// UnrealizedPnL returns an account's mark-price unrealized PnL in a market, and
// false when the account has no position or the market has no mark price.
func (s *PerpOrderBookService) UnrealizedPnL(m perp.MarketID, a ledger.AccountID) (*big.Int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	market, ok := s.markets[m]
	if !ok || s.tracker == nil {
		return nil, false
	}
	mark, ok := s.tracker.LastMark(m)
	if !ok {
		return nil, false
	}
	pos, ok := s.engine.Position(m, a)
	if !ok {
		return new(big.Int), true
	}
	pnl, err := market.UnrealizedPnL(pos.Side, pos.Size, pos.EntryPrice, mark)
	if err != nil {
		return nil, false
	}
	return pnl, true
}

// refreshMarks pulls one index price per market and derives a mark. It journals
// a mark record only when the derived mark changed, so an idle market does not
// grow the WAL every tick.
//
// Events are collected under the lock and published after it is released: the
// EventBus invokes subscribers synchronously, and a subscriber that calls back
// into this service would self-deadlock if the lock were still held.
func (s *PerpOrderBookService) refreshMarks(now time.Time) {
	var updates []MarkUpdate
	s.mu.Lock()
	if s.prices == nil || s.tracker == nil || s.fatalErr != nil {
		s.mu.Unlock()
		return
	}
	var journalErr error
	for id, market := range s.markets {
		index, ok := s.prices.Index(id)
		if !ok || index == 0 {
			continue
		}
		s.tracker.Observe(id, index, now)
		longs, shorts := s.engine.Book(id, 1)
		var bestBid, bestAsk perp.Price
		var hasBid, hasAsk bool
		if len(longs) > 0 {
			bestBid, hasBid = longs[0].Price, true
		}
		if len(shorts) > 0 {
			bestAsk, hasAsk = shorts[0].Price, true
		}
		before, hadMark := s.tracker.LastMark(id)
		mark, ok := s.tracker.Mark(market, index, bestBid, bestAsk, hasBid, hasAsk, now)
		if !ok || (hadMark && mark == before) {
			continue
		}
		if err := s.journal.AppendMark(MarkCmd{Market: id, Index: index, Mark: mark, At: now.Unix()}); err != nil {
			journalErr = err
			break
		}
		updates = append(updates, MarkUpdate{Market: id, Index: index, Mark: mark, At: now.Unix()})
	}
	s.mu.Unlock()

	if journalErr != nil {
		_ = s.degrade(journalErr)
		return
	}
	for _, u := range updates {
		_ = s.bus.Publish(Event{Topic: TopicPerpMark, Data: u})
	}
}

// Place freezes the order-level margin, journals the instruction, matches,
// realizes PnL into the ledger, releases margin, and publishes events — in that
// order. A validation or funding failure leaves positions untouched.
//
// Events are published AFTER s.mu is released. EventBus.Publish invokes
// subscribers synchronously, so a subscriber that reads service state (for an
// alert, a depth push, anything) would self-deadlock the matcher if it ran under
// the lock. See TestPublishDoesNotHoldLock.
func (s *PerpOrderBookService) Place(req perp.PlaceRequest) (perp.Result, error) {
	res, err := s.place(req)
	s.reportDegraded()
	if err != nil {
		return perp.Result{}, err
	}
	s.publish(res)
	return res, nil
}

// reportDegraded publishes the degradation event if the service just degraded.
// It is called from unlocked contexts only.
func (s *PerpOrderBookService) reportDegraded() {
	s.mu.Lock()
	err := s.fatalErr
	s.mu.Unlock()
	if err != nil {
		_ = s.bus.Publish(Event{Topic: TopicPerpDegraded, Data: err})
	}
}

func (s *PerpOrderBookService) place(req perp.PlaceRequest) (perp.Result, error) {
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
	// Fees are collected after PnL, so a losing close pays its loss first and the
	// fee is capped by whatever remains rather than driving the balance negative.
	if err := s.settleFees(res.Fees); err != nil {
		return perp.Result{}, s.degrade(err)
	}
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, quote, r.Amount); err != nil {
			return perp.Result{}, s.degrade(err)
		}
	}
	return res, nil
}

// Cancel journals the instruction, removes the caller's resting order, releases
// its residual margin, and publishes the position event. Like Place, the event
// is published after the lock is released.
func (s *PerpOrderBookService) Cancel(m perp.MarketID, a ledger.AccountID, id perp.OrderID) (perp.Result, error) {
	res, err := s.cancel(m, a, id)
	s.reportDegraded()
	if err != nil {
		return perp.Result{}, err
	}
	s.publish(res)
	return res, nil
}

func (s *PerpOrderBookService) cancel(m perp.MarketID, a ledger.AccountID, id perp.OrderID) (perp.Result, error) {
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
	return res, nil
}

// settlePnL realizes each PnL settlement as a DepositOp (profit) or WithdrawOp
// (loss) in the settlement's market quote asset, keyed by trade id AND account
// so a replay is idempotent. The account is part of the key because a single
// trade can settle BOTH sides (maker and taker each reducing a position); a
// trade-only key would make the ledger silently drop the second settlement,
// destroying or minting funds.
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
		ref := fmt.Sprintf("perp:pnl:%d:%s", p.TradeID, p.Account.Hex())
		if p.Profit.Sign() >= 0 {
			if err := s.ledger.Apply(ledger.DepositOp{Account: p.Account, Asset: quote, Amount: amt, Ref: ref}); err != nil {
				return err
			}
			continue
		}
		if err := s.ledger.Apply(ledger.WithdrawOp{Account: p.Account, Asset: quote, Amount: amt, Ref: ref}); err != nil {
			if errors.Is(err, ledger.ErrInsufficientBalance) {
				// A loss larger than the account's balance is a shortfall, not a
				// reason to halt the whole book (spec §3.3). Debit what is there
				// and cover the rest from the insurance fund.
				ev, err := s.coverShortfall(p.Account, quote, amt, ref)
				if err != nil {
					return err
				}
				if ev != nil {
					s.badDebts = append(s.badDebts, *ev)
				}
				continue
			}
			return err
		}
	}
	return nil
}

// settleFees moves each fill's fee from the trading account to the fee account.
//
// A fee is a transfer, not a burn and not a mint: the ledger total is unchanged.
// The idempotency key carries trade id, account and role, so replay cannot charge
// twice and the two sides of one trade are distinct keys.
//
// A fee that exceeds the account's balance is charged only up to that balance.
// Charging more would go negative on an account already being liquidated; the
// remainder is dropped rather than recorded as debt, because a fee is never a
// claim on future funds.
func (s *PerpOrderBookService) settleFees(fees []perp.FeeSettlement) error {
	for _, f := range fees {
		if f.Amount == nil || f.Amount.IsZero() {
			continue
		}
		quote := s.markets[f.Market].Quote
		ref := fmt.Sprintf("perp:fee:%d:%s:%s", f.TradeID, f.Account.Hex(), f.Role)
		if s.ledger.WasApplied(ref) {
			continue
		}
		amount := new(uint256.Int).Set(f.Amount)
		if bal := s.ledger.Balance(f.Account, quote); bal.Cmp(amount) < 0 {
			amount = new(uint256.Int).Set(bal)
		}
		if amount.IsZero() {
			continue
		}
		if err := s.ledger.Apply(ledger.TransferOp{
			From: f.Account, To: s.feeAccount, Asset: quote, Amount: amount, Ref: ref,
		}); err != nil {
			if errors.Is(err, ledger.ErrInsufficientBalance) {
				continue // nothing to take; a fee must not halt the book
			}
			return err
		}
	}
	return nil
}

// coverShortfall debits whatever the account can pay, then draws the remainder// from the insurance fund. The book stays live: a single account's insolvency is
// that account's problem, never a market-wide halt.
//
// It returns the bad-debt event it would like published, or nil. It does NOT
// publish: every caller holds s.mu, and EventBus subscribers run synchronously.
func (s *PerpOrderBookService) coverShortfall(account ledger.AccountID, asset ledger.AssetID, loss *uint256.Int, ref string) (*BadDebtEvent, error) {
	balance := s.ledger.Balance(account, asset)
	payable := new(uint256.Int).Set(loss)
	if balance.Cmp(payable) < 0 {
		payable = new(uint256.Int).Set(balance)
	}
	if !payable.IsZero() {
		if err := s.ledger.Apply(ledger.WithdrawOp{
			Account: account, Asset: asset, Amount: payable, Ref: ref,
		}); err != nil {
			return nil, err
		}
	}
	shortfall := new(uint256.Int).Sub(loss, payable)
	if shortfall.IsZero() {
		return nil, nil
	}
	fundRef := fmt.Sprintf("%s:fund", ref)
	if !s.ledger.WasApplied(fundRef) {
		if err := s.ledger.Apply(ledger.DepositOp{
			Account: account, Asset: asset, Amount: shortfall, Ref: fundRef,
		}); err != nil {
			return nil, err
		}
	}
	ev := &BadDebtEvent{Account: account, Asset: asset, Amount: shortfall}
	// Try to cover the shortfall by de-leveraging profitable positions first. ADL
	// is the last resort before bad debt: it takes profit from winners rather
	// than minting the shortfall out of nothing.
	if covered, adlErr := s.autoDeleverage(asset, shortfall, account); adlErr == nil && covered.Sign() > 0 {
		remaining := new(uint256.Int).Sub(shortfall, covered)
		if remaining.IsZero() {
			return nil, nil // fully covered; no bad debt
		}
		shortfall = remaining
		ev.Amount = shortfall
	}

	if err := s.ledger.Apply(ledger.WithdrawOp{
		Account: s.insuranceFund, Asset: asset, Amount: shortfall,
		Ref: fmt.Sprintf("%s:drawn", ref),
	}); err != nil {
		if errors.Is(err, ledger.ErrInsufficientBalance) {
			// The fund itself cannot pay. Record the bad debt and keep trading:
			// an insolvent fund is an accounting fact, not a reason to stop.
			return ev, nil
		}
		return nil, err
	}
	return ev, nil
}

// autoDeleverage reduces profitable positions to cover up to `need`, returning
// the amount actually raised. Caller must hold s.mu.
//
// Selection is by unrealized profit, largest first: ADL must remove the least
// size to cover the gap, and the biggest winner is the one carrying the most
// unrealized gain to give back. Positions are reduced at the mark, so the winner
// keeps the profit it had earned up to that price.
func (s *PerpOrderBookService) autoDeleverage(asset ledger.AssetID, need *uint256.Int, deficient ledger.AccountID) (*uint256.Int, error) {
	raised := uint256.NewInt(0)
	if need.IsZero() {
		return raised, nil
	}

	type candidate struct {
		market perp.Market
		pos    perp.Position
		mark   perp.Price
		profit *big.Int
	}
	var cands []candidate
	// Caller holds s.mu, so the fields are read directly.
	if s.tracker == nil {
		return raised, nil // no prices, no way to judge profit
	}
	for id, market := range s.markets {
		if market.Quote != asset {
			continue
		}
		mark, ok := s.tracker.LastMark(id)
		if !ok {
			continue // never de-leverage on a missing price
		}
		for _, p := range s.engine.Positions(id) {
			pnl, err := market.UnrealizedPnL(p.Side, p.Size, p.EntryPrice, mark)
			if err != nil || pnl.Sign() <= 0 {
				continue // only profitable positions are eligible
			}
			cands = append(cands, candidate{market: market, pos: p, mark: mark, profit: pnl})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if c := cands[i].profit.Cmp(cands[j].profit); c != 0 {
			return c > 0
		}
		return cands[i].pos.Account.Hex() < cands[j].pos.Account.Hex()
	})

	for _, c := range cands {
		if raised.Cmp(need) >= 0 {
			break
		}
		// How much of this position covers the remaining need: at the mark, a
		// unit of size yields notional/leverage-independent profit proportional
		// to the price move. Reduce at most the whole position.
		qty := new(uint256.Int).Set(c.pos.Size)
		avail := new(big.Int).Set(c.profit)
		remainingNeed := new(big.Int).Sub(need.ToBig(), raised.ToBig())
		if avail.Cmp(remainingNeed) > 0 {
			// Only part of the position is needed: scale qty by need/profit.
			scaled := new(big.Int).Mul(qty.ToBig(), remainingNeed)
			scaled.Div(scaled, avail)
			if scaled.Sign() <= 0 {
				continue
			}
			q := uint256.MustFromBig(scaled)
			if q.Cmp(qty) > 0 {
				q = qty
			}
			qty = q
		}

		res, err := s.engine.AutoDeleverage(c.market.ID, c.pos.Account, qty, c.mark)
		if err != nil {
			if errors.Is(err, perp.ErrOrderNotFound) {
				continue
			}
			return raised, err
		}
		for _, r := range res.Releases {
			if err := s.holds.Release(r.Account, asset, r.Amount); err != nil {
				return raised, err
			}
		}

		// The profit realized by this reduction pays the shortfall. It is MOVED,
		// not created: a single TransferOp takes it from the de-leveraged winner
		// and credits the insolvent account, so the ledger total is unchanged.
		// settlePnL is deliberately not used — it would deposit the profit without
		// debiting anything, minting the money ADL exists to avoid minting.
		for _, p := range res.PnL {
			if p.Profit.Sign() <= 0 {
				continue
			}
			transferRef := fmt.Sprintf("perp:adl:%d:%s", p.TradeID, p.Account.Hex())
			if !s.ledger.WasApplied(transferRef) {
				if err := s.ledger.Apply(ledger.TransferOp{
					From: p.Account, To: deficient, Asset: asset,
					Amount: p.Profit, Ref: transferRef,
				}); err != nil {
					return raised, err
				}
			}
			raised.Add(raised, p.Profit)
		}
		s.adlEvents = append(s.adlEvents, ADLEvent{
			Account: c.pos.Account, Market: c.market.ID,
			Qty: new(uint256.Int).Set(qty), Mark: c.mark, Ref: deficient.Hex(),
		})
	}
	if raised.Cmp(need) > 0 {
		raised = new(uint256.Int).Set(need)
	}
	return raised, nil
}

// degrade marks the service unusable and returns the sentinel error. It does NOT
// publish: every caller holds s.mu, and publishing under the lock can deadlock
// against a subscriber. Callers that want the event published must do so after
// unlocking (see the `pending` buffer in Place/Cancel/liquidate).
func (s *PerpOrderBookService) degrade(cause error) error {
	if s.fatalErr == nil {
		s.fatalErr = fmt.Errorf("%w: %v", ErrPerpServiceDegraded, cause)
	}
	return s.fatalErr
}

// publish emits the result's events plus any bad-debt events buffered while the
// lock was held. Callers must NOT hold s.mu.
func (s *PerpOrderBookService) publish(res perp.Result) {
	if res.Order != nil {
		_ = s.bus.Publish(Event{Topic: TopicPerpPosition, Data: res.Order})
	}
	for _, t := range res.Trades {
		_ = s.bus.Publish(Event{Topic: TopicPerpTrade, Data: t})
	}

	// Drain bad debts recorded under the lock. Copy first so a concurrent append
	// to s.badDebts is not lost.
	s.mu.Lock()
	debts := s.badDebts
	s.badDebts = nil
	adls := s.adlEvents
	s.adlEvents = nil
	s.mu.Unlock()
	for _, ev := range debts {
		_ = s.bus.Publish(Event{Topic: TopicPerpBadDebt, Data: ev})
	}
	for _, ev := range adls {
		_ = s.bus.Publish(Event{Topic: TopicPerpADL, Data: ev})
	}
}

// recover replays the instruction WAL and re-settles PnL idempotently. It does
// NOT reconcile Holds: Holds.Reconcile replaces the whole map, and the perp
// service shares that map with the spot order book, so reconciling here would
// drop the spot book's freezes. The caller reconciles the union of both books
// once (see UnionHolds).
func (s *PerpOrderBookService) recover() error {
	ins, err := s.journal.Replay()
	if err != nil {
		return fmt.Errorf("services: perp: replay journal: %w", err)
	}
	for _, in := range ins {
		switch in.Kind {
		case "mark":
			// A replayed mark restores the price history without re-reading an
			// oracle whose price may have moved (spec §3.5).
			if s.tracker == nil {
				s.tracker = NewPerpPriceTracker(0, 0)
			}
			s.tracker.Observe(in.Mark.Market, in.Mark.Index, time.Unix(in.Mark.At, 0))
			s.tracker.restore(in.Mark.Market, in.Mark.Mark, time.Unix(in.Mark.At, 0))
		case "funding":
			rate, ok := new(big.Int).SetString(in.Funding.Rate, 10)
			if !ok {
				return fmt.Errorf("services: perp: replay funding: bad rate %q", in.Funding.Rate)
			}
			for _, l := range in.Funding.Legs {
				ref := fundingRef(in.Funding.Market, in.Funding.Index, l.Payer, l.Receiver)
				if s.ledger.WasApplied(ref) {
					continue
				}
				if err := s.ledger.Apply(ledger.TransferOp{
					From: l.Payer, To: l.Receiver, Asset: s.markets[in.Funding.Market].Quote,
					Amount: l.Amount, Ref: ref,
				}); err != nil {
					// A replay that cannot re-apply a journaled leg means the
					// ledger snapshot and the WAL diverged: refuse to start.
					return fmt.Errorf("services: perp: replay funding leg: %w", err)
				}
			}
			if market, ok := s.markets[in.Funding.Market]; ok {
				s.recordFundingState(market, in.Funding.Index, rate.String(), in.Funding.Mark, in.Funding.At)
			}
			// Rebuild this market's debt state from the record. The round's Debts
			// list is the complete post-round state for the market, so it replaces
			// whatever earlier rounds left behind.
			for k := range s.debts {
				if k.Market == in.Funding.Market {
					delete(s.debts, k)
				}
			}
			for _, d := range in.Funding.Debts {
				if d.Amount == nil || d.Amount.IsZero() {
					continue
				}
				s.debts[fundingDebtKey{Account: d.Account, Market: in.Funding.Market}] =
					new(uint256.Int).Set(d.Amount)
			}
		case "place":
			res, err := s.engine.Place(*in.Place)
			if err != nil {
				// Every journaled place was validated before it was appended and
				// the kernel is deterministic, so a failure here means the WAL or
				// the market configuration diverged. Refuse to start rather than
				// silently drop a user's order.
				return fmt.Errorf("services: perp: replay place: %w", err)
			}
			if err := s.settlePnL(res.PnL); err != nil {
				return fmt.Errorf("services: perp: replay settle: %w", err)
			}
			if err := s.settleFees(res.Fees); err != nil {
				return fmt.Errorf("services: perp: replay fees: %w", err)
			}
		case "cancel":
			// A cancel of an order that is no longer open is harmless to skip.
			if _, err := s.engine.Cancel(in.Cancel.Market, in.Cancel.Account, in.Cancel.OrderID); err != nil &&
				!errors.Is(err, perp.ErrOrderNotFound) {
				return fmt.Errorf("services: perp: replay cancel: %w", err)
			}
		case "liquidation":
			// Replay the forced close rung exactly as it ran. The Limit field
			// distinguishes the limit pass from the market sweep, and the two fill
			// against different counterparties — replaying a sweep when a limit
			// ran would produce a different book and different PnL.
			var res perp.Result
			var err error
			if in.Liquidation.Limit == 0 {
				res, err = s.engine.Liquidate(in.Liquidation.Market, in.Liquidation.Account, nil)
			} else {
				res, err = s.engine.LiquidateLimit(in.Liquidation.Market, in.Liquidation.Account,
					nil, in.Liquidation.Limit)
			}
			if err != nil {
				if errors.Is(err, perp.ErrOrderNotFound) {
					continue // Already closed by an earlier replayed instruction.
				}
				return fmt.Errorf("services: perp: replay liquidation: %w", err)
			}
			// PnL is idempotent by trade id + account, so re-applying is safe.
			if err := s.settlePnL(res.PnL); err != nil {
				return fmt.Errorf("services: perp: replay liquidation settle: %w", err)
			}
		}
	}
	return nil
}

// OpenMargins returns the (account, quote asset) → frozen margin map implied by
// the kernel's final state, covering both open positions and resting orders.
func (s *PerpOrderBookService) OpenMargins() map[ledger.HoldKey]*uint256.Int {
	return s.engine.OpenMargins()
}

// UnionHolds merges two open-hold maps into a fresh map, summing amounts that
// share a key. Holds.Reconcile replaces the entire map, so the spot and perp
// books must be reconciled together — reconciling either alone would unfreeze
// the other's resting orders on restart.
func UnionHolds(a, b map[ledger.HoldKey]*uint256.Int) map[ledger.HoldKey]*uint256.Int {
	out := make(map[ledger.HoldKey]*uint256.Int, len(a)+len(b))
	for _, src := range []map[ledger.HoldKey]*uint256.Int{a, b} {
		for k, v := range src {
			if v == nil || v.IsZero() {
				continue
			}
			if cur, ok := out[k]; ok {
				cur.Add(cur, v)
			} else {
				out[k] = new(uint256.Int).Set(v)
			}
		}
	}
	return out
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



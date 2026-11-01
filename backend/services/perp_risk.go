package services

import (
	"errors"
	"fmt"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// reservedInsuranceAccount is the fallback insurance fund account when the
// operator does not configure one. It is a fixed, recognizable address rather
// than a random one so accounting reports can find it.
var reservedInsuranceAccount = ledger.AccountID([20]byte{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xde, 0xad,
})

// LiquidationOptions configures the forced-closing loop (spec §3.6).
type LiquidationOptions struct {
	// Interval is the pause between liquidation scans. Zero means one second.
	Interval time.Duration
}

// SetInsuranceFund sets the account that absorbs losses exceeding a liquidated
// trader's margin. It also becomes the fee recipient unless SetFeeAccount was
// called, so fees strengthen the buffer that pays bad debt.
func (s *PerpOrderBookService) SetInsuranceFund(a ledger.AccountID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insuranceFund = a
	if s.feeAccount == reservedInsuranceAccount {
		s.feeAccount = a
	}
}

// SetFeeAccount sets where trading fees are credited.
func (s *PerpOrderBookService) SetFeeAccount(a ledger.AccountID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feeAccount = a
}

// FeeAccount returns the account trading fees are credited to.
func (s *PerpOrderBookService) FeeAccount() ledger.AccountID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feeAccount
}

// InsuranceFund returns the configured insurance fund account.
func (s *PerpOrderBookService) InsuranceFund() ledger.AccountID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insuranceFund
}

// SetLiquidationOptions configures the liquidation scan cadence.
func (s *PerpOrderBookService) SetLiquidationOptions(opts LiquidationOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	s.liquidationOpts = opts
	s.run.setInterval(s.tickIntervalLocked())
}

// liquidationInterval returns the configured scan period, defaulting to one
// second. Caller must hold s.mu.

// LiquidationScans reports how many times the liquidation scan has run. It exists
// so the scheduler's cadence is observable without inferring it from side effects.
func (s *PerpOrderBookService) LiquidationScans() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liquidationScans
}
func (s *PerpOrderBookService) liquidationInterval() time.Duration {
	if s.liquidationOpts.Interval <= 0 {
		return time.Second
	}
	return s.liquidationOpts.Interval
}

// tickIntervalLocked returns how often the runner must wake: the shortest of the
// intervals it drives. Without this a 1s liquidation cadence would still only be
// honoured at the price-refresh interval. Caller must hold s.mu.
func (s *PerpOrderBookService) tickIntervalLocked() time.Duration {
	out := perpRunInterval
	if s.fundingOpts.Tick > 0 && s.fundingOpts.Tick < out {
		out = s.fundingOpts.Tick
	}
	if li := s.liquidationInterval(); li < out {
		out = li
	}
	return out
}

// scanLiquidations closes every position whose equity has fallen below its
// maintenance margin (spec §3.2). A market without a fresh mark price is skipped:
// liquidating on a stale price would misjudge risk and punish the wrong people.
func (s *PerpOrderBookService) scanLiquidations(now time.Time) {
	s.mu.Lock()
	if s.tracker == nil || s.fatalErr != nil {
		s.mu.Unlock()
		return
	}
	type target struct {
		market perp.Market
		mark   perp.Price
		pos    perp.Position
	}
	var candidates []target

	// Fresh marks per market, shared by both the isolated and cross judgements.
	marks := make(map[perp.MarketID]perp.Price)
	for id := range s.markets {
		mark, ok := s.tracker.LastMark(id)
		if !ok || s.tracker.Stale(id, now) {
			// A stale index means the mark is a fallback, not a live view.
			continue
		}
		marks[id] = mark
	}

	for id, market := range s.markets {
		mark, ok := marks[id]
		if !ok {
			continue
		}
		positions := s.engine.Positions(id)

		if market.RiskConfig.Mode != perp.MarginCross {
			for _, p := range positions {
				bad, err := market.Liquidatable(&p, mark)
				if err != nil || !bad {
					continue
				}
				candidates = append(candidates, target{market: market, mark: mark, pos: p})
			}
			continue
		}

		// Cross margin: judge every position in this quote asset together, so a
		// winning leg can back a losing one. Only the worst leg is closed per
		// scan; the next scan re-evaluates the aggregate.
		byAccount := make(map[ledger.AccountID][]perp.Position)
		for _, p := range positions {
			byAccount[p.Account] = append(byAccount[p.Account], p)
		}
		for acct := range byAccount {
			all := s.crossPositions(acct, market.Quote, marks)
			risk, err := perp.AccountRiskFor(acct, all, s.markets, marks)
			if err != nil || !risk.Liquidatable || risk.WorstMarket == 0 {
				continue
			}
			for _, p := range all {
				if p.Market == risk.WorstMarket {
					candidates = append(candidates, target{
						market: s.markets[p.Market], mark: marks[p.Market], pos: p,
					})
					break
				}
			}
		}
	}
	s.mu.Unlock()

	for _, c := range candidates {
		s.liquidate(c.market, c.pos, c.mark, now)
	}
}

// crossPositions collects every position an account holds in markets sharing the
// given quote asset — the margin scope for cross judgement. Caller must hold s.mu.
func (s *PerpOrderBookService) crossPositions(account ledger.AccountID, asset ledger.AssetID, marks map[perp.MarketID]perp.Price) []perp.Position {
	var out []perp.Position
	for id, market := range s.markets {
		if market.Quote != asset || market.RiskConfig.Mode != perp.MarginCross {
			continue
		}
		if _, ok := marks[id]; !ok {
			continue // no price, not part of the risk picture
		}
		if p, ok := s.engine.Position(id, account); ok {
			out = append(out, *p)
		}
	}
	return out
}

// liquidate force-closes one position and reports it.
//
// The instruction is journaled BEFORE the engine is touched, so a restart replays
// the same liquidation instead of resurrecting a position whose loss the ledger
// already settled. Events are published after the lock is released, for the same
// re-entrancy reason as Place.
func (s *PerpOrderBookService) liquidate(market perp.Market, pos perp.Position, mark perp.Price, now time.Time) {
	ev, res, ok := s.liquidateLocked(market, pos, mark, now)
	if !ok {
		return
	}
	s.publish(res)
	_ = s.bus.Publish(Event{Topic: TopicPerpLiquidation, Data: ev})
}

func (s *PerpOrderBookService) liquidateLocked(market perp.Market, pos perp.Position, mark perp.Price, now time.Time) (LiquidationEvent, perp.Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return LiquidationEvent{}, perp.Result{}, false
	}

	equity, err := market.Equity(pos.Side, pos.Size, pos.EntryPrice, mark, pos.Margin)
	if err != nil {
		return LiquidationEvent{}, perp.Result{}, false
	}
	fee, err := market.LiquidatorFee(pos.Size, mark)
	if err != nil {
		fee = uint256.NewInt(0)
	}

	// Probe before journaling: a liquidation with nothing to hit the book closes
	// nothing, so it must not leave a WAL record, charge a fee, or emit an event.
	if !s.canFill(market.ID, pos) {
		return LiquidationEvent{}, perp.Result{}, false
	}

	// The ladder, rung by rung. Each rung is journaled before it runs, because a
	// replay must reproduce the identical fills: a limit pass and a sweep trade
	// different counterparties even on the same book.
	var (
		results []perp.Result
		limits  []perp.Price
	)
	appendRung := func(limit perp.Price) bool {
		if err := s.journal.AppendLiquidation(LiquidationCmd{
			Market: market.ID, Account: pos.Account, Mark: mark, Limit: limit, At: now.Unix(),
		}); err != nil {
			_ = s.degrade(fmt.Errorf("services: perp: journal liquidation: %w", err))
			return false
		}
		var res perp.Result
		var err error
		if limit == 0 {
			res, err = s.engine.Liquidate(market.ID, pos.Account, nil)
		} else {
			res, err = s.engine.LiquidateLimit(market.ID, pos.Account, nil, limit)
		}
		if err != nil {
			if errors.Is(err, perp.ErrOrderNotFound) {
				return false // Already closed by an earlier rung or another path.
			}
			_ = s.degrade(fmt.Errorf("services: perp: liquidate %s: %w", pos.Account.Hex(), err))
			return false
		}
		if len(res.Trades) == 0 {
			return false
		}
		results = append(results, res)
		limits = append(limits, limit)
		return true
	}

	// Rung 1: a limit IOC priced off the mark. Closing a long sells, so the floor
	// is mark*(1-slippage); closing a short buys, so the ceiling is mark*(1+slip).
	// This is what keeps a liquidation from dumping into a thin book at any price.
	if limit, ok := s.liquidationLimit(market, pos, mark); ok {
		appendRung(limit)
	}
	// Rung 2: sweep whatever the limit pass left, at any price. Without this a
	// position could sit liquidatable forever because the top of the book was
	// better than the limit.
	if _, stillOpen := s.engine.Position(market.ID, pos.Account); stillOpen {
		appendRung(0)
	}

	if len(results) == 0 {
		return LiquidationEvent{}, perp.Result{}, false
	}

	merged := perp.Result{Trades: nil}
	for _, r := range results {
		merged.Trades = append(merged.Trades, r.Trades...)
		merged.Releases = append(merged.Releases, r.Releases...)
		merged.PnL = append(merged.PnL, r.PnL...)
		merged.Fees = append(merged.Fees, r.Fees...)
	}

	// Realize PnL before releasing margin, exactly as the normal place path does.
	if err := s.settlePnL(merged.PnL); err != nil {
		_ = s.degrade(err)
		return LiquidationEvent{}, perp.Result{}, false
	}
	// A forced close pays its fee like any other trade: the liquidated account is
	// not exempt, and the fee is capped by its remaining balance.
	if err := s.settleFees(merged.Fees); err != nil {
		_ = s.degrade(err)
		return LiquidationEvent{}, perp.Result{}, false
	}
	for _, r := range merged.Releases {
		if err := s.holds.Release(r.Account, market.Quote, r.Amount); err != nil {
			_ = s.degrade(err)
			return LiquidationEvent{}, perp.Result{}, false
		}
	}

	// Pay the liquidator out of the position's released margin, never out of thin
	// air: the fee is capped by what the close actually freed.
	fee = s.payLiquidationFee(pos.Account, market.Quote, fee, merged.Releases)

	// Report what actually closed, not what was requested.
	closed := uint256.NewInt(0)
	for _, t := range merged.Trades {
		if t.Qty != nil {
			closed.Add(closed, t.Qty)
		}
	}
	_, stillOpen := s.engine.Position(market.ID, pos.Account)

	_ = limits
	return LiquidationEvent{
		Market: market.ID, Account: pos.Account, Size: closed, Mark: mark,
		Equity: equity, Fee: fee, Partial: stillOpen, At: now.Unix(),
	}, merged, true
}

// liquidationLimit computes the limit-IOC price for the first liquidation rung:
// the mark moved against the position by the configured slippage. It returns
// false when the market disables the limit rung.
//
// nil SlippageE18 means the default; an explicit zero means "sweep only", which
// is how an operator opts back into the pre-ladder behaviour.
func (s *PerpOrderBookService) liquidationLimit(market perp.Market, pos perp.Position, mark perp.Price) (perp.Price, bool) {
	slip := market.RiskConfig.SlippageE18
	if slip == nil {
		slip = perp.DefaultSlippage()
	}
	if slip.IsZero() {
		return 0, false
	}
	// bound = mark * (1e18 ± slip) / 1e18, computed in 256 bits.
	e18 := uint256.NewInt(1_000_000_000_000_000_000)
	m := uint256.NewInt(uint64(mark))
	adj := new(uint256.Int).Mul(m, slip)
	adj.Div(adj, e18)
	if adj.IsZero() {
		adj = uint256.NewInt(1)
	}

	var out *uint256.Int
	if pos.Side == perp.SideLong {
		// Selling: accept prices down to mark - adj, but never below 1.
		if m.Cmp(adj) <= 0 {
			return 1, true
		}
		out = new(uint256.Int).Sub(m, adj)
	} else {
		out = new(uint256.Int).Add(m, adj)
	}
	if out.IsZero() || !out.IsUint64() {
		return 0, false
	}
	return perp.Price(out.Uint64()), true
}

// canFill reports whether a liquidation of pos has anything to trade against:
// at least one resting order on the opposite side owned by someone else. Caller
// must hold s.mu.
//
// A liquidated account's own orders on that side are skipped by STP, so they do
// not count — treating them as liquidity would journal a liquidation that cannot
// fill.
func (s *PerpOrderBookService) canFill(market perp.MarketID, pos perp.Position) bool {
	// A long is closed by selling into bids; a short by buying from asks.
	longs, shorts := s.engine.Book(market, 64)
	var levels []perp.LevelView
	if pos.Side == perp.SideLong {
		levels = longs
	} else {
		levels = shorts
	}
	for _, l := range levels {
		if l.Total != nil && !l.Total.IsZero() {
			return true
		}
	}
	return false
}

// payLiquidationFee moves up to fee from the liquidated account to the insurance
// fund, and returns what was actually charged.
//
// The cap is the position's RELEASED MARGIN, not the account's whole balance: the
// fee is a liquidation cost paid out of the margin that was backing this position,
// and charging from the full balance would reach funds belonging to other
// positions in the same quote asset (margin is isolated per market, but the asset
// is shared).
func (s *PerpOrderBookService) payLiquidationFee(account ledger.AccountID, asset ledger.AssetID, fee *uint256.Int, releases []perp.MarginRelease) *uint256.Int {
	if fee.IsZero() {
		return uint256.NewInt(0)
	}
	// Sum what this close released for the account.
	released := uint256.NewInt(0)
	for _, r := range releases {
		if r.Account == account && r.Amount != nil {
			released.Add(released, r.Amount)
		}
	}
	payable := new(uint256.Int).Set(fee)
	if released.Cmp(payable) < 0 {
		payable = new(uint256.Int).Set(released)
	}
	// The account may still have spent the released margin on its loss; never
	// overdraw what it holds right now.
	if bal := s.ledger.Balance(account, asset); bal.Cmp(payable) < 0 {
		payable = new(uint256.Int).Set(bal)
	}
	if payable.IsZero() {
		return uint256.NewInt(0)
	}

	ref := liquidationFeeRef(account, asset)
	if !s.ledger.WasApplied(ref) {
		if err := s.ledger.Apply(ledger.TransferOp{
			From: account, To: s.insuranceFund, Asset: asset, Amount: payable, Ref: ref,
		}); err != nil {
			// A fee failure must never block a liquidation that already closed the
			// position; the trade is the important part.
			return uint256.NewInt(0)
		}
	}
	return payable
}

// liquidationFeeRef keys the fee transfer on the account and asset only. The fee
// is charged at most once per account per asset, so a partial liquidation followed
// by a full one in the same scan cannot charge twice.
func liquidationFeeRef(account ledger.AccountID, asset ledger.AssetID) string {
	return fmt.Sprintf("perp:liqfee:%s:%d", account.Hex(), asset)
}


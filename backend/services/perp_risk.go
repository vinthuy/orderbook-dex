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
// trader's margin.
func (s *PerpOrderBookService) SetInsuranceFund(a ledger.AccountID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insuranceFund = a
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
	for id, market := range s.markets {
		mark, ok := s.tracker.LastMark(id)
		if !ok {
			continue
		}
		if s.tracker.Stale(id, now) {
			// A stale index means the mark is a fallback, not a live view.
			continue
		}
		for _, p := range s.engine.Positions(id) {
			bad, err := market.Liquidatable(&p, mark)
			if err != nil || !bad {
				continue
			}
			candidates = append(candidates, target{market: market, mark: mark, pos: p})
		}
	}
	s.mu.Unlock()

	for _, c := range candidates {
		s.liquidate(c.market, c.pos, c.mark, now)
	}
}

// liquidate force-closes one position and reports it.
func (s *PerpOrderBookService) liquidate(market perp.Market, pos perp.Position, mark perp.Price, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return
	}

	equity, err := market.Equity(pos.Side, pos.Size, pos.EntryPrice, mark, pos.Margin)
	if err != nil {
		return
	}
	fee, err := market.LiquidatorFee(pos.Size, mark)
	if err != nil {
		fee = uint256.NewInt(0)
	}

	res, err := s.engine.Liquidate(market.ID, pos.Account, nil)
	if err != nil {
		if errors.Is(err, perp.ErrOrderNotFound) {
			return // Already closed by another path.
		}
		_ = s.degrade(fmt.Errorf("services: perp: liquidate %s: %w", pos.Account.Hex(), err))
		return
	}

	// Realize PnL before releasing margin, exactly as the normal place path does.
	if err := s.settlePnL(res.PnL); err != nil {
		_ = s.degrade(err)
		return
	}
	for _, r := range res.Releases {
		if err := s.holds.Release(r.Account, market.Quote, r.Amount); err != nil {
			_ = s.degrade(err)
			return
		}
	}

	// Pay the liquidator (the operator in v1) out of the position's released
	// margin, never out of thin air: if the margin is exhausted the fee is capped.
	if !fee.IsZero() {
		s.payLiquidationFee(pos.Account, market.Quote, fee, res.Trades)
	}

	remaining, stillOpen := s.engine.Position(market.ID, pos.Account)
	_ = remaining
	s.publish(res)
	_ = s.bus.Publish(Event{Topic: TopicPerpLiquidation, Data: LiquidationEvent{
		Market: market.ID, Account: pos.Account, Size: pos.Size, Mark: mark,
		Equity: equity, Fee: fee, Partial: stillOpen, At: now.Unix(),
	}})
}

// payLiquidationFee moves up to fee from the liquidated account's balance to the
// insurance fund. It never overdraws: the transfer is capped at the account's
// remaining balance, so a liquidation cannot mint a negative balance.
func (s *PerpOrderBookService) payLiquidationFee(account ledger.AccountID, asset ledger.AssetID, fee *uint256.Int, trades []perp.Trade) {
	if fee.IsZero() || len(trades) == 0 {
		return
	}
	balance := s.ledger.Balance(account, asset)
	payable := new(uint256.Int).Set(fee)
	if balance.Cmp(payable) < 0 {
		payable = new(uint256.Int).Set(balance)
	}
	if payable.IsZero() {
		return
	}
	ref := fmt.Sprintf("perp:liqfee:%d:%s", trades[len(trades)-1].ID, account.Hex())
	if s.ledger.WasApplied(ref) {
		return
	}
	if err := s.ledger.Apply(ledger.TransferOp{
		From: account, To: s.insuranceFund, Asset: asset, Amount: payable, Ref: ref,
	}); err != nil {
		// A fee failure must never block a liquidation that already closed the
		// position; the trade is the important part.
		return
	}
}

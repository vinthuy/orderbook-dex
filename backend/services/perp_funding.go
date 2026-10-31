package services

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// fundingDebtKey identifies one account's unpaid funding in one market.
type fundingDebtKey struct {
	Account ledger.AccountID
	Market  perp.MarketID
}

// FundingSkip records one leg that could not be collected this round. The
// shortfall is carried as a debt and retried in a later round.
type FundingSkip struct {
	Account ledger.AccountID
	Market  perp.MarketID
	Owed    *uint256.Int
}

// FundingResult is the outcome of one funding round.
type FundingResult struct {
	Market perp.MarketID
	Index  uint64
	Rate   string
	Mark   perp.Price
	Legs   []FundingLeg
	Skips  []FundingSkip
}

// FundingState is the public snapshot of a market's last funding round.
type FundingState struct {
	Market      perp.MarketID
	Rate        string
	Mark        perp.Price
	LastRoundAt int64
	LastIndex   uint64
	TotalDebt   *uint256.Int
}

func fundingRef(m perp.MarketID, index uint64, payer, receiver ledger.AccountID) string {
	return fmt.Sprintf("perp:funding:%d:%d:%s:%s", m, index, payer.Hex(), receiver.Hex())
}

// SettleFunding runs one funding round for a market (spec §3.4).
//
// Every open position pays or receives floor(notional(size, mark) * |rate| / 1e18)
// in the market's quote asset: a positive rate means longs pay shorts, a negative
// rate means shorts pay longs. The round is journaled BEFORE any ledger mutation,
// and every transfer's idempotency key contains the round index, so replaying the
// WAL can never pay a round twice.
//
// A payer who cannot cover its payment has the shortfall recorded as a debt and
// retried next round. Unlike a failed PnL settlement, funding NEVER degrades the
// whole book: one broke account is that account's problem, not the market's.
func (s *PerpOrderBookService) SettleFunding(m perp.MarketID, index uint64, mark perp.Price, rateScaled *big.Int, at int64) (FundingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return FundingResult{}, s.fatalErr
	}
	market, ok := s.markets[m]
	if !ok {
		return FundingResult{}, perp.ErrUnknownMarket
	}
	res := FundingResult{Market: m, Index: index, Mark: mark}
	if rateScaled != nil {
		res.Rate = rateScaled.String()
	}
	if rateScaled == nil {
		rateScaled = new(big.Int)
	}
	if mark == 0 {
		// No mark means no pricing: the round did not happen at all.
		return res, nil
	}

	legs, skips, err := s.planFunding(market, index, mark, rateScaled, at)
	if err != nil {
		return FundingResult{}, err
	}
	res.Legs = legs
	res.Skips = skips
	// A zero rate still completes a round (there are simply no legs); recording
	// it keeps the round index advancing, which is what the WAL replay and the
	// scheduler's interval logic rely on.
	s.recordFundingState(market, index, res.Rate, mark, at)
	return res, nil
}

// planFunding computes the netted legs, journals them, and applies them.
func (s *PerpOrderBookService) planFunding(market perp.Market, index uint64, mark perp.Price, rateScaled *big.Int, at int64) ([]FundingLeg, []FundingSkip, error) {
	if rateScaled.Sign() == 0 {
		// A zero rate is a legitimate round with no payments, but it is still
		// journaled so replay reconstructs the same round sequence.
		if err := s.journal.AppendFunding(FundingCmd{
			Market: market.ID, Index: index, Rate: "0", Mark: mark, At: at,
		}); err != nil {
			return nil, nil, fmt.Errorf("services: perp: journal funding: %w", err)
		}
		return nil, nil, nil
	}
	positive := rateScaled.Sign() > 0

	payers := make(map[ledger.AccountID]*uint256.Int)
	receivers := make(map[ledger.AccountID]*uint256.Int)
	var accounts []ledger.AccountID

	for _, p := range s.engine.Positions(market.ID) {
		amt, err := market.FundingAmount(p.Size, mark, rateScaled)
		if err != nil {
			return nil, nil, err
		}
		if amt.IsZero() {
			continue
		}
		// Longs pay on a positive rate; shorts pay on a negative one.
		pays := (positive && p.Side == perp.SideLong) || (!positive && p.Side == perp.SideShort)
		side := receivers
		if pays {
			side = payers
		}
		if cur, ok := side[p.Account]; ok {
			cur.Add(cur, amt)
		} else {
			side[p.Account] = new(uint256.Int).Set(amt)
			accounts = append(accounts, p.Account)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		return bytes.Compare(accounts[i][:], accounts[j][:]) < 0
	})

	// Each payer's collectible amount is capped by its available balance plus
	// whatever debt it already carried; the remainder becomes new debt.
	var legs []FundingLeg
	var skips []FundingSkip
	var newDebts = make(map[fundingDebtKey]*uint256.Int)

	for _, payer := range accounts {
		paid := payers[payer]
		if paid == nil {
			continue
		}
		key := fundingDebtKey{Account: payer, Market: market.ID}
		prevDebt := s.debts[key]
		if prevDebt == nil {
			prevDebt = uint256.NewInt(0)
		}
		owed := new(uint256.Int).Add(new(uint256.Int).Set(paid), prevDebt)

		avail := s.holds.Available(payer, market.Quote)
		collectible := new(uint256.Int).Set(owed)
		if avail.Cmp(owed) < 0 {
			collectible = new(uint256.Int).Set(avail)
		}
		shortfall := new(uint256.Int).Sub(owed, collectible)

		// Never collect more from a payer than it owes this round plus old debt,
		// and never credit a receiver more than it is due.
		remaining := new(uint256.Int).Set(collectible)
		for _, receiver := range accounts {
			due := receivers[receiver]
			if due == nil || due.IsZero() || remaining.IsZero() {
				continue
			}
			amt := new(uint256.Int).Set(remaining)
			if amt.Cmp(due) > 0 {
				amt = new(uint256.Int).Set(due)
			}
			legs = append(legs, FundingLeg{Payer: payer, Receiver: receiver, Amount: amt})
			remaining.Sub(remaining, amt)
			receivers[receiver] = new(uint256.Int).Sub(due, amt)
		}

		if shortfall.Sign() > 0 {
			skips = append(skips, FundingSkip{Account: payer, Market: market.ID, Owed: shortfall})
			newDebts[key] = shortfall
		} else if prevDebt.Sign() > 0 {
			newDebts[key] = uint256.NewInt(0)
		}
	}

	// Journal before mutating: the WAL is the truth source for replay.
	if err := s.journal.AppendFunding(FundingCmd{
		Market: market.ID, Index: index, Rate: rateScaled.String(), Mark: mark, At: at, Legs: legs,
	}); err != nil {
		return nil, nil, fmt.Errorf("services: perp: journal funding: %w", err)
	}

	applied := make([]FundingLeg, 0, len(legs))
	for _, l := range legs {
		ref := fundingRef(market.ID, index, l.Payer, l.Receiver)
		if s.ledger.WasApplied(ref) {
			applied = append(applied, l)
			continue
		}
		if err := s.ledger.Apply(ledger.TransferOp{
			From: l.Payer, To: l.Receiver, Asset: market.Quote, Amount: l.Amount, Ref: ref,
		}); err != nil {
			// Availability is checked under the Holds lock while the ledger has
			// its own lock, so a concurrent spend can still surface here. Treat
			// it as a debt for that payer instead of degrading the book.
			key := fundingDebtKey{Account: l.Payer, Market: market.ID}
			if cur, ok := newDebts[key]; ok {
				cur.Add(cur, l.Amount)
			} else {
				newDebts[key] = new(uint256.Int).Set(l.Amount)
			}
			skips = append(skips, FundingSkip{Account: l.Payer, Market: market.ID, Owed: l.Amount})
			continue
		}
		applied = append(applied, l)
	}

	for key, amt := range newDebts {
		if amt.IsZero() {
			delete(s.debts, key)
			continue
		}
		s.debts[key] = amt
	}
	return applied, skips, nil
}

// recordFundingState stores the round's public snapshot.
func (s *PerpOrderBookService) recordFundingState(market perp.Market, index uint64, rate string, mark perp.Price, at int64) {
	st := s.funding[market.ID]
	st.Market = market.ID
	st.Rate = rate
	st.Mark = mark
	st.LastRoundAt = at
	st.LastIndex = index
	s.funding[market.ID] = st
}

// FundingDebtOf returns the funding debt an account owes in a market.
func (s *PerpOrderBookService) FundingDebtOf(a ledger.AccountID, m perp.MarketID) *uint256.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.debts[fundingDebtKey{Account: a, Market: m}]; ok {
		return new(uint256.Int).Set(d)
	}
	return uint256.NewInt(0)
}

// FundingState returns the last funding round's snapshot for a market.
func (s *PerpOrderBookService) FundingState(m perp.MarketID) (FundingState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.funding[m]
	if !ok || st.LastIndex == 0 {
		return FundingState{}, false
	}
	total := uint256.NewInt(0)
	for k, v := range s.debts {
		if k.Market == m && v != nil {
			total.Add(total, v)
		}
	}
	st.TotalDebt = total
	return st, true
}

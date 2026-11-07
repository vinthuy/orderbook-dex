package services

import (
	"errors"
	"fmt"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// ErrNoCollateral is returned when a market does not price the requested asset as
// margin collateral.
var ErrNoCollateral = errors.New("services: asset is not accepted as collateral for this market")

// CollateralValue quotes one asset's credited margin value in a market's quote
// asset, applying the configured haircut.
func (s *PerpOrderBookService) CollateralValue(m perp.MarketID, asset ledger.AssetID, amount *uint256.Int) (*uint256.Int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	market, ok := s.markets[m]
	if !ok {
		return nil, perp.ErrUnknownMarket
	}
	if asset == market.Quote {
		return new(uint256.Int).Set(amount), nil
	}
	c, ok := market.CollateralFor(asset)
	if !ok {
		return nil, ErrNoCollateral
	}
	return c.CollateralValue(amount)
}

// Pledge converts a non-quote collateral asset into the market's quote balance,
// so it can back margin.
//
// Why convert rather than let the kernel freeze foreign assets: the entire margin
// engine — order-level ceil freezes, position-level floor accounting, the
// conservation invariant, liquidation releases — is expressed in ONE asset per
// market. Teaching it to freeze several would touch every one of those invariants
// for a benefit the conversion already provides. Converting keeps a single unit of
// account and makes the haircut the only new risk input.
//
// The conversion is a ledger transfer at the haircut-adjusted rate, so the ledger
// total is preserved: the collateral asset leaves the account, quote enters it at
// a discounted value, and the discount is not minted anywhere — it simply is not
// credited.
func (s *PerpOrderBookService) Pledge(m perp.MarketID, account ledger.AccountID, asset ledger.AssetID, amount *uint256.Int) (*uint256.Int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return nil, s.fatalErr
	}
	market, ok := s.markets[m]
	if !ok {
		return nil, perp.ErrUnknownMarket
	}
	if amount == nil || amount.IsZero() {
		return uint256.NewInt(0), nil
	}
	if asset == market.Quote {
		return nil, fmt.Errorf("%w: %d is already the margin asset", perp.ErrBadRequest, asset)
	}
	c, ok := market.CollateralFor(asset)
	if !ok {
		return nil, fmt.Errorf("%w: asset %d", ErrNoCollateral, asset)
	}

	credited, err := c.CollateralValue(amount)
	if err != nil {
		return nil, err
	}
	if credited.IsZero() {
		return nil, fmt.Errorf("%w: pledge rounds to zero", perp.ErrBadRequest)
	}

	// Debit the collateral and credit the haircut-adjusted quote in one ledger
	// operation pair, keyed so a retry cannot double-pledge.
	ref := fmt.Sprintf("perp:pledge:%s:%d:%s:%d", account.Hex(), asset, amount.Dec(), m)
	if s.ledger.WasApplied(ref) {
		return credited, nil
	}
	if err := s.ledger.Apply(
		ledger.WithdrawOp{Account: account, Asset: asset, Amount: amount, Ref: ref + ":out"},
		ledger.DepositOp{Account: account, Asset: market.Quote, Amount: credited, Ref: ref + ":in"},
	); err != nil {
		return nil, err
	}
	return credited, nil
}

// Redeem converts quote margin back into a collateral asset, returning the
// collateral received.
//
// This closes the one-way door Pledge used to be: after pledging, the asset left
// the account and nothing could bring it back. Redeem is the inverse conversion at
// the CURRENT haircut and price, which is the honest semantics — a user gets the
// asset back at today's terms, not last week's.
//
// Two guards matter:
//   - the quote spent must be free (not frozen as margin on an open position or a
//     resting order), which Holds.Available enforces;
//   - the amount must round to a non-zero quantity of collateral, so a redemption
//     cannot silently vaporise quote into nothing.
func (s *PerpOrderBookService) Redeem(m perp.MarketID, account ledger.AccountID, asset ledger.AssetID, quoteAmount *uint256.Int) (*uint256.Int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return nil, s.fatalErr
	}
	market, ok := s.markets[m]
	if !ok {
		return nil, perp.ErrUnknownMarket
	}
	if quoteAmount == nil || quoteAmount.IsZero() {
		return uint256.NewInt(0), nil
	}
	if asset == market.Quote {
		return nil, fmt.Errorf("%w: %d is already the margin asset", perp.ErrBadRequest, asset)
	}
	c, ok := market.CollateralFor(asset)
	if !ok {
		return nil, fmt.Errorf("%w: asset %d", ErrNoCollateral, asset)
	}
	if c.Price == 0 || c.HaircutE18 == nil || c.HaircutE18.IsZero() {
		return nil, fmt.Errorf("%w: asset %d has no usable price or haircut", ErrNoCollateral, asset)
	}

	// quote = collateral * price * haircut / 1e18, so
	// collateral = floor(quote * 1e18 / (price * haircut)).
	denom, overflow := new(uint256.Int).MulOverflow(uint256.NewInt(uint64(c.Price)), c.HaircutE18)
	if overflow {
		return nil, perp.ErrAmountTooLarge
	}
	if denom.IsZero() {
		return nil, fmt.Errorf("%w: asset %d values to zero per unit", ErrNoCollateral, asset)
	}
	num, overflow := new(uint256.Int).MulOverflow(quoteAmount, uint256.NewInt(scaleE18()))
	if overflow {
		return nil, perp.ErrAmountTooLarge
	}
	collateralOut := new(uint256.Int).Div(num, denom)
	if collateralOut.IsZero() {
		return nil, fmt.Errorf("%w: redemption rounds to zero collateral", perp.ErrBadRequest)
	}

	// Only free quote can be spent: margin backing a position or a resting order is
	// already committed, and spending it would leave the account unable to cover a
	// loss it is contractually exposed to.
	available := s.holds.Available(account, market.Quote)
	if available.Cmp(quoteAmount) < 0 {
		return nil, ledger.ErrInsufficientBalance
	}

	ref := fmt.Sprintf("perp:redeem:%s:%d:%s:%d", account.Hex(), asset, quoteAmount.Dec(), m)
	if s.ledger.WasApplied(ref) {
		return collateralOut, nil
	}
	if err := s.ledger.Apply(
		ledger.WithdrawOp{Account: account, Asset: market.Quote, Amount: quoteAmount, Ref: ref + ":out"},
		ledger.DepositOp{Account: account, Asset: asset, Amount: collateralOut, Ref: ref + ":in"},
	); err != nil {
		return nil, err
	}
	return collateralOut, nil
}

// CollateralBalances reports an account's pledgeable collateral for a market: the
// asset, its balance, its haircut, and the quote value it would credit.
type CollateralBalance struct {
	Asset      ledger.AssetID
	Balance    *uint256.Int
	HaircutE18 *uint256.Int
	Price      perp.Price
	Value      *uint256.Int
}

// CollateralBalances returns every accepted collateral asset and its current
// value for an account. The market's own quote asset is included first with a 1:1
// value, so a client can render one consistent list.
func (s *PerpOrderBookService) CollateralBalances(m perp.MarketID, account ledger.AccountID) ([]CollateralBalance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	market, ok := s.markets[m]
	if !ok {
		return nil, perp.ErrUnknownMarket
	}

	out := make([]CollateralBalance, 0, len(market.Collateral)+1)
	quoteBal := s.ledger.Balance(account, market.Quote)
	out = append(out, CollateralBalance{
		Asset: market.Quote, Balance: quoteBal,
		HaircutE18: uint256.NewInt(scaleE18()), Price: 1, Value: new(uint256.Int).Set(quoteBal),
	})
	for _, c := range market.Collateral {
		bal := s.ledger.Balance(account, c.Asset)
		val, err := c.CollateralValue(bal)
		if err != nil {
			return nil, err
		}
		out = append(out, CollateralBalance{
			Asset: c.Asset, Balance: bal, HaircutE18: c.HaircutE18, Price: c.Price, Value: val,
		})
	}
	return out, nil
}

// scaleE18 returns 1e18 as a uint256.
func scaleE18() uint64 { return 1_000_000_000_000_000_000 }

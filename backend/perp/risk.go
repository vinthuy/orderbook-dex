package perp

import (
	"math/big"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
)

// Default risk parameters (spec §3.1). The maintenance rate is deliberately far
// below the initial margin so a position has room to move before it is closed
// out: at 10x, initial margin is 10% of notional and maintenance is 0.5%, so the
// mark must move roughly 9.5% against the position before liquidation.
var (
	defaultMaintenanceRateE18 = uint256.NewInt(5_000_000_000_000_000)  // 0.5%
	defaultLiquidatorFeeE18   = uint256.NewInt(1_000_000_000_000_000)  // 0.1%
	defaultSlippageE18        = uint256.NewInt(10_000_000_000_000_000) // 1%
)

// maintenanceRate returns the configured maintenance margin rate, or the default.
func (c RiskConfig) maintenanceRate() *uint256.Int {
	if c.MaintenanceMarginRateE18 == nil || c.MaintenanceMarginRateE18.IsZero() {
		return new(uint256.Int).Set(defaultMaintenanceRateE18)
	}
	return c.MaintenanceMarginRateE18
}

// liquidatorFee returns the configured liquidation fee, or the default.
func (c RiskConfig) liquidatorFee() *uint256.Int {
	if c.LiquidatorFeeE18 == nil {
		return new(uint256.Int).Set(defaultLiquidatorFeeE18)
	}
	return c.LiquidatorFeeE18
}

// MarginMode selects how equity is pooled when judging whether an account is
// liquidatable.
type MarginMode uint8

const (
	// MarginIsolated judges each position on its own margin (the 6a default).
	MarginIsolated MarginMode = iota
	// MarginCross judges all of an account's positions in markets of the same
	// margin scope together: one position's profit can back another's loss, so a
	// losing leg does not get closed while a winning leg sits on gains that would
	// have covered it.
	MarginCross
)

func (m MarginMode) String() string {
	if m == MarginCross {
		return "cross"
	}
	return "isolated"
}

// RiskConfig holds a market's risk parameters. The zero value means defaults.
type RiskConfig struct {
	// MaintenanceMarginRateE18 is the fraction of notional a position must keep
	// as equity, scaled by 1e18.
	MaintenanceMarginRateE18 *uint256.Int
	// LiquidatorFeeE18 is the fraction of notional paid to the liquidator (the
	// operator in v1), scaled by 1e18. Zero is a valid "no fee".
	LiquidatorFeeE18 *uint256.Int
	// SlippageE18 is how far from the mark the first liquidation rung will trade,
	// scaled by 1e18 (default 1%). Zero disables the limit rung, leaving only the
	// market sweep — which is also the pre-ladder behaviour.
	SlippageE18 *uint256.Int
	// Mode selects isolated or cross judgement. Isolated keeps the 6a semantics
	// exactly; cross is opt-in per market.
	Mode MarginMode
}

// DefaultSlippage returns the default first-rung liquidation slippage.
func DefaultSlippage() *uint256.Int { return new(uint256.Int).Set(defaultSlippageE18) }

// slippage returns the configured first-rung slippage, or the default.
func (c RiskConfig) slippage() *uint256.Int {
	if c.SlippageE18 == nil || c.SlippageE18.IsZero() {
		return new(uint256.Int).Set(defaultSlippageE18)
	}
	return c.SlippageE18
}

// MaintenanceMargin returns ceil(notional(size, mark) * rate / 1e18): the equity
// a position must hold to stay open.
func (m Market) MaintenanceMargin(size *uint256.Int, mark Price) (*uint256.Int, error) {
	if mark == 0 {
		return nil, ErrBadRequest
	}
	if size == nil || size.IsZero() {
		return uint256.NewInt(0), nil
	}
	notional, err := m.notionalChecked(size, mark, false)
	if err != nil {
		return nil, err
	}
	return mulRateCeil(notional, m.RiskConfig.maintenanceRate())
}

// LiquidatorFee is the fee a liquidation pays, ceil(notional(size, mark) * fee / 1e18).
func (m Market) LiquidatorFee(size *uint256.Int, mark Price) (*uint256.Int, error) {
	if mark == 0 {
		return nil, ErrBadRequest
	}
	if size == nil || size.IsZero() {
		return uint256.NewInt(0), nil
	}
	notional, err := m.notionalChecked(size, mark, false)
	if err != nil {
		return nil, err
	}
	return mulRateCeil(notional, m.RiskConfig.liquidatorFee())
}

// mulRateCeil returns ceil(amount * rateE18 / 1e18).
func mulRateCeil(amount, rateE18 *uint256.Int) (*uint256.Int, error) {
	if rateE18 == nil || rateE18.IsZero() {
		return uint256.NewInt(0), nil
	}
	prod, overflow := new(uint256.Int).MulOverflow(amount, rateE18)
	if overflow {
		return nil, ErrAmountTooLarge
	}
	return divCeil(prod, uint256.NewInt(scaleE18))
}

// Equity returns margin plus mark-price unrealized PnL: what the position is
// worth if closed at the mark. It is signed because a large adverse move can
// make it negative (that is the shortfall the insurance fund covers).
func (m Market) Equity(side Side, size *uint256.Int, entry, mark Price, margin *uint256.Int) (*big.Int, error) {
	pnl, err := m.UnrealizedPnL(side, size, entry, mark)
	if err != nil {
		return nil, err
	}
	eq := new(big.Int).Set(pnl)
	if margin != nil {
		eq.Add(eq, margin.ToBig())
	}
	return eq, nil
}

// AccountRisk is an account's aggregate risk exposure in one margin scope: every
// position in markets that share the same quote asset.
type AccountRisk struct {
	Account            ledger.AccountID
	Asset              ledger.AssetID
	Equity             *big.Int     // Σ margin + Σ unrealized PnL (signed)
	MaintenanceMargin  *uint256.Int // Σ per-position maintenance requirement
	Liquidatable       bool
	// Worst is the position with the largest shortfall against its own
	// maintenance requirement, used to pick the first leg to close.
	WorstMarket MarketID
}

// AccountRiskFor aggregates every position the account holds in markets of the
// given margin scope. marks must contain a fresh mark for each market; a market
// without one is skipped, because judging risk on a missing price would
// mis-liquidate.
//
// In isolated mode each position is judged on its own margin, so the aggregate is
// only informative; callers that need the isolated verdict use Liquidatable.
func AccountRiskFor(account ledger.AccountID, positions []Position, markets map[MarketID]Market, marks map[MarketID]Price) (AccountRisk, error) {
	var (
		asset    ledger.AssetID
		equity   = new(big.Int)
		maint    = uint256.NewInt(0)
		worst    MarketID
		worstGap = new(big.Int)
		seen     bool
	)
	for _, p := range positions {
		market, ok := markets[p.Market]
		if !ok {
			continue
		}
		mark, ok := marks[p.Market]
		if !ok || mark == 0 {
			continue
		}
		seen = true
		asset = market.Quote

		eq, err := market.Equity(p.Side, p.Size, p.EntryPrice, mark, p.Margin)
		if err != nil {
			return AccountRisk{}, err
		}
		equity.Add(equity, eq)

		req, err := market.MaintenanceMargin(p.Size, mark)
		if err != nil {
			return AccountRisk{}, err
		}
		maint.Add(maint, req)

		// Per-position shortfall picks the worst leg.
		legEq, err := market.Equity(p.Side, p.Size, p.EntryPrice, mark, p.Margin)
		if err != nil {
			return AccountRisk{}, err
		}
		gap := new(big.Int).Sub(req.ToBig(), legEq)
		if gap.Cmp(worstGap) > 0 {
			worstGap = gap
			worst = p.Market
		}
	}
	if !seen {
		return AccountRisk{}, nil
	}
	return AccountRisk{
		Account:           account,
		Asset:             asset,
		Equity:            equity,
		MaintenanceMargin: maint,
		Liquidatable:      equity.Cmp(maint.ToBig()) < 0,
		WorstMarket:       worst,
	}, nil
}

// Liquidatable reports whether a position's equity has fallen below its
// maintenance margin (spec §3.1). It requires a mark price; without one the
// caller must not liquidate (a stale or missing price would misjudge the risk).
//
// This is the ISOLATED verdict. A cross-margin market must use AccountRiskFor
// instead, or a losing leg would be closed while a winning leg's profit sits
// unused.
func (m Market) Liquidatable(p *Position, mark Price) (bool, error) {
	if p == nil || p.Size == nil || p.Size.IsZero() {
		return false, nil
	}
	req, err := m.MaintenanceMargin(p.Size, mark)
	if err != nil {
		return false, err
	}
	eq, err := m.Equity(p.Side, p.Size, p.EntryPrice, mark, p.Margin)
	if err != nil {
		return false, err
	}
	return eq.Cmp(req.ToBig()) < 0, nil
}

// FeeRates configures per-fill fees, both 1e18-scaled fractions of notional.
// The zero value means no fees, so an existing deployment is unchanged.
type FeeRates struct {
	MakerE18 *uint256.Int
	TakerE18 *uint256.Int
}

// fee returns the rate for a role, treating nil as zero.
func (f FeeRates) fee(role FeeRole) *uint256.Int {
	r := f.TakerE18
	if role == FeeMaker {
		r = f.MakerE18
	}
	if r == nil {
		return uint256.NewInt(0)
	}
	return r
}

// Enabled reports whether any fee is configured.
func (f FeeRates) Enabled() bool {
	return (f.MakerE18 != nil && !f.MakerE18.IsZero()) || (f.TakerE18 != nil && !f.TakerE18.IsZero())
}

// Fee returns floor(notional(qty, price) * rate / 1e18) for one fill. Flooring
// keeps a dust fill from owing more than it is worth.
func (m Market) Fee(qty *uint256.Int, price Price, role FeeRole) (*uint256.Int, error) {
	rate := m.FeeRates.fee(role)
	if rate.IsZero() || qty == nil || qty.IsZero() || price == 0 {
		return uint256.NewInt(0), nil
	}
	notional, err := m.notionalChecked(qty, price, false)
	if err != nil {
		return nil, err
	}
	prod, overflow := new(uint256.Int).MulOverflow(notional, rate)
	if overflow {
		return nil, ErrAmountTooLarge
	}
	return prod.Div(prod, uint256.NewInt(scaleE18)), nil
}

// CollateralAsset is one non-quote asset accepted as margin, with the rate at
// which it is valued in the market's quote asset.
//
// The haircut is not optional: a collateral asset that is not the settlement
// asset carries price and liquidity risk the market does not control, so it is
// credited at a discount (haircutE18 = 0.9 means 90% of market value). A haircut of
// zero would credit nothing, which is a valid way to temporarily disable an asset.
type CollateralAsset struct {
	Asset    ledger.AssetID
	// HaircutE18 scales the asset's quote value, 1e18 = no haircut.
	HaircutE18 *uint256.Int
	// Price is the asset's price in the market's quote asset, in quote minor
	// units per whole unit of collateral. Must be > 0 to be usable.
	Price Price
}

// CollateralValue returns floor(amount * price * haircut / 1e18 / scale), the
// quote value credited for `amount` units of this collateral.
func (c CollateralAsset) CollateralValue(amount *uint256.Int) (*uint256.Int, error) {
	if amount == nil || amount.IsZero() || c.Price == 0 {
		return uint256.NewInt(0), nil
	}
	if c.HaircutE18 == nil || c.HaircutE18.IsZero() {
		return uint256.NewInt(0), nil
	}
	// product = amount * price * haircut, then / 1e18.
	prod, overflow := new(uint256.Int).MulOverflow(amount, uint256.NewInt(uint64(c.Price)))
	if overflow {
		return nil, ErrAmountTooLarge
	}
	prod, overflow = new(uint256.Int).MulOverflow(prod, c.HaircutE18)
	if overflow {
		return nil, ErrAmountTooLarge
	}
	return prod.Div(prod, uint256.NewInt(scaleE18)), nil
}

// CollateralFor returns the configured collateral entry for an asset.
func (m Market) CollateralFor(asset ledger.AssetID) (CollateralAsset, bool) {
	for _, c := range m.Collateral {
		if c.Asset == asset {
			return c, true
		}
	}
	return CollateralAsset{}, false
}

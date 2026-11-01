package perp

import (
	"math/big"

	"github.com/holiman/uint256"
)

// Default risk parameters (spec §3.1). The maintenance rate is deliberately far
// below the initial margin so a position has room to move before it is closed
// out: at 10x, initial margin is 10% of notional and maintenance is 0.5%, so the
// mark must move roughly 9.5% against the position before liquidation.
var (
	defaultMaintenanceRateE18 = uint256.NewInt(5_000_000_000_000_000)  // 0.5%
	defaultLiquidatorFeeE18   = uint256.NewInt(1_000_000_000_000_000)  // 0.1%
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

// RiskConfig holds a market's risk parameters. The zero value means defaults.
type RiskConfig struct {
	// MaintenanceMarginRateE18 is the fraction of notional a position must keep
	// as equity, scaled by 1e18.
	MaintenanceMarginRateE18 *uint256.Int
	// LiquidatorFeeE18 is the fraction of notional paid to the liquidator (the
	// operator in v1), scaled by 1e18. Zero is a valid "no fee".
	LiquidatorFeeE18 *uint256.Int
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

// Liquidatable reports whether a position's equity has fallen below its
// maintenance margin (spec §3.1). It requires a mark price; without one the
// caller must not liquidate (a stale or missing price would misjudge the risk).
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

package perp

import "errors"

// Sentinel errors. Callers should use errors.Is against these.
//
// These are declared in their own file (rather than engine.go) so that the
// market arithmetic in market.go — which reports ErrBadRequest and
// ErrAmountTooLarge — can reference them without a forward dependency on the
// engine.
var (
	ErrUnknownMarket  = errors.New("perp: unknown market")
	ErrBadRequest     = errors.New("perp: invalid order")
	ErrOrderNotFound  = errors.New("perp: order not found")
	ErrNotOwner       = errors.New("perp: not order owner")
	ErrAmountTooLarge = errors.New("perp: notional overflows")
)

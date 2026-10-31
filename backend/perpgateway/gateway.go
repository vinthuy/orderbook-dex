// Package perpgateway exposes the perpetual order book over HTTP and
// WebSocket. It mirrors the spot gateway's shape — EIP-712 signed instructions,
// a persistent per-account nonce log, rate limiting, and push frames — but its
// payloads describe positions and margin rather than asset deliveries.
//
// The spot gateway is untouched: perpetual orders freeze margin in the quote
// asset on both sides, and conflating the two instruction spaces would make
// nonce and signature domains ambiguous.
package perpgateway

import (
	"errors"
	"math/big"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/holiman/uint256"

	"web3-amm-dex-backend/gateway"
	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
	"web3-amm-dex-backend/services"
)

// Defaults for the perpetual gateway.
const (
	// A distinct domain keeps a spot signature from being replayed as a perp
	// instruction (and vice versa): the EIP-712 domain separator differs even
	// though the chain id and signer are the same.
	DefaultDomainName    = "Web3 Orderbook DEX Perp"
	DefaultDomainVersion = "1"
	DefaultRate          = 10.0
	DefaultBurst         = 20.0
	DefaultDepth         = 20
)

// Config configures the perpetual gateway.
type Config struct {
	ChainID       int64
	DomainName    string
	DomainVersion string
	Rate          float64
	Burst         float64
	NonceDir      string
	Depth         int
}

// ErrPerpRequired is returned by New when the perp service is nil.
var ErrPerpRequired = errors.New("perpgateway: perp order book service is required")

// Gateway assembles the perpetual gateway's routes and dependencies.
type Gateway struct {
	perp    *services.PerpOrderBookService
	bus     *services.EventBus
	verifier *Verifier
	nonce   *gateway.NonceStore
	limiter *gateway.RateLimiter
	hub     *Hub
	cfg     Config
}

// New builds the gateway. The hub subscribes to the bus immediately; Close must
// be called to release the nonce store handle.
func New(p *services.PerpOrderBookService, bus *services.EventBus, cfg Config) (*Gateway, error) {
	if p == nil {
		return nil, ErrPerpRequired
	}
	if bus == nil {
		return nil, errors.New("perpgateway: EventBus is required (must be the same bus the perp service publishes to)")
	}
	applyDefaults(&cfg)

	nonce, err := gateway.OpenNonceStore(cfg.NonceDir)
	if err != nil {
		return nil, err
	}
	hub := NewHub(p, cfg.Depth)
	hub.Subscribe(bus)

	return &Gateway{
		perp:     p,
		bus:      bus,
		verifier: NewVerifier(cfg.ChainID, cfg.DomainName, cfg.DomainVersion),
		nonce:    nonce,
		limiter:  gateway.NewRateLimiter(cfg.Rate, cfg.Burst),
		hub:      hub,
		cfg:      cfg,
	}, nil
}

func applyDefaults(cfg *Config) {
	if cfg.ChainID == 0 {
		cfg.ChainID = 13700
	}
	if cfg.DomainName == "" {
		cfg.DomainName = DefaultDomainName
	}
	if cfg.DomainVersion == "" {
		cfg.DomainVersion = DefaultDomainVersion
	}
	if cfg.Rate == 0 && cfg.Burst == 0 {
		cfg.Rate, cfg.Burst = DefaultRate, DefaultBurst
	}
	if cfg.Depth <= 0 {
		cfg.Depth = DefaultDepth
	}
}

// Router returns a gin engine with the perpetual routes registered.
func (g *Gateway) Router() *gin.Engine {
	r := gin.New()
	g.Register(r)
	return r
}

// Register mounts the /v1/perp/* and /ws/perp routes.
//
// Every route is namespaced under /perp so a deployment can mount both gateways
// on one engine without collisions, which is the point of having two gateways.
func (g *Gateway) Register(r gin.IRouter) {
	v1 := r.Group("/v1/perp")
	v1.POST("/orders", g.placeOrder)
	v1.DELETE("/orders/:id", g.cancelOrder)
	v1.GET("/orders/:id", g.getOrder)
	v1.GET("/markets", g.listMarkets)
	v1.GET("/book/:market", g.bookDepth)
	v1.GET("/positions/:account", g.accountPositions)
	v1.GET("/positions/:account/:market", g.accountPosition)
	v1.GET("/accounts/:account/balances", g.accountBalances)
	v1.GET("/mark/:market", g.markPrice)
	v1.GET("/funding/:market", g.fundingState)
	r.GET("/ws/perp", g.serveWS)
}

// Close releases the nonce store handle.
func (g *Gateway) Close() error { return g.nonce.Close() }

// balanceView is the shared JSON shape for a frozen/available bucket.
type balanceView struct {
	Asset     uint32 `json:"asset"`
	Balance   string `json:"balance"`
	Available string `json:"available"`
	Held      string `json:"held"`
}

// marketView is the JSON shape of a perpetual market.
type marketView struct {
	ID           uint32 `json:"id"`
	Symbol       string `json:"symbol"`
	Base         uint32 `json:"base"`
	Quote        uint32 `json:"quote"`
	BaseDecimals uint8  `json:"baseDecimals"`
	MaxLeverage  uint32 `json:"maxLeverage"`
	MinQty       string `json:"minQty,omitempty"`
	MaxQty       string `json:"maxQty,omitempty"`
}

func marketToView(m perp.Market) marketView {
	v := marketView{
		ID: uint32(m.ID), Symbol: m.Symbol, Base: uint32(m.Base), Quote: uint32(m.Quote),
		BaseDecimals: m.BaseDecimals, MaxLeverage: m.MaxLeverage,
	}
	if m.MinQty != nil {
		v.MinQty = m.MinQty.Dec()
	}
	if m.MaxQty != nil {
		v.MaxQty = m.MaxQty.Dec()
	}
	return v
}

// positionView is the JSON shape of one position, including mark-price risk
// figures so a client does not have to recompute them.
type positionView struct {
	Market         uint32 `json:"market"`
	Account        string `json:"account"`
	Side           string `json:"side"`
	Size           string `json:"size"`
	EntryPrice     uint64 `json:"entryPrice"`
	Margin         string `json:"margin"`
	Leverage       uint32 `json:"leverage"`
	MarkPrice      uint64 `json:"markPrice,omitempty"`
	UnrealizedPnL  string `json:"unrealizedPnl,omitempty"`
	MaintenanceMar string `json:"maintenanceMargin,omitempty"`
	Liquidatable   bool   `json:"liquidatable"`
}

func positionToView(p perp.Position, mark perp.Price, hasMark bool, m perp.Market) positionView {
	v := positionView{
		Market: uint32(p.Market), Account: p.Account.Hex(), Side: p.Side.String(),
		EntryPrice: uint64(p.EntryPrice), Leverage: p.Leverage,
	}
	if p.Size != nil {
		v.Size = p.Size.Dec()
	}
	if p.Margin != nil {
		v.Margin = p.Margin.Dec()
	}
	if hasMark {
		v.MarkPrice = uint64(mark)
		if pnl, err := m.UnrealizedPnL(p.Side, p.Size, p.EntryPrice, mark); err == nil {
			v.UnrealizedPnL = pnl.String()
		}
		if mm, err := m.MaintenanceMargin(p.Size, mark); err == nil {
			v.MaintenanceMar = mm.Dec()
		}
		if bad, err := m.Liquidatable(&p, mark); err == nil {
			v.Liquidatable = bad
		}
	}
	return v
}

// uintString renders a possibly nil amount as "0".
func uintString(v *uint256.Int) string {
	if v == nil {
		return "0"
	}
	return v.Dec()
}

// bigString renders a possibly nil signed amount as "0".
func bigString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// unixNow is overridable in tests so signature expiry can be exercised
// deterministically.
var unixNow = func() int64 { return time.Now().Unix() }

// accountID converts a recovered address into a ledger account.
func accountID(a [20]byte) ledger.AccountID { return ledger.AccountID(a) }

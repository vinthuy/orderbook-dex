// Package gateway is the HTTP/WebSocket entry point for the off-chain order
// book. It verifies EIP-712 signed orders, rate-limits per account, serializes
// instructions through a memory sequencer, and pushes order/trade/depth updates
// to subscribers.
//
// Layering: gateway depends only on services.OrderBookService's public methods
// (Place/Cancel/Engine/Holds), the services.EventBus, and exported types from
// matching and ledger. It never touches the ledger or the matching kernel's
// internals directly, and the sequencer adds no persistence of its own (the
// instruction WAL is owned by subproject 3).
package gateway

import (
	"errors"

	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/services"
)

// Defaults for the EIP-712 domain and rate limiter (spec §7).
const (
	DefaultDomainName    = "Web3 Orderbook DEX"
	DefaultDomainVersion = "1"
	DefaultRate          = 10.0 // tokens per second per account
	DefaultBurst         = 20.0 // token bucket capacity per account
	DefaultDepth         = 20   // book depth for depth snapshots
)

// Config configures the gateway. All fields are independently defaulted by
// New so the zero value is not required to be meaningful.
type Config struct {
	ChainID       int64
	DomainName    string
	DomainVersion string
	Rate          float64 // tokens per second per account; <= 0 disables limiting
	Burst         float64 // bucket capacity per account
	NonceDir      string  // nonce persistence dir; empty means memory-only
	Depth         int     // depth levels for WebSocket depth snapshots
}

// ErrOrderBookRequired is returned by New when the order book is nil.
var ErrOrderBookRequired = errors.New("gateway: order book service is required")

// Gateway assembles the gateway's HTTP router, WebSocket hub, sequencer, state
// machine, verifier, nonce store, and rate limiter around one OrderBookService.
type Gateway struct {
	ob       *services.OrderBookService
	bus      *services.EventBus
	verifier *Verifier
	nonce    *NonceStore
	limiter  *RateLimiter
	seq      *Sequencer
	state    *StateMachine
	hub      *Hub
	cfg      Config
}

// New builds the gateway and its dependencies. The WebSocket hub subscribes to
// the bus immediately; Close must be called to release the nonce store handle.
func New(ob *services.OrderBookService, bus *services.EventBus, cfg Config) (*Gateway, error) {
	if ob == nil {
		return nil, ErrOrderBookRequired
	}
	if bus == nil {
		return nil, errors.New("gateway: EventBus is required (must be the same bus the order book publishes to)")
	}
	applyDefaults(&cfg)

	nonce, err := OpenNonceStore(cfg.NonceDir)
	if err != nil {
		return nil, err
	}

	seq := NewSequencer(ob)
	state := NewStateMachine()
	hub := NewHub(ob, cfg.Depth)
	hub.Subscribe(bus)

	return &Gateway{
		ob:       ob,
		bus:      bus,
		verifier: NewVerifier(cfg.ChainID, cfg.DomainName, cfg.DomainVersion),
		nonce:    nonce,
		limiter:  NewRateLimiter(cfg.Rate, cfg.Burst),
		seq:      seq,
		state:    state,
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

// Router returns a fresh gin engine with the gateway routes registered. main
// can mount these routes onto an existing engine via Register instead.
func (g *Gateway) Router() *gin.Engine {
	r := gin.New()
	g.Register(r)
	return r
}

// Register mounts the /v1/* and /ws routes onto an existing engine.
func (g *Gateway) Register(r gin.IRouter) {
	v1 := r.Group("/v1")
	v1.POST("/orders", g.placeOrder)
	v1.DELETE("/orders/:id", g.cancelOrder)
	v1.GET("/orders/:id", g.getOrder)
	v1.GET("/markets", g.listMarkets)
	v1.GET("/book/:market", g.bookDepth)
	v1.GET("/accounts/:account/balances", g.accountBalances)
	r.GET("/ws", g.serveWS)
}

// Close releases the nonce store handle. The hub owns no OS resources beyond
// client connections, which are closed as clients disconnect.
func (g *Gateway) Close() error {
	return g.nonce.Close()
}

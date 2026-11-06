package services

import (
	"fmt"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// HealthStatus is the coarse verdict a monitor reports.
type HealthStatus string

const (
	HealthOK       HealthStatus = "ok"
	HealthDegraded HealthStatus = "degraded"
	HealthStalled  HealthStatus = "stalled"
)

// MarketHealth is one market's live risk and pricing state.
type MarketHealth struct {
	Market perp.MarketID
	Symbol string

	MarkPrice   perp.Price
	HasMark     bool
	MarkAgeSecs int64
	IndexPrice  perp.Price
	HasIndex    bool
	PriceStale  bool
	// Unpriced means no price source is configured for this market at all, as
	// distinct from a source that is merely stale.
	Unpriced bool

	OpenPositions int
	RestingOrders int
	FrozenMargin  *uint256.Int
	LastFundingAt int64
}

// PerpHealth is the full snapshot the health and metrics endpoints serve.
type PerpHealth struct {
	Status HealthStatus
	// Problems lists every reason the status is not ok, in operator-facing words.
	// An empty list with status ok is the only "all good" signal.
	Problems []string

	Degraded bool
	// TotalBadDebt is the cumulative loss the insurance fund could not cover.
	TotalBadDebt *uint256.Int
	// InsuranceFundBalance is what the fund holds now, per quote asset. Cross
	// margin is per quote asset, so a single number would be misleading.
	InsuranceFundBalance map[ledger.AssetID]*uint256.Int

	LiquidationScans    int64
	Liquidations        int64
	LiquidationsPartial int64
	ADLCount            int64
	BadDebtEvents       int64
	FundingRounds       int64

	Markets []MarketHealth
	// LastEventAt is the most recent risk event of any kind; a long silence on a
	// busy market can itself be a symptom.
	LastEventAt int64
	Now         int64
}

// PerpMonitor subscribes to the risk and pricing topics and keeps the counters
// the health endpoint reports.
//
// It is deliberately passive: it never acts on what it sees, so a monitor bug
// cannot change trading behaviour. Everything it reports is derived from state the
// service already exposes or events it already emits.
type PerpMonitor struct {
	svc *PerpOrderBookService

	mu          sync.Mutex
	lastEventAt time.Time

	liquidations        int64
	liquidationsPartial int64
	adlCount            int64
	badDebtEvents       int64
	fundingRounds       int64

	totalBadDebt *uint256.Int
}

// NewPerpMonitor builds a monitor over a service.
func NewPerpMonitor(svc *PerpOrderBookService) *PerpMonitor {
	return &PerpMonitor{svc: svc, totalBadDebt: uint256.NewInt(0)}
}

// Subscribe wires the monitor to every topic it reports on and registers it on the
// service so the health path can reach it.
func (m *PerpMonitor) Subscribe(bus *EventBus) {
	bus.Subscribe(TopicPerpDegraded, m.onDegraded)
	bus.Subscribe(TopicPerpLiquidation, m.onLiquidation)
	bus.Subscribe(TopicPerpADL, m.onADL)
	bus.Subscribe(TopicPerpBadDebt, m.onBadDebt)
	bus.Subscribe(TopicPerpFunding, m.onFunding)
	m.svc.registerMonitor(m)
}

func (m *PerpMonitor) onDegraded(Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEventAt = time.Now()
	return nil
}

func (m *PerpMonitor) onLiquidation(e Event) error {
	ev, ok := e.Data.(LiquidationEvent)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.liquidations++
	if ev.Partial {
		m.liquidationsPartial++
	}
	m.lastEventAt = time.Now()
	return nil
}

func (m *PerpMonitor) onADL(e Event) error {
	if _, ok := e.Data.(ADLEvent); !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adlCount++
	m.lastEventAt = time.Now()
	return nil
}

func (m *PerpMonitor) onBadDebt(e Event) error {
	ev, ok := e.Data.(BadDebtEvent)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.badDebtEvents++
	if ev.Amount != nil {
		m.totalBadDebt.Add(m.totalBadDebt, ev.Amount)
	}
	m.lastEventAt = time.Now()
	return nil
}

func (m *PerpMonitor) onFunding(e Event) error {
	if _, ok := e.Data.(FundingResult); !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fundingRounds++
	m.lastEventAt = time.Now()
	return nil
}

// noRiskEventsFor is how long a market with open positions may go without any
// risk event before the monitor calls it stalled.
const noRiskEventsFor = 10 * time.Minute

// Snapshot builds the current health report.
func (m *PerpMonitor) Snapshot(now time.Time) PerpHealth {
	h := PerpHealth{Status: HealthOK, Now: now.Unix()}
	h.Markets = m.svc.marketHealth(now)
	h.LiquidationScans = int64(m.svc.LiquidationScans())

	m.mu.Lock()
	h.Liquidations = m.liquidations
	h.LiquidationsPartial = m.liquidationsPartial
	h.ADLCount = m.adlCount
	h.BadDebtEvents = m.badDebtEvents
	h.FundingRounds = m.fundingRounds
	h.TotalBadDebt = new(uint256.Int).Set(m.totalBadDebt)
	h.LastEventAt = m.lastEventAt.Unix()
	m.mu.Unlock()

	h.Degraded = m.svc.isDegraded()
	h.InsuranceFundBalance = m.svc.insuranceFundBalances()

	if h.Degraded {
		h.Status = HealthDegraded
		h.Problems = append(h.Problems, "service degraded: restart required")
	}

	// A market with open positions but no usable price cannot be risk-managed: the
	// scanner skips it, so nobody is being liquidated who should be. This is the
	// exact failure the old hard-coded /health reported as "ok".
	hasPositions := false
	for _, mh := range h.Markets {
		if mh.OpenPositions > 0 {
			hasPositions = true
		}
		usable := mh.HasMark && !mh.PriceStale
		if mh.OpenPositions > 0 && !usable {
			h.Status = HealthDegraded
			if mh.Unpriced {
				h.Problems = append(h.Problems,
					"market "+mh.Symbol+": open positions but no price source configured")
			} else {
				h.Problems = append(h.Problems,
					"market "+mh.Symbol+": open positions cannot be risk-managed without a fresh price")
			}
			continue
		}
		// An idle market with a stale price is worth reporting but is not a risk:
		// there is nothing to liquidate.
		if mh.PriceStale && !mh.Unpriced {
			h.Problems = append(h.Problems, "market "+mh.Symbol+": price is stale")
		}
	}

	// Silence is itself a symptom: positions open and no risk event for a long
	// time suggests the scanner is not running.
	if hasPositions && h.LastEventAt > 0 && now.Unix()-h.LastEventAt > int64(noRiskEventsFor/time.Second) {
		h.Status = HealthStalled
		h.Problems = append(h.Problems, "no risk events for over 10 minutes while positions are open")
	}

	return h
}

// registerMonitor stores the monitor on the service so the health path can reach
// it without a package-level global.
func (s *PerpOrderBookService) registerMonitor(m *PerpMonitor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monitor = m
}

// Monitor returns the registered monitor, or nil when health reporting is off.
func (s *PerpOrderBookService) Monitor() *PerpMonitor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.monitor
}

// isDegraded reports whether the service has taken its fatal error.
func (s *PerpOrderBookService) isDegraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatalErr != nil
}

// insuranceFundBalances reports the fund's holding per asset it has ever been
// asked to pay in.
func (s *PerpOrderBookService) insuranceFundBalances() map[ledger.AssetID]*uint256.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[ledger.AssetID]*uint256.Int)
	for _, market := range s.markets {
		if _, ok := out[market.Quote]; ok {
			continue
		}
		out[market.Quote] = s.ledger.Balance(s.insuranceFund, market.Quote)
	}
	return out
}

// marketHealth builds the per-market section of the health report.
func (s *PerpOrderBookService) marketHealth(now time.Time) []MarketHealth {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]MarketHealth, 0, len(s.markets))
	for id, market := range s.markets {
		mh := MarketHealth{Market: id, Symbol: market.Symbol, FrozenMargin: uint256.NewInt(0)}
		if s.tracker != nil {
			if mark, ok := s.tracker.LastMark(id); ok {
				mh.MarkPrice = mark
				mh.HasMark = true
				if at, ok := s.tracker.lastMarkTime(id); ok {
					mh.MarkAgeSecs = int64(now.Sub(at) / time.Second)
				}
			}
			if idx, ok := s.tracker.IndexPrice(id); ok {
				mh.IndexPrice = idx
				mh.HasIndex = true
			}
			mh.PriceStale = s.tracker.Stale(id, now)
		} else {
			// No price source configured: the market is unpriced by construction.
			// This is only a health problem once something trades on it, which the
			// caller decides using HasMark and OpenPositions — a dormant market with
			// no marks and no positions is not broken.
			mh.PriceStale = true
			mh.Unpriced = true
		}
		for _, p := range s.engine.Positions(id) {
			mh.OpenPositions++
			if p.Margin != nil {
				mh.FrozenMargin.Add(mh.FrozenMargin, p.Margin)
			}
		}
		longs, shorts := s.engine.Book(id, 1_000_000)
		mh.RestingOrders = len(longs) + len(shorts)
		mh.LastFundingAt = s.funding[id].LastRoundAt
		out = append(out, mh)
	}
	return out
}

// MetricsText renders the health snapshot in Prometheus exposition format.
//
// Hand-rolled rather than pulling a client library: the surface is a dozen
// counters and gauges, and a dependency that owns the /metrics writer would be
// more moving parts than the problem needs.
func (h PerpHealth) MetricsText() string {
	var b []byte
	add := func(line string) { b = append(b, line...) }

	add("# HELP perp_health_status 1 when the service is ok, 0 otherwise\n")
	add("# TYPE perp_health_status gauge\n")
	okVal := 0
	if h.Status == HealthOK {
		okVal = 1
	}
	add(fmt.Sprintf("perp_health_status %d\n", okVal))

	add("# HELP perp_degraded 1 when the service has taken its fatal error\n")
	add("# TYPE perp_degraded gauge\n")
	deg := 0
	if h.Degraded {
		deg = 1
	}
	add(fmt.Sprintf("perp_degraded %d\n", deg))

	add("# HELP perp_liquidation_scans_total Liquidation scan iterations\n")
	add("# TYPE perp_liquidation_scans_total counter\n")
	add(fmt.Sprintf("perp_liquidation_scans_total %d\n", h.LiquidationScans))

	add("# HELP perp_liquidations_total Forced closes performed\n")
	add("# TYPE perp_liquidations_total counter\n")
	add(fmt.Sprintf("perp_liquidations_total %d\n", h.Liquidations))

	add("# HELP perp_liquidations_partial_total Forced closes that could not fill completely\n")
	add("# TYPE perp_liquidations_partial_total counter\n")
	add(fmt.Sprintf("perp_liquidations_partial_total %d\n", h.LiquidationsPartial))

	add("# HELP perp_adl_total Auto-deleveraging events\n")
	add("# TYPE perp_adl_total counter\n")
	add(fmt.Sprintf("perp_adl_total %d\n", h.ADLCount))

	add("# HELP perp_bad_debt_total Bad-debt events\n")
	add("# TYPE perp_bad_debt_total counter\n")
	add(fmt.Sprintf("perp_bad_debt_total %d\n", h.BadDebtEvents))

	add("# HELP perp_bad_debt_amount_total Cumulative uncovered loss\n")
	add("# TYPE perp_bad_debt_amount_total counter\n")
	amt := "0"
	if h.TotalBadDebt != nil {
		amt = h.TotalBadDebt.Dec()
	}
	add(fmt.Sprintf("perp_bad_debt_amount_total %s\n", amt))

	add("# HELP perp_funding_rounds_total Completed funding rounds\n")
	add("# TYPE perp_funding_rounds_total counter\n")
	add(fmt.Sprintf("perp_funding_rounds_total %d\n", h.FundingRounds))

	add("# HELP perp_open_positions Open positions per market\n")
	add("# TYPE perp_open_positions gauge\n")
	for _, mh := range h.Markets {
		add(fmt.Sprintf("perp_open_positions{market=%q} %d\n", mh.Symbol, mh.OpenPositions))
	}

	add("# HELP perp_price_stale 1 when a market's index price is stale\n")
	add("# TYPE perp_price_stale gauge\n")
	for _, mh := range h.Markets {
		stale := 0
		if mh.PriceStale && !mh.Unpriced {
			stale = 1
		}
		add(fmt.Sprintf("perp_price_stale{market=%q} %d\n", mh.Symbol, stale))
	}

	add("# HELP perp_unpriced 1 when no price source is configured for a market\n")
	add("# TYPE perp_unpriced gauge\n")
	for _, mh := range h.Markets {
		unpriced := 0
		if mh.Unpriced {
			unpriced = 1
		}
		add(fmt.Sprintf("perp_unpriced{market=%q} %d\n", mh.Symbol, unpriced))
	}

	add("# HELP perp_mark_price Current mark price per market\n")
	add("# TYPE perp_mark_price gauge\n")
	for _, mh := range h.Markets {
		add(fmt.Sprintf("perp_mark_price{market=%q} %d\n", mh.Symbol, mh.MarkPrice))
	}

	add("# HELP perp_frozen_margin Frozen margin per market, quote units\n")
	add("# TYPE perp_frozen_margin gauge\n")
	for _, mh := range h.Markets {
		v := "0"
		if mh.FrozenMargin != nil {
			v = mh.FrozenMargin.Dec()
		}
		add(fmt.Sprintf("perp_frozen_margin{market=%q} %s\n", mh.Symbol, v))
	}

	return string(b)
}

package services

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"

	"web3-amm-dex-backend/perp"
)

// chainPriceSourceABI is the minimal PriceOracle surface the source needs.
const chainPriceSourceABI = `[
  {"inputs":[{"internalType":"uint32","name":"market","type":"uint32"}],
   "name":"getPrice","outputs":[
     {"internalType":"uint64","name":"price","type":"uint64"},
     {"internalType":"uint64","name":"updatedAt","type":"uint64"}],
   "stateMutability":"view","type":"function"}
]`

// ChainPriceSource reads index prices from the on-chain PriceOracle contract.
//
// It is a perp.PriceSource: Index returns false unless the contract reports the
// price as FRESH. That is deliberate — a stale on-chain price must read as "no
// price" so the matcher skips liquidation, rather than liquidating everyone at a
// price the operator stopped updating hours ago.
//
// The source caches each market's decoded price so a tick that polls every market
// does not issue one RPC round-trip per call site; the cache is refreshed at most
// once per `refresh` interval.
type ChainPriceSource struct {
	adapter  BlockchainAdapter
	contract string
	maxAge   uint64
	refresh  time.Duration

	abi_ abi.ABI

	mu      sync.Mutex
	cache   map[perp.MarketID]cachedPrice
	lastErr error
}

type cachedPrice struct {
	price perp.Price
	at    time.Time
	fresh bool
}

// NewChainPriceSource builds a source reading `contract` through adapter.
// maxAgeSeconds is the on-chain staleness bound passed to getFreshPrice;
// refresh throttles RPC traffic (zero means one second).
func NewChainPriceSource(adapter BlockchainAdapter, contract string, maxAgeSeconds uint64, refresh time.Duration) (*ChainPriceSource, error) {
	if adapter == nil {
		return nil, errors.New("services: chain price source requires an adapter")
	}
	if contract == "" {
		return nil, errors.New("services: chain price source requires a contract address")
	}
	parsed, err := abi.JSON(strings.NewReader(chainPriceSourceABI))
	if err != nil {
		return nil, fmt.Errorf("services: chain price source ABI: %w", err)
	}
	if refresh <= 0 {
		refresh = time.Second
	}
	return &ChainPriceSource{
		adapter:  adapter,
		contract: contract,
		maxAge:   maxAgeSeconds,
		refresh:  refresh,
		abi_:     parsed,
		cache:    make(map[perp.MarketID]cachedPrice),
	}, nil
}

// Index implements perp.PriceSource.
func (s *ChainPriceSource) Index(m perp.MarketID) (perp.Price, bool) {
	s.mu.Lock()
	if c, ok := s.cache[m]; ok && time.Since(c.at) < s.refresh {
		s.mu.Unlock()
		return c.price, c.fresh && c.price != 0
	}
	s.mu.Unlock()

	price, fresh, err := s.fetch(m)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = err
		// Keep serving the last known value within the throttle window rather
		// than flapping to "no price" on a single flaky RPC call. The staleness
		// bound is still enforced by the tracker, which is the layer that decides
		// whether the mark is usable.
		if c, ok := s.cache[m]; ok && c.price != 0 && time.Since(c.at) < 3*s.refresh {
			return c.price, true
		}
		return 0, false
	}
	s.cache[m] = cachedPrice{price: price, at: time.Now(), fresh: fresh}
	s.lastErr = nil
	return price, fresh && price != 0
}

// LastError reports the most recent RPC failure, for health endpoints.
func (s *ChainPriceSource) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// fetch performs the eth_call and decodes (price, updatedAt). Freshness is
// judged here in Go rather than on-chain: keeping the contract call a plain
// getter means the staleness policy lives in one place (this function) and can be
// unit-tested without deploying anything.
func (s *ChainPriceSource) fetch(m perp.MarketID) (perp.Price, bool, error) {
	data, err := s.abi_.Pack("getPrice", uint32(m))
	if err != nil {
		return 0, false, fmt.Errorf("services: chain price source pack: %w", err)
	}
	out, err := s.adapter.CallContract(s.contract, data, 0)
	if err != nil {
		return 0, false, fmt.Errorf("services: chain price source call: %w", err)
	}
	if len(out) == 0 {
		return 0, false, errors.New("services: chain price source: empty return data")
	}
	vals, err := s.abi_.Unpack("getPrice", out)
	if err != nil {
		return 0, false, fmt.Errorf("services: chain price source unpack: %w", err)
	}
	if len(vals) != 2 {
		return 0, false, fmt.Errorf("services: chain price source: want 2 return values, got %d", len(vals))
	}
	price, ok := vals[0].(uint64)
	if !ok {
		return 0, false, fmt.Errorf("services: chain price source: price is %T, want uint64", vals[0])
	}
	updatedAt, ok := vals[1].(uint64)
	if !ok {
		return 0, false, fmt.Errorf("services: chain price source: updatedAt is %T, want uint64", vals[1])
	}
	if price == 0 {
		// Never published.
		return 0, false, nil
	}
	if s.maxAge > 0 {
		now := uint64(time.Now().Unix())
		if updatedAt == 0 || now > updatedAt+s.maxAge {
			// Stale: report "no price" so the matcher skips liquidation rather
			// than acting on a value the operator stopped maintaining.
			return perp.Price(price), false, nil
		}
	}
	return perp.Price(price), true, nil
}

// ChainPriceSourceFromEnv is a convenience for main: it returns nil when no
// oracle address is configured, which keeps marks and funding disabled.
func ChainPriceSourceFromEnv(adapter BlockchainAdapter, address string, maxAgeSeconds uint64) (perp.PriceSource, error) {
	if address == "" || adapter == nil {
		return nil, nil
	}
	return NewChainPriceSource(adapter, address, maxAgeSeconds, time.Second)
}

// compile-time check
var _ perp.PriceSource = (*ChainPriceSource)(nil)




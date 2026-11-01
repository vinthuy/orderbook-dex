package services

import (
	"errors"
	"fmt"
	"sync"

	"web3-amm-dex-backend/config"
	"web3-amm-dex-backend/ledger"
)

// SettlementStack is the assembled settlement subsystem: one blockchain
// adapter, one ledger recovered from disk, and the three settlement services
// registered in a single ServiceRegistry so they can be initialized, started,
// and stopped together.
//
// Build it with BuildSettlementStack; the zero value is not usable.
type SettlementStack struct {
	Adapter    BlockchainAdapter
	Store      *ledger.FileStore
	Ledger     *ledger.Ledger
	Holds      *ledger.Holds
	Settlement *SettlementService
	Indexer    *DepositIndexer
	Anchor     *StateAnchorService
	Bus        *EventBus
	// OrderBook is nil unless ORDERBOOK_MARKETS configured at least one market.
	OrderBook *OrderBookService
	Registry  *ServiceRegistry

	closeOnce sync.Once
	closeErr  error
}

// BuildSettlementStack assembles the settlement subsystem into a runnable
// stack.
//
// When adapter is nil a real EVMAdapter is built from cfg.RPCURL,
// cfg.OperatorKey, and cfg.ChainID; tests inject a mock instead, in which case
// those three fields are not required. The ledger is opened at cfg.DataDir and
// rebuilt through the ledger's own recovery path (ledger.Recover, which reads
// the snapshot plus the full WAL and restores the idempotency-key set) — that
// path is never reimplemented here.
func BuildSettlementStack(cfg Config, adapter BlockchainAdapter, onTrade ...EventHandler) (*SettlementStack, error) {
	if cfg.SettlementAddress == "" {
		return nil, errors.New("settlement stack: SettlementAddress is required")
	}
	if adapter == nil {
		if cfg.RPCURL == "" {
			return nil, errors.New("settlement stack: RPCURL is required to build the EVM adapter")
		}
		if cfg.OperatorKey == "" {
			return nil, errors.New("settlement stack: OperatorKey is required to build the EVM adapter")
		}
		evm, err := NewEVMAdapter(cfg.RPCURL, cfg.OperatorKey, cfg.ChainID)
		if err != nil {
			return nil, fmt.Errorf("settlement stack: build EVM adapter: %w", err)
		}
		adapter = evm
	}
	if cfg.DataDir == "" {
		return nil, errors.New("settlement stack: DataDir is required")
	}

	store, err := ledger.NewFileStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("settlement stack: open file store: %w", err)
	}
	l, err := ledger.Recover(store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("settlement stack: recover ledger: %w", err)
	}

	// Holds and the EventBus are shared across the stack: the settlement service
	// must withdraw only against available (balance - frozen) funds, and the
	// order book publishes into the same bus.
	holds := ledger.NewHolds(l)
	bus := NewEventBus()

	// Subscribe trade consumers BEFORE the order book is built: its constructor
	// replays the instruction WAL and re-emits settled trades, and a subscriber
	// that attaches later would miss them (EventBus drops events with no
	// subscribers).
	for _, h := range onTrade {
		if h != nil {
			bus.Subscribe(TopicOrderBookTrade, h)
		}
	}

	settlement, err := NewSettlementService(adapter, l, cfg.SettlementAddress)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("settlement stack: build settlement service: %w", err)
	}
	settlement.SetHoldProvider(holds)

	indexer := NewDepositIndexer(adapter, l, cfg.SettlementAddress, cfg.RequiredConfirmations)
	anchor := NewStateAnchorService(settlement, l)

	// The order book is assembled only when markets are configured, so an
	// unconfigured stack behaves exactly as before. A malformed market string
	// is fatal: an operator typo must not silently trade a subset.
	var orderBook *OrderBookService
	if cfg.OrderBookMarkets != "" {
		markets, err := ParseMarkets(cfg.OrderBookMarkets)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("settlement stack: parse ORDERBOOK_MARKETS: %w", err)
		}
		journal, err := NewJournal(cfg.OrderBookDataDir)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("settlement stack: open order book journal: %w", err)
		}
		orderBook, err = NewOrderBookService(markets, l, holds, journal, bus)
		if err != nil {
			_ = journal.Close()
			_ = store.Close()
			return nil, fmt.Errorf("settlement stack: build order book service: %w", err)
		}
	}

	registry := NewServiceRegistry()
	registry.Register(settlement)
	registry.Register(indexer)
	registry.Register(anchor)
	if orderBook != nil {
		registry.Register(orderBook)
	}

	return &SettlementStack{
		Adapter:    adapter,
		Store:      store,
		Ledger:     l,
		Holds:      holds,
		Settlement: settlement,
		Indexer:    indexer,
		Anchor:     anchor,
		Bus:        bus,
		OrderBook:  orderBook,
		Registry:   registry,
	}, nil
}

// Close stops every registered service and releases the ledger's WAL handle so
// the DataDir can be reopened. It is idempotent: teardown runs exactly once and
// a second Close returns the same result without touching the already-closed
// store.
func (st *SettlementStack) Close() error {
	st.closeOnce.Do(func() {
		var errs []error
		if st.Registry != nil {
			if err := st.Registry.StopAll(); err != nil {
				errs = append(errs, fmt.Errorf("settlement stack: stop services: %w", err))
			}
		}
		// Close the order-book instruction journal's file handle (Stop only
		// halts the runner). Idempotent, so it is safe after StopAll.
		if st.OrderBook != nil {
			if err := st.OrderBook.Close(); err != nil {
				errs = append(errs, fmt.Errorf("settlement stack: close order book: %w", err))
			}
		}
		if st.Store != nil {
			if err := st.Store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("settlement stack: close file store: %w", err))
			}
		}
		st.closeErr = errors.Join(errs...)
	})
	return st.closeErr
}

// ConfigFromSettlement bridges the application-level config.SettlementConfig
// into the services.Config the stack is built from.
//
// RPCURL is intentionally left unset because config.SettlementConfig carries no
// RPC endpoint; the caller (main) supplies it before BuildSettlementStack.
// Port is deliberately not bridged: services.Config.Port is an int while
// config.Config.Port is a string, and the settlement stack does not use it, so
// no string->int parse (and no silently swallowed parse error) happens here.
func ConfigFromSettlement(c config.SettlementConfig) Config {
	return Config{
		OperatorKey:           c.OperatorKey,
		SettlementAddress:     c.ContractAddress,
		DataDir:               c.DataDir,
		RequiredConfirmations: c.Confirmations,
		AnchorInterval:        c.AnchorIntervalSeconds,
		ChainID:               c.ChainID,
		OrderBookDataDir:      c.OrderBookDataDir,
		OrderBookMarkets:      c.OrderBookMarkets,
	}
}

package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/config"
	"web3-amm-dex-backend/gateway"
	"web3-amm-dex-backend/handlers"
	"web3-amm-dex-backend/indexer"
	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
	"web3-amm-dex-backend/services"
)

func main() {
	rpcURL := getEnv("ETH_RPC_URL", "http://localhost:8545")
	services.SetRPCURL(rpcURL)

	if err := services.InitClient(); err != nil {
		log.Fatalf("Failed to initialize client: %v", err)
	}

	go services.ListenEvents()

	sc := config.LoadSettlementConfig()
	ic := config.LoadIndexerConfig()

	// Build the indexer before the settlement stack so its trade handler can be
	// subscribed to the stack's EventBus BEFORE the order book is constructed:
	// the order book replays its instruction WAL in its constructor and re-emits
	// settled trades, which are lost if no subscriber is attached yet.
	var (
		istore *indexer.Store
		idxSvc *indexer.IndexerService
		err    error
	)
	if sc.ContractAddress != "" {
		istore, err = indexer.OpenStore(ic.DBPath)
		if err != nil {
			log.Fatalf("Failed to open indexer store: %v", err)
		}
		idxSvc = indexer.New(istore, nil, nil, sc.ContractAddress, sc.Confirmations)
		idxSvc.SetPollInterval(time.Duration(ic.PollInterval) * time.Second)
	}

	var onTrade services.EventHandler
	if idxSvc != nil {
		onTrade = idxSvc.HandleTrade
	}

	// Start the settlement stack alongside the AMM API. When SETTLEMENT_ADDRESS
	// is unset, startSettlementStack returns a nil stack and settlement stays
	// disabled while the AMM API keeps serving as before.
	settlement, err := startSettlementStack(os.Getenv, nil, onTrade)
	if err != nil {
		log.Fatalf("Failed to start settlement stack: %v", err)
	}

	r := gin.Default()

	// Mount the order-book gateway when the settlement stack assembled an
	// order book (ORDERBOOK_MARKETS configured at least one market). It shares
	// the same gin engine and EventBus as the order book so WS pushes work.
	var gw *gateway.Gateway
	if settlement != nil && settlement.OrderBook != nil {
		g, err := gateway.New(settlement.OrderBook, settlement.Bus, gatewayConfigFromEnv())
		if err != nil {
			log.Fatalf("Failed to build gateway: %v", err)
		}
		g.Register(r)
		gw = g
		log.Printf("order book gateway mounted at /v1 and /ws")
	}

	// The perpetual order book is mounted below (when PERP_MARKETS is set); its
	// handle is kept so shutdown can release the instruction journal.
	var perpsvc *services.PerpOrderBookService

	// Mount the perpetual order book when PERP_MARKETS is configured. It shares
	// the settlement stack's ledger, holds, and bus, so perp margin and PnL
	// contend for the same funds as spot and withdrawals.
	if settlement != nil && config.LoadPerpConfig().Markets != "" {
		pc := config.LoadPerpConfig()
		perpMarkets, err := services.ParsePerpMarkets(pc.Markets)
		if err != nil {
			log.Fatalf("Failed to parse PERP_MARKETS: %v", err)
		}
		// 6c risk parameters apply to every configured market. They must be
		// parsed before the engine is built, since the engine copies the market
		// configuration at construction.
		rc := config.LoadPerpRiskConfig()
		maintenanceRate, err := uint256.FromDecimal(rc.MaintenanceMarginRate)
		if err != nil {
			log.Fatalf("Failed to parse PERP_MAINTENANCE_MARGIN_RATE: %v", err)
		}
		liquidatorFee, err := uint256.FromDecimal(rc.LiquidatorFee)
		if err != nil {
			log.Fatalf("Failed to parse PERP_LIQUIDATOR_FEE: %v", err)
		}
		for i := range perpMarkets {
			perpMarkets[i].RiskConfig = perp.RiskConfig{
				MaintenanceMarginRateE18: maintenanceRate,
				LiquidatorFeeE18:         liquidatorFee,
			}
		}
		perpJournal, err := services.NewPerpJournal(pc.DataDir)
		if err != nil {
			log.Fatalf("Failed to open perp journal: %v", err)
		}
		perpsvc, err = services.NewPerpOrderBookService(perpMarkets, settlement.Ledger, settlement.Holds, perpJournal, settlement.Bus)
		if err != nil {
			_ = perpJournal.Close()
			log.Fatalf("Failed to build perp order book: %v", err)
		}
		settlement.Registry.Register(perpsvc)
		if err := perpsvc.Start(); err != nil {
			log.Fatalf("Failed to start perp order book: %v", err)
		}
		// Holds.Reconcile replaces the whole map, and the spot order book has
		// already reconciled its own reserves during stack assembly. Reconcile
		// the union once here, or mounting perp would unfreeze every spot
		// resting order on restart.
		var spotReserves map[ledger.HoldKey]*uint256.Int
		if settlement.OrderBook != nil {
			spotReserves = settlement.OrderBook.Engine().OpenReserves()
		}
		if err := settlement.Holds.Reconcile(services.UnionHolds(spotReserves, perpsvc.OpenMargins())); err != nil {
			log.Fatalf("Failed to reconcile spot+perp holds: %v", err)
		}

		// Subproject 6b: mark prices and funding. Without PERP_INDEX_PRICES the
		// service keeps pure 6a behaviour (PnL at fill price, no funding).
		fc := config.LoadPerpFundingConfig()
		if fc.IndexPrices != "" {
			prices, err := services.ParseIndexPrices(fc.IndexPrices)
			if err != nil {
				log.Fatalf("Failed to parse PERP_INDEX_PRICES: %v", err)
			}
			maxRate, err := uint256.FromDecimal(fc.MaxFundingRateE18)
			if err != nil {
				log.Fatalf("Failed to parse PERP_MAX_FUNDING_RATE: %v", err)
			}
			maxDev, err := uint256.FromDecimal(fc.MarkMaxDeviationE18)
			if err != nil {
				log.Fatalf("Failed to parse PERP_MARK_MAX_DEVIATION: %v", err)
			}
			perpsvc.SetPriceSource(prices, services.PriceOptions{
				MaxPriceAge:     fc.MaxPriceAge(),
				MaxMarkStaleAge: fc.MaxMarkStaleness(),
			})
			perpsvc.SetFundingOptions(services.FundingOptions{
				Tick:         fc.Tick(),
				Interval:     fc.FundingInterval(),
				MaxRateE18:   maxRate,
				MaxDeviation: maxDev,
			})
			log.Printf("perp mark price + funding enabled (interval %s)", fc.FundingInterval())
		}

		// Subproject 6c: forced liquidation. This runs regardless of whether a
		// price source is configured -- without one the scan is a no-op (it never
		// liquidates on an absent price), but the shortfall path is always live.
		if rc.InsuranceFundAccount != "" {
			fund, err := ledger.AccountFromHex(rc.InsuranceFundAccount)
			if err != nil {
				log.Fatalf("Failed to parse PERP_INSURANCE_FUND_ACCOUNT: %v", err)
			}
			perpsvc.SetInsuranceFund(fund)
		}
		perpsvc.SetLiquidationOptions(services.LiquidationOptions{
			Interval: rc.LiquidationInterval(),
		})
		log.Printf("perp liquidation enabled (interval %s)", rc.LiquidationInterval())

		log.Printf("perpetual order book mounted (%d markets)", len(perpMarkets))
	}

	// Mount the history indexer's read-only query routes and reconcile endpoint.
	if settlement != nil && idxSvc != nil {
		idxSvc.SetAdapter(settlement.Adapter)
		qs := &indexer.QueryServer{
			Store:         istore,
			Adapter:       settlement.Adapter,
			Chain:         sc.ContractAddress,
			Running:       settlement.Ledger,
			Confirmations: sc.Confirmations,
		}
		qs.Register(r)
		settlement.Registry.Register(idxSvc)
		if err := idxSvc.Initialize(services.Config{
			SettlementAddress:     sc.ContractAddress,
			RequiredConfirmations: sc.Confirmations,
		}); err != nil {
			log.Fatalf("Failed to initialize indexer: %v", err)
		}
		if err := idxSvc.Start(); err != nil {
			log.Fatalf("Failed to start indexer: %v", err)
		}
		log.Printf("indexer mounted at /v1/trades, /v1/fund-events, /v1/roots, /v1/reconcile")
	}

	r.GET("/health", handlers.HealthCheck)
	r.GET("/pools", handlers.GetAllPools)
	r.GET("/pools/:pool", handlers.GetPoolInfo)
	r.POST("/swap", handlers.Swap)
	r.POST("/liquidity", handlers.AddLiquidity)
	r.DELETE("/liquidity", handlers.RemoveLiquidity)
	r.GET("/events", handlers.GetEvents)

	port := getEnv("PORT", "8080")
	addr := fmt.Sprintf(":%s", port)

	fmt.Printf("AMM DEX Backend running on %s\n", addr)
	fmt.Printf("RPC URL: %s\n", rpcURL)

	go func() {
		if err := r.Run(addr); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("\nShutting down...")

	// Stop the gateway before the settlement stack so no request races a closed
	// nonce store; then stop the settlement stack. A Close error is logged only:
	// the process is already shutting down and must not log.Fatal here.
	if gw != nil {
		if err := gw.Close(); err != nil {
			log.Printf("gateway shutdown error: %v", err)
		}
	}
	if istore != nil {
		if err := istore.Close(); err != nil {
			log.Printf("indexer store shutdown error: %v", err)
		}
	}
	// Release the perp instruction journal's file handle. The settlement
	// stack's StopAll also stops the perp service runner, but only Close
	// releases the journal; Close is idempotent, so the later Stop is harmless.
	if perpsvc != nil {
		if err := perpsvc.Close(); err != nil {
			log.Printf("perp order book shutdown error: %v", err)
		}
	}
	if settlement != nil {
		if err := settlement.Close(); err != nil {
			log.Printf("settlement stack shutdown error: %v", err)
		}
	}
}

// startSettlementStack wires the settlement subsystem into the running process.
//
// It returns a nil stack when SETTLEMENT_ADDRESS is unset: settlement is then
// disabled (not an error) and only the AMM API is served. getenv is the
// environment-lookup injection point so tests can drive the orchestration
// without mutating the real environment; adapter injects a blockchain adapter
// (tests pass a mock, production passes nil to build the real EVMAdapter).
//
// Order matters: build -> InitializeAll -> StartAll, and every step releases
// the already-open resources when it fails so a rejected start cannot leak the
// ledger's WAL handle.
func startSettlementStack(getenv func(string) string, adapter services.BlockchainAdapter, onTrade ...services.EventHandler) (*services.SettlementStack, error) {
	sc := config.LoadSettlementConfig()
	if sc.ContractAddress == "" {
		log.Println("settlement stack disabled: SETTLEMENT_ADDRESS is not set; serving AMM API only")
		return nil, nil
	}

	cfg := services.ConfigFromSettlement(sc)

	// config.SettlementConfig carries no RPC endpoint, so supply it from the
	// existing app config (RPC_URL env with its default). Without this the
	// real EVMAdapter path fails with "RPCURL is required".
	rpcURL := config.LoadConfig().RPCURL
	if v := getenv("RPC_URL"); v != "" {
		rpcURL = v
	}
	cfg.RPCURL = rpcURL

	stack, err := services.BuildSettlementStack(cfg, adapter, onTrade...)
	if err != nil {
		return nil, fmt.Errorf("build settlement stack: %w", err)
	}

	if err := stack.Registry.InitializeAll(cfg); err != nil {
		_ = stack.Close()
		return nil, fmt.Errorf("initialize settlement stack: %w", err)
	}
	if err := stack.Registry.StartAll(); err != nil {
		_ = stack.Close()
		return nil, fmt.Errorf("start settlement stack: %w", err)
	}

	// Log the wiring, never OPERATOR_KEY (not its value, prefix, or length).
	log.Printf("settlement stack started: address=%s dataDir=%s confirmations=%d anchorInterval=%s",
		sc.ContractAddress, sc.DataDir, sc.Confirmations, sc.AnchorInterval())
	return stack, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// gatewayConfigFromEnv bridges config.GatewayConfig into gateway.Config.
func gatewayConfigFromEnv() gateway.Config {
	gc := config.LoadGatewayConfig()
	return gateway.Config{
		ChainID:       gc.ChainID,
		DomainName:    gc.DomainName,
		DomainVersion: gc.DomainVersion,
		Rate:          gc.Rate,
		Burst:         gc.Burst,
		NonceDir:      gc.NonceDir,
	}
}

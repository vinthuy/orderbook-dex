package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"web3-amm-dex-backend/config"
	"web3-amm-dex-backend/handlers"
	"web3-amm-dex-backend/services"
)

func main() {
	rpcURL := getEnv("ETH_RPC_URL", "http://localhost:8545")
	services.SetRPCURL(rpcURL)

	if err := services.InitClient(); err != nil {
		log.Fatalf("Failed to initialize client: %v", err)
	}

	go services.ListenEvents()

	// Start the settlement stack alongside the AMM API. When SETTLEMENT_ADDRESS
	// is unset, startSettlementStack returns a nil closer and settlement stays
	// disabled while the AMM API keeps serving as before.
	settlementCloser, err := startSettlementStack(os.Getenv, nil)
	if err != nil {
		log.Fatalf("Failed to start settlement stack: %v", err)
	}

	r := gin.Default()

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

	// Stop the settlement stack before exiting. A Close error is logged only:
	// the process is already shutting down and must not log.Fatal here.
	if settlementCloser != nil {
		if err := settlementCloser.Close(); err != nil {
			log.Printf("settlement stack shutdown error: %v", err)
		}
	}
}

// startSettlementStack wires the settlement subsystem into the running process.
//
// It returns a nil closer when SETTLEMENT_ADDRESS is unset: settlement is then
// disabled (not an error) and only the AMM API is served. getenv is the
// environment-lookup injection point so tests can drive the orchestration
// without mutating the real environment; adapter injects a blockchain adapter
// (tests pass a mock, production passes nil to build the real EVMAdapter).
//
// Order matters: build -> InitializeAll -> StartAll, and every step releases
// the already-open resources when it fails so a rejected start cannot leak the
// ledger's WAL handle.
func startSettlementStack(getenv func(string) string, adapter services.BlockchainAdapter) (io.Closer, error) {
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

	stack, err := services.BuildSettlementStack(cfg, adapter)
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

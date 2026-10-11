package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
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
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

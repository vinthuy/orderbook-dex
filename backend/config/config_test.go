package config

import (
    "os"
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestConfig_LoadDefaults(t *testing.T) {
    config := LoadDefaults()
    
    assert.Equal(t, "http://localhost:8545", config.RPCURL)
    assert.Equal(t, "8080", config.Port)
    assert.Equal(t, "localhost", config.Host)
    assert.Equal(t, "", config.DatabaseURL)
    assert.Equal(t, "", config.JWTSecret)
    assert.Equal(t, "info", config.LogLevel)
}

func TestConfig_OverrideDefaults(t *testing.T) {
    config := LoadDefaults()
    config.RPCURL = "http://custom-rpc:8545"
    config.Port = "3000"
    
    assert.Equal(t, "http://custom-rpc:8545", config.RPCURL)
    assert.Equal(t, "3000", config.Port)
}

func TestConfig_LoadConfig(t *testing.T) {
    // Set environment variables
    os.Setenv("RPC_URL", "http://env-rpc:8545")
    os.Setenv("PORT", "4000")
    os.Setenv("HOST", "env-host")
    os.Setenv("DATABASE_URL", "postgres://user:pass@localhost/db")
    os.Setenv("JWT_SECRET", "my-secret-key")
    os.Setenv("LOG_LEVEL", "debug")
    
    config := LoadConfig()
    
    assert.Equal(t, "http://env-rpc:8545", config.RPCURL)
    assert.Equal(t, "4000", config.Port)
    assert.Equal(t, "env-host", config.Host)
    assert.Equal(t, "postgres://user:pass@localhost/db", config.DatabaseURL)
    assert.Equal(t, "my-secret-key", config.JWTSecret)
    assert.Equal(t, "debug", config.LogLevel)
    
    // Clean up environment variables
    os.Unsetenv("RPC_URL")
    os.Unsetenv("PORT")
    os.Unsetenv("HOST")
    os.Unsetenv("DATABASE_URL")
    os.Unsetenv("JWT_SECRET")
    os.Unsetenv("LOG_LEVEL")
}

func TestConfig_LoadConfig_Defaults(t *testing.T) {
    // Ensure environment variables are not set
    os.Unsetenv("RPC_URL")
    os.Unsetenv("PORT")
    os.Unsetenv("HOST")
    os.Unsetenv("DATABASE_URL")
    os.Unsetenv("JWT_SECRET")
    os.Unsetenv("LOG_LEVEL")
    
    config := LoadConfig()
    
    assert.Equal(t, "http://localhost:8545", config.RPCURL)
    assert.Equal(t, "8080", config.Port)
    assert.Equal(t, "localhost", config.Host)
    assert.Equal(t, "", config.DatabaseURL)
    assert.Equal(t, "", config.JWTSecret)
    assert.Equal(t, "info", config.LogLevel)
}

func TestLoadSettlementConfigDefaults(t *testing.T) {
    t.Setenv("SETTLEMENT_ADDRESS", "")
    t.Setenv("DATA_DIR", "")
    cfg := LoadSettlementConfig()
    if cfg.ChainID != 13700 {
        t.Fatalf("ChainID = %d, want 13700", cfg.ChainID)
    }
    if cfg.Confirmations != 1 {
        t.Fatalf("Confirmations = %d, want 1", cfg.Confirmations)
    }
    if cfg.ExitDelay != 300 {
        t.Fatalf("ExitDelay = %d, want 300", cfg.ExitDelay)
    }
}

func TestLoadSettlementConfigFromEnv(t *testing.T) {
    t.Setenv("SETTLEMENT_ADDRESS", "0xabc")
    t.Setenv("OPERATOR_KEY", "0xdead")
    t.Setenv("REQUIRED_CONFIRMATIONS", "12")
    t.Setenv("ANCHOR_INTERVAL", "90")
    t.Setenv("CHAIN_ID", "31337")
    cfg := LoadSettlementConfig()
    if cfg.ContractAddress != "0xabc" || cfg.OperatorKey != "0xdead" {
        t.Fatalf("address/key not loaded: %+v", cfg)
    }
    if cfg.Confirmations != 12 || cfg.AnchorIntervalSeconds != 90 || cfg.ChainID != 31337 {
        t.Fatalf("numeric fields not loaded: %+v", cfg)
    }
}

func TestLoadSettlementConfigOrderBook(t *testing.T) {
    t.Setenv("ORDERBOOK_DATA_DIR", "./orders")
    t.Setenv("ORDERBOOK_MARKETS", "1:ETH/USDT:2:1:2")
    cfg := LoadSettlementConfig()
    if cfg.OrderBookDataDir != "./orders" {
        t.Fatalf("OrderBookDataDir = %q, want ./orders", cfg.OrderBookDataDir)
    }
    if cfg.OrderBookMarkets != "1:ETH/USDT:2:1:2" {
        t.Fatalf("OrderBookMarkets = %q, want the raw spec string", cfg.OrderBookMarkets)
    }
}

func TestLoadSettlementConfigOrderBookDefaultsEmpty(t *testing.T) {
    t.Setenv("ORDERBOOK_DATA_DIR", "")
    t.Setenv("ORDERBOOK_MARKETS", "")
    cfg := LoadSettlementConfig()
    if cfg.OrderBookDataDir != "" || cfg.OrderBookMarkets != "" {
        t.Fatalf("order book config should default empty: %+v", cfg)
    }
}

func TestLoadPerpConfig(t *testing.T) {
    t.Setenv("PERP_MARKETS", "")
    t.Setenv("PERP_DATA_DIR", "")
    if cfg := LoadPerpConfig(); cfg.Markets != "" || cfg.DataDir != "" {
        t.Fatalf("perp config should default empty: %+v", cfg)
    }

    t.Setenv("PERP_MARKETS", "1:ETH-PERP:2:1:18:10:1000000000000000")
    t.Setenv("PERP_DATA_DIR", "./perp")
    cfg := LoadPerpConfig()
    if cfg.Markets != "1:ETH-PERP:2:1:18:10:1000000000000000" {
        t.Fatalf("Markets = %q, want the raw PERP_MARKETS string", cfg.Markets)
    }
    if cfg.DataDir != "./perp" {
        t.Fatalf("DataDir = %q, want ./perp", cfg.DataDir)
    }
}

func TestLoadGatewayConfig(t *testing.T) {
    t.Setenv("EIP712_DOMAIN_NAME", "Test DEX")
    t.Setenv("EIP712_DOMAIN_VERSION", "2")
    t.Setenv("GATEWAY_RATE", "7")
    t.Setenv("GATEWAY_BURST", "13")
    t.Setenv("GATEWAY_NONCE_DIR", "/tmp/nonces")
    t.Setenv("CHAIN_ID", "999")

    cfg := LoadGatewayConfig()
    if cfg.DomainName != "Test DEX" || cfg.DomainVersion != "2" {
        t.Fatalf("domain = %q/%q", cfg.DomainName, cfg.DomainVersion)
    }
    if cfg.Rate != 7 || cfg.Burst != 13 {
        t.Fatalf("rate/burst = %v/%v", cfg.Rate, cfg.Burst)
    }
    if cfg.NonceDir != "/tmp/nonces" {
        t.Fatalf("nonceDir = %q", cfg.NonceDir)
    }
    if cfg.ChainID != 999 {
        t.Fatalf("chainID = %d", cfg.ChainID)
    }
}
package config

import (
    "os"
    "strconv"
    "time"
)

// Config holds the application configuration
type Config struct {
    RPCURL      string
    Port        string
    Host        string
    DatabaseURL string
    JWTSecret   string
    LogLevel    string
}

// SettlementConfig configures the settlement/ledger subsystem (subproject 1)
// and the off-chain order book that sits on top of it (subproject 3).
type SettlementConfig struct {
    ContractAddress       string
    OperatorKey           string
    ChainID               int64
    DataDir               string
    Confirmations         uint64
    AnchorIntervalSeconds int64
    ExitDelay             uint64

    // OrderBookDataDir is the order-book instruction WAL directory. Empty means
    // the order book is memory-only (no crash recovery); unset OrderBookMarkets
    // disables the order book entirely.
    OrderBookDataDir string
    // OrderBookMarkets is the compact market definition string, parsed by
    // services.ParseMarkets. A malformed string must fail startup.
    OrderBookMarkets string
}

// LoadSettlementConfig reads settlement settings from the environment.
func LoadSettlementConfig() SettlementConfig {
    cfg := SettlementConfig{
        ContractAddress:       os.Getenv("SETTLEMENT_ADDRESS"),
        OperatorKey:           os.Getenv("OPERATOR_KEY"),
        ChainID:               13700,
        DataDir:               defaultDataDir(),
        Confirmations:         1,
        AnchorIntervalSeconds: 30,
        ExitDelay:             300,
    }
    if v := os.Getenv("CHAIN_ID"); v != "" {
        if n, err := strconv.ParseInt(v, 10, 64); err == nil {
            cfg.ChainID = n
        }
    }
    if v := os.Getenv("DATA_DIR"); v != "" {
        cfg.DataDir = v
    }
    if v := os.Getenv("REQUIRED_CONFIRMATIONS"); v != "" {
        if n, err := strconv.ParseUint(v, 10, 64); err == nil {
            cfg.Confirmations = n
        }
    }
    if v := os.Getenv("ANCHOR_INTERVAL"); v != "" {
        if n, err := strconv.ParseInt(v, 10, 64); err == nil {
            cfg.AnchorIntervalSeconds = n
        }
    }
    if v := os.Getenv("EXIT_DELAY"); v != "" {
        if n, err := strconv.ParseUint(v, 10, 64); err == nil {
            cfg.ExitDelay = n
        }
    }
    if v := os.Getenv("ORDERBOOK_DATA_DIR"); v != "" {
        cfg.OrderBookDataDir = v
    }
    if v := os.Getenv("ORDERBOOK_MARKETS"); v != "" {
        cfg.OrderBookMarkets = v
    }
    return cfg
}

// AnchorInterval returns the anchor period as a Duration.
func (c SettlementConfig) AnchorInterval() time.Duration {
    return time.Duration(c.AnchorIntervalSeconds) * time.Second
}
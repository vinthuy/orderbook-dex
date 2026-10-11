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

// SettlementConfig configures the settlement/ledger subsystem (subproject 1).
type SettlementConfig struct {
    ContractAddress       string
    OperatorKey           string
    ChainID               int64
    DataDir               string
    Confirmations         uint64
    AnchorIntervalSeconds int64
    ExitDelay             uint64
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
    return cfg
}

// AnchorInterval returns the anchor period as a Duration.
func (c SettlementConfig) AnchorInterval() time.Duration {
    return time.Duration(c.AnchorIntervalSeconds) * time.Second
}
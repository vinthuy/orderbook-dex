package config

import "os"

// LoadDefaults returns a Config with default values
func LoadDefaults() *Config {
    return &Config{
        RPCURL:      "http://localhost:8545",
        Port:        "8080",
        Host:        "localhost",
        DatabaseURL: "",
        JWTSecret:   "",
        LogLevel:    "info",
    }
}

// LoadConfig loads configuration from environment variables with defaults as fallback
func LoadConfig() *Config {
    config := LoadDefaults()
    
    if rpcURL := os.Getenv("RPC_URL"); rpcURL != "" {
        config.RPCURL = rpcURL
    }
    if port := os.Getenv("PORT"); port != "" {
        config.Port = port
    }
    if host := os.Getenv("HOST"); host != "" {
        config.Host = host
    }
    if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
        config.DatabaseURL = dbURL
    }
    if jwtSecret := os.Getenv("JWT_SECRET"); jwtSecret != "" {
        config.JWTSecret = jwtSecret
    }
    if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
        config.LogLevel = logLevel
    }
    
    return config
}

// defaultDataDir returns the default on-disk location for the ledger WAL/snapshots.
func defaultDataDir() string {
    if v := os.Getenv("DATA_DIR"); v != "" {
        return v
    }
    return "./data/settlement"
}
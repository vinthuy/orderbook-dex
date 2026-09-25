package config

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
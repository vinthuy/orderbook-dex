package config

// Config holds the application configuration
type Config struct {
    RPCURL      string
    Port        string
    Host        string
    DatabaseURL string
    JWTSecret   string
    LogLevel    string
}
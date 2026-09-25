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
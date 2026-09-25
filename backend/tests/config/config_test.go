package config

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestConfig_LoadDefaults(t *testing.T) {
    config := LoadDefaults()
    
    assert.Equal(t, "http://localhost:8545", config.RPCURL)
    assert.Equal(t, "8080", config.Port)
    assert.Equal(t, "localhost", config.Host)
}

func TestConfig_OverrideDefaults(t *testing.T) {
    config := LoadDefaults()
    config.RPCURL = "http://custom-rpc:8545"
    config.Port = "3000"
    
    assert.Equal(t, "http://custom-rpc:8545", config.RPCURL)
    assert.Equal(t, "3000", config.Port)
}
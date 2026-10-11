package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecoveryMiddlewareExists(t *testing.T) {
	// Test that RecoveryMiddleware function exists and returns a gin.HandlerFunc
	handler := RecoveryMiddleware()
	assert.NotNil(t, handler, "RecoveryMiddleware should return a non-nil handler")
}

func TestLoggerMiddlewareExists(t *testing.T) {
	// Test that LoggerMiddleware function exists and returns a gin.HandlerFunc
	handler := LoggerMiddleware()
	assert.NotNil(t, handler, "LoggerMiddleware should return a non-nil handler")
}

func TestRateLimitMiddlewareExists(t *testing.T) {
	// Test that RateLimitMiddleware function exists and returns a gin.HandlerFunc
	handler := RateLimitMiddleware(100)
	assert.NotNil(t, handler, "RateLimitMiddleware should return a non-nil handler")
}

func TestGatewayExists(t *testing.T) {
	// Test that NewGateway function exists and returns a Gateway
	gateway := NewGateway()
	assert.NotNil(t, gateway, "NewGateway should return a non-nil gateway")
	assert.NotNil(t, gateway.GetRouter(), "Gateway should have a non-nil router")
}
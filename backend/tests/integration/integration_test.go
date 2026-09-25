package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"web3-amm-dex-backend/config"
	"web3-amm-dex-backend/services"
)

func TestServiceRegistryIntegration(t *testing.T) {
	// Create registry
	registry := services.NewServiceRegistry()

	// Create and register services
	eventBus := services.NewEventBus()
	registry.Register(eventBus)

	// Initialize
	cfg := config.LoadDefaults()
	err := registry.InitializeAll(services.Config{
		RPCURL: cfg.RPCURL,
	})

	assert.NoError(t, err)

	// Verify services are registered
	_, exists := registry.Get("EventBus")
	assert.True(t, exists)
}

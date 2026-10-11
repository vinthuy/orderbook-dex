package services

import "fmt"

// Service defines the interface for all services in the registry
type Service interface {
	Name() string
	Initialize(config Config) error
	Start() error
	Stop() error
}

// Config holds configuration for services
type Config struct {
	RPCURL      string
	OperatorKey string // settlement operator private key; required only when building the real EVM adapter
	Port        int
	DatabaseURL string

	SettlementAddress     string
	DataDir               string
	RequiredConfirmations uint64
	AnchorInterval        int64 // seconds
	ChainID               int64

	// ReceiptTimeoutSeconds bounds how long CommitRoot waits for a mined
	// receipt before failing; ReceiptPollMillis is the interval between
	// GetTransactionReceipt polls. Zero leaves the service defaults
	// (60s / 1000ms) in place.
	ReceiptTimeoutSeconds int64
	ReceiptPollMillis     int64
}

// ServiceRegistry manages the lifecycle of services
type ServiceRegistry struct {
	services map[string]Service
}

// NewServiceRegistry creates a new instance of ServiceRegistry
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[string]Service),
	}
}

// Register adds a service to the registry
func (r *ServiceRegistry) Register(service Service) {
	r.services[service.Name()] = service
}

// Get retrieves a service by name
func (r *ServiceRegistry) Get(name string) (Service, bool) {
	service, exists := r.services[name]
	return service, exists
}

// InitializeAll initializes all registered services with the given configuration
func (r *ServiceRegistry) InitializeAll(config Config) error {
	for name, service := range r.services {
		if err := service.Initialize(config); err != nil {
			return fmt.Errorf("failed to initialize service %s: %w", name, err)
		}
	}
	return nil
}

// StartAll starts all registered services
func (r *ServiceRegistry) StartAll() error {
	for name, service := range r.services {
		if err := service.Start(); err != nil {
			return fmt.Errorf("failed to start service %s: %w", name, err)
		}
	}
	return nil
}

// StopAll stops all registered services
func (r *ServiceRegistry) StopAll() error {
	for name, service := range r.services {
		if err := service.Stop(); err != nil {
			return fmt.Errorf("failed to stop service %s: %w", name, err)
		}
	}
	return nil
}

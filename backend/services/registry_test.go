package services

import (
	"errors"
	"strings"
	"testing"
)

// MockService is a test implementation of the Service interface
type MockService struct {
	name        string
	initialized bool
	started     bool
	stopped     bool
	initErr     error
	startErr    error
	stopErr     error
}

func (m *MockService) Name() string {
	return m.name
}

func (m *MockService) Initialize(config Config) error {
	if m.initErr != nil {
		return m.initErr
	}
	m.initialized = true
	return nil
}

func (m *MockService) Start() error {
	if m.startErr != nil {
		return m.startErr
	}
	m.started = true
	return nil
}

func (m *MockService) Stop() error {
	if m.stopErr != nil {
		return m.stopErr
	}
	m.stopped = true
	return nil
}

func TestServiceRegistry_RegisterAndGet(t *testing.T) {
	registry := NewServiceRegistry()

	// Create a mock service
	mockService := &MockService{
		name:        "test-service",
		initialized: false,
	}

	// Register service
	registry.Register(mockService)

	// Get service
	service, exists := registry.Get("test-service")

	if !exists {
		t.Error("Expected service to exist")
	}

	if service.Name() != "test-service" {
		t.Errorf("Expected service name 'test-service', got '%s'", service.Name())
	}
}

func TestServiceRegistry_InitializeAll(t *testing.T) {
	registry := NewServiceRegistry()

	// Create mock services
	service1 := &MockService{name: "service1"}
	service2 := &MockService{name: "service2"}

	// Register services
	registry.Register(service1)
	registry.Register(service2)

	// Initialize all
	config := Config{RPCURL: "http://localhost:8545"}
	err := registry.InitializeAll(config)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if !service1.initialized {
		t.Error("Expected service1 to be initialized")
	}

	if !service2.initialized {
		t.Error("Expected service2 to be initialized")
	}
}

func TestServiceRegistry_GetNotFound(t *testing.T) {
	registry := NewServiceRegistry()

	service, exists := registry.Get("nonexistent")

	if exists {
		t.Error("Expected service to not exist")
	}

	if service != nil {
		t.Error("Expected nil service")
	}
}

func TestServiceRegistry_StopAll(t *testing.T) {
	registry := NewServiceRegistry()

	// Create mock services
	service1 := &MockService{name: "service1"}
	service2 := &MockService{name: "service2"}

	// Register services
	registry.Register(service1)
	registry.Register(service2)

	// Initialize and start services first
	config := Config{RPCURL: "http://localhost:8545"}
	registry.InitializeAll(config)
	registry.StartAll()

	// Stop all
	err := registry.StopAll()

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if !service1.stopped {
		t.Error("Expected service1 to be stopped")
	}

	if !service2.stopped {
		t.Error("Expected service2 to be stopped")
	}
}

func TestServiceRegistry_InitializeAllWithError(t *testing.T) {
	registry := NewServiceRegistry()

	// Create mock service with initialization error
	service1 := &MockService{name: "service1"}
	service2 := &MockService{name: "service2", initErr: errors.New("init failed")}
	service3 := &MockService{name: "service3"}

	// Register services
	registry.Register(service1)
	registry.Register(service2)
	registry.Register(service3)

	// Initialize all
	config := Config{RPCURL: "http://localhost:8545"}
	err := registry.InitializeAll(config)

	if err == nil {
		t.Error("Expected error, got nil")
	}

	// The error must name the failing service. InitializeAll ranges over an
	// unordered map, so we cannot assert on visit order -- only that the
	// failure is attributed to service2.
	if err != nil && !strings.Contains(err.Error(), "service2") {
		t.Errorf("Expected error to name service2, got %v", err)
	}

	// The service that failed must not be marked initialized.
	if service2.initialized {
		t.Error("Expected service2 to not be initialized")
	}
}

func TestServiceRegistry_StartAll(t *testing.T) {
	registry := NewServiceRegistry()

	// Create mock services
	service1 := &MockService{name: "service1"}
	service2 := &MockService{name: "service2"}

	// Register services
	registry.Register(service1)
	registry.Register(service2)

	// Initialize services
	config := Config{RPCURL: "http://localhost:8545"}
	registry.InitializeAll(config)

	// Start all
	err := registry.StartAll()

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if !service1.started {
		t.Error("Expected service1 to be started")
	}

	if !service2.started {
		t.Error("Expected service2 to be started")
	}
}

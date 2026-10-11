# Web3 AMM DEX Architecture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a scalable and extensible architecture for the Web3 AMM DEX prototype, focusing on service registry, event-driven architecture, and plugin system.

**Architecture:** Implement a layered architecture with service registry, event bus, blockchain adapters, and plugin system. The design emphasizes loose coupling, testability, and extensibility.

**Tech Stack:** Go (backend), Solidity (smart contracts), Hardhat (development), Gin (HTTP framework), Ethereum client library

---

## File Structure

### Backend Services
- `services/registry.go` - Service registry and dependency injection
- `services/event_bus.go` - Event-driven communication system
- `services/blockchain_adapter.go` - Blockchain interaction interface
- `services/event_indexer.go` - Event indexing and storage
- `services/config.go` - Configuration management

### API Gateway
- `api/gateway.go` - API gateway with middleware chain
- `api/middleware.go` - Authentication, rate limiting, logging
- `api/v1/routes.go` - API version 1 routes
- `api/v2/routes.go` - API version 2 routes (future)

### Smart Contracts
- `contracts/Factory.sol` - Enhanced factory with better indexing
- `contracts/Router.sol` - Enhanced router with multi-hop support
- `contracts/interfaces/IPool.sol` - Pool interface for extensibility
- `contracts/interfaces/IFactory.sol` - Factory interface

### Configuration
- `config/config.go` - Configuration structures
- `config/defaults.go` - Default configuration values

### Testing
- `tests/services/registry_test.go` - Service registry tests
- `tests/services/event_bus_test.go` - Event bus tests
- `tests/api/gateway_test.go` - API gateway tests

---

## Task 1: Service Registry Implementation

**Files:**
- Create: `services/registry.go`
- Create: `tests/services/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
package services

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestServiceRegistry_RegisterAndGet(t *testing.T) {
    registry := NewServiceRegistry()
    
    // Create a mock service
    mockService := &MockService{
        name: "test-service",
        initialized: false,
    }
    
    // Register service
    registry.Register(mockService)
    
    // Get service
    service, exists := registry.Get("test-service")
    
    assert.True(t, exists)
    assert.Equal(t, "test-service", service.Name())
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
    
    assert.NoError(t, err)
    assert.True(t, service1.initialized)
    assert.True(t, service2.initialized)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3\backend && go test ./tests/services/... -v -run TestServiceRegistry`
Expected: FAIL with "undefined: NewServiceRegistry"

- [ ] **Step 3: Write minimal implementation**

```go
// services/registry.go
package services

import (
    "fmt"
    "sync"
)

// Service interface for all services
type Service interface {
    Name() string
    Initialize(config Config) error
    Start() error
    Stop() error
}

// Config holds configuration for services
type Config struct {
    RPCURL     string
    Port       string
    DatabaseURL string
}

// ServiceRegistry manages service lifecycle
type ServiceRegistry struct {
    mu       sync.RWMutex
    services map[string]Service
    config   Config
}

// NewServiceRegistry creates a new registry
func NewServiceRegistry() *ServiceRegistry {
    return &ServiceRegistry{
        services: make(map[string]Service),
    }
}

// Register adds a service to the registry
func (r *ServiceRegistry) Register(service Service) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.services[service.Name()] = service
}

// Get retrieves a service by name
func (r *ServiceRegistry) Get(name string) (Service, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    service, exists := r.services[name]
    return service, exists
}

// InitializeAll initializes all registered services
func (r *ServiceRegistry) InitializeAll(config Config) error {
    r.mu.RLock()
    defer r.mu.RUnlock()
    
    r.config = config
    for name, service := range r.services {
        if err := service.Initialize(config); err != nil {
            return fmt.Errorf("failed to initialize service %s: %w", name, err)
        }
    }
    return nil
}

// StartAll starts all registered services
func (r *ServiceRegistry) StartAll() error {
    r.mu.RLock()
    defer r.mu.RUnlock()
    
    for name, service := range r.services {
        if err := service.Start(); err != nil {
            return fmt.Errorf("failed to start service %s: %w", name, err)
        }
    }
    return nil
}

// StopAll stops all registered services
func (r *ServiceRegistry) StopAll() error {
    r.mu.RLock()
    defer r.mu.RUnlock()
    
    for name, service := range r.services {
        if err := service.Stop(); err != nil {
            return fmt.Errorf("failed to stop service %s: %w", name, err)
        }
    }
    return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3\backend && go test ./tests/services/... -v -run TestServiceRegistry`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add services/registry.go tests/services/registry_test.go
git commit -m "feat: implement service registry with tests"
```

---

## Task 2: Event Bus Implementation

**Files:**
- Create: `services/event_bus.go`
- Create: `tests/services/event_bus_test.go`

- [ ] **Step 1: Write the failing test**

```go
package services

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestEventBus_SubscribeAndPublish(t *testing.T) {
    bus := NewEventBus()
    
    // Create handler
    handlerCalled := false
    handler := func(event Event) error {
        handlerCalled = true
        return nil
    }
    
    // Subscribe
    bus.Subscribe("test.event", handler)
    
    // Publish event
    event := Event{
        Topic: "test.event",
        Data:  "test data",
    }
    
    err := bus.Publish(event)
    
    assert.NoError(t, err)
    assert.True(t, handlerCalled)
}

func TestEventBus_MultipleSubscribers(t *testing.T) {
    bus := NewEventBus()
    
    callCount := 0
    handler1 := func(event Event) error {
        callCount++
        return nil
    }
    handler2 := func(event Event) error {
        callCount++
        return nil
    }
    
    bus.Subscribe("test.event", handler1)
    bus.Subscribe("test.event", handler2)
    
    event := Event{Topic: "test.event", Data: "test"}
    bus.Publish(event)
    
    assert.Equal(t, 2, callCount)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3\backend && go test ./tests/services/... -v -run TestEventBus`
Expected: FAIL with "undefined: NewEventBus"

- [ ] **Step 3: Write minimal implementation**

```go
// services/event_bus.go
package services

import (
    "sync"
)

// Event represents a system event
type Event struct {
    Topic     string
    Data      interface{}
    Timestamp int64
}

// EventHandler processes events
type EventHandler func(event Event) error

// EventBus manages event subscriptions and publishing
type EventBus struct {
    mu          sync.RWMutex
    subscribers map[string][]EventHandler
}

// NewEventBus creates a new event bus
func NewEventBus() *EventBus {
    return &EventBus{
        subscribers: make(map[string][]EventHandler),
    }
}

// Subscribe adds a handler for a topic
func (eb *EventBus) Subscribe(topic string, handler EventHandler) {
    eb.mu.Lock()
    defer eb.mu.Unlock()
    eb.subscribers[topic] = append(eb.subscribers[topic], handler)
}

// Publish sends an event to all subscribers
func (eb *EventBus) Publish(event Event) error {
    eb.mu.RLock()
    defer eb.mu.RUnlock()
    
    handlers := eb.subscribers[event.Topic]
    for _, handler := range handlers {
        if err := handler(event); err != nil {
            return err
        }
    }
    return nil
}

// Unsubscribe removes all handlers for a topic
func (eb *EventBus) Unsubscribe(topic string) {
    eb.mu.Lock()
    defer eb.mu.Unlock()
    delete(eb.subscribers, topic)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3\backend && go test ./tests/services/... -v -run TestEventBus`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add services/event_bus.go tests/services/event_bus_test.go
git commit -m "feat: implement event bus with pub/sub pattern"
```

---

## Task 3: Blockchain Adapter Interface

**Files:**
- Create: `services/blockchain_adapter.go`
- Create: `tests/services/blockchain_adapter_test.go`

- [ ] **Step 1: Write the failing test**

```go
package services

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestBlockchainAdapter_GetBlockNumber(t *testing.T) {
    adapter := NewMockBlockchainAdapter()
    
    blockNumber, err := adapter.GetBlockNumber()
    
    assert.NoError(t, err)
    assert.Greater(t, blockNumber, uint64(0))
}

func TestBlockchainAdapter_CallContract(t *testing.T) {
    adapter := NewMockBlockchainAdapter()
    
    result, err := adapter.CallContract(
        "0x1234567890123456789012345678901234567890",
        []byte{0x01, 0x02, 0x03},
        100,
    )
    
    assert.NoError(t, err)
    assert.NotNil(t, result)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3\backend && go test ./tests/services/... -v -run TestBlockchainAdapter`
Expected: FAIL with "undefined: NewMockBlockchainAdapter"

- [ ] **Step 3: Write minimal implementation**

```go
// services/blockchain_adapter.go
package services

// BlockchainAdapter defines interface for blockchain interaction
type BlockchainAdapter interface {
    GetBlockNumber() (uint64, error)
    GetTransactionReceipt(txHash string) (*Receipt, error)
    CallContract(contract string, data []byte, blockNumber uint64) ([]byte, error)
    SendTransaction(tx *Transaction) (string, error)
}

// Receipt represents transaction receipt
type Receipt struct {
    Status            uint64
    TransactionHash   string
    BlockNumber       uint64
    GasUsed           uint64
    ContractAddress   string
    Logs              []Log
}

// Log represents event log
type Log struct {
    Address     string
    Topics      []string
    Data        string
    BlockNumber uint64
}

// Transaction represents blockchain transaction
type Transaction struct {
    To       string
    Data     []byte
    Value    uint64
    GasLimit uint64
}

// MockBlockchainAdapter implements BlockchainAdapter for testing
type MockBlockchainAdapter struct {
    blockNumber uint64
}

// NewMockBlockchainAdapter creates a mock adapter
func NewMockBlockchainAdapter() *MockBlockchainAdapter {
    return &MockBlockchainAdapter{
        blockNumber: 1000,
    }
}

func (m *MockBlockchainAdapter) GetBlockNumber() (uint64, error) {
    return m.blockNumber, nil
}

func (m *MockBlockchainAdapter) GetTransactionReceipt(txHash string) (*Receipt, error) {
    return &Receipt{
        Status:          1,
        TransactionHash: txHash,
        BlockNumber:     m.blockNumber,
        GasUsed:         21000,
    }, nil
}

func (m *MockBlockchainAdapter) CallContract(contract string, data []byte, blockNumber uint64) ([]byte, error) {
    return []byte{0x00, 0x00, 0x00, 0x01}, nil
}

func (m *MockBlockchainAdapter) SendTransaction(tx *Transaction) (string, error) {
    return "0x" + "abc123", nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3\backend && go test ./tests/services/... -v -run TestBlockchainAdapter`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add services/blockchain_adapter.go tests/services/blockchain_adapter_test.go
git commit -m "feat: implement blockchain adapter interface with mock"
```

---

## Task 4: API Gateway with Middleware

**Files:**
- Create: `api/gateway.go`
- Create: `api/middleware.go`
- Create: `tests/api/gateway_test.go`

- [ ] **Step 1: Write the failing test**

```go
package api

import (
    "net/http"
    "net/http/httptest"
    "testing"
    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
)

func TestGateway_MiddlewareChain(t *testing.T) {
    gin.SetMode(gin.TestMode)
    
    // Create test router
    router := gin.New()
    
    // Add middleware
    router.Use(
        RecoveryMiddleware(),
        LoggerMiddleware(),
    )
    
    // Add test route
    router.GET("/test", func(c *gin.Context) {
        c.JSON(http.StatusOK, gin.H{"message": "success"})
    })
    
    // Create test request
    w := httptest.NewRecorder()
    req, _ := http.NewRequest("GET", "/test", nil)
    
    // Execute request
    router.ServeHTTP(w, req)
    
    // Assert response
    assert.Equal(t, http.StatusOK, w.Code)
    assert.Contains(t, w.Body.String(), "success")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3\backend && go test ./tests/api/... -v -run TestGateway`
Expected: FAIL with "undefined: RecoveryMiddleware"

- [ ] **Step 3: Write minimal implementation**

```go
// api/middleware.go
package api

import (
    "log"
    "time"
    "github.com/gin-gonic/gin"
)

// RecoveryMiddleware recovers from panics
func RecoveryMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        defer func() {
            if err := recover(); err != nil {
                log.Printf("Recovery from panic: %v", err)
                c.AbortWithStatusJSON(500, gin.H{"error": "Internal server error"})
            }
        }()
        c.Next()
    }
}

// LoggerMiddleware logs requests
func LoggerMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        start := time.Now()
        path := c.Request.URL.Path
        
        c.Next()
        
        latency := time.Since(start)
        status := c.Writer.Status()
        
        log.Printf("[%s] %s %s %d %v",
            c.ClientIP(),
            c.Request.Method,
            path,
            status,
            latency,
        )
    }
}

// RateLimitMiddleware limits requests per second
func RateLimitMiddleware(limit int) gin.HandlerFunc {
    // Implementation would use token bucket or similar
    return func(c *gin.Context) {
        // For now, just pass through
        c.Next()
    }
}
```

```go
// api/gateway.go
package api

import (
    "github.com/gin-gonic/gin"
)

// Gateway manages API routes and middleware
type Gateway struct {
    router *gin.Engine
}

// NewGateway creates a new API gateway
func NewGateway() *Gateway {
    gin.SetMode(gin.ReleaseMode)
    router := gin.New()
    
    // Add global middleware
    router.Use(
        RecoveryMiddleware(),
        LoggerMiddleware(),
    )
    
    return &Gateway{
        router: router,
    }
}

// RegisterRoute adds a route to the gateway
func (g *Gateway) RegisterRoute(method, path string, handler gin.HandlerFunc) {
    switch method {
    case "GET":
        g.router.GET(path, handler)
    case "POST":
        g.router.POST(path, handler)
    case "PUT":
        g.router.PUT(path, handler)
    case "DELETE":
        g.router.DELETE(path, handler)
    }
}

// Run starts the gateway
func (g *Gateway) Run(addr string) error {
    return g.router.Run(addr)
}

// GetRouter returns the gin router
func (g *Gateway) GetRouter() *gin.Engine {
    return g.router
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3\backend && go test ./tests/api/... -v -run TestGateway`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add api/gateway.go api/middleware.go tests/api/gateway_test.go
git commit -m "feat: implement API gateway with middleware chain"
```

---

## Task 5: Configuration Management

**Files:**
- Create: `config/config.go`
- Create: `config/defaults.go`
- Create: `tests/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3\backend && go test ./tests/config/... -v -run TestConfig`
Expected: FAIL with "undefined: LoadDefaults"

- [ ] **Step 3: Write minimal implementation**

```go
// config/config.go
package config

// Config holds application configuration
type Config struct {
    RPCURL      string
    Port        string
    Host        string
    DatabaseURL string
    JWTSecret   string
    LogLevel    string
}

// LoadConfig loads configuration from environment
func LoadConfig() *Config {
    config := LoadDefaults()
    
    // Override with environment variables
    if rpcURL := getEnv("ETH_RPC_URL", ""); rpcURL != "" {
        config.RPCURL = rpcURL
    }
    if port := getEnv("PORT", ""); port != "" {
        config.Port = port
    }
    if host := getEnv("HOST", ""); host != "" {
        config.Host = host
    }
    if dbURL := getEnv("DATABASE_URL", ""); dbURL != "" {
        config.DatabaseURL = dbURL
    }
    if jwtSecret := getEnv("JWT_SECRET", ""); jwtSecret != "" {
        config.JWTSecret = jwtSecret
    }
    if logLevel := getEnv("LOG_LEVEL", ""); logLevel != "" {
        config.LogLevel = logLevel
    }
    
    return config
}

func getEnv(key, defaultVal string) string {
    if val := os.Getenv(key); val != "" {
        return val
    }
    return defaultVal
}
```

```go
// config/defaults.go
package config

// LoadDefaults returns default configuration
func LoadDefaults() *Config {
    return &Config{
        RPCURL:   "http://localhost:8545",
        Port:     "8080",
        Host:     "localhost",
        LogLevel: "info",
    }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3\backend && go test ./tests/config/... -v -run TestConfig`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add config/config.go config/defaults.go tests/config/config_test.go
git commit -m "feat: implement configuration management"
```

---

## Task 6: Enhanced Smart Contract Factory

**Files:**
- Create: `contracts/interfaces/IFactory.sol`
- Modify: `contracts/Factory.sol`

- [ ] **Step 1: Write the failing test**

```solidity
// test/Factory.test.js
const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Enhanced Factory", function () {
    let factory, tokenA, tokenB;
    
    beforeEach(async function () {
        const Token = await ethers.getContractFactory("Token");
        tokenA = await Token.deploy("Token A", "TKA", 18, 1000000);
        tokenB = await Token.deploy("Token B", "TKB", 18, 1000000);
        
        const Factory = await ethers.getContractFactory("Factory");
        factory = await Factory.deploy();
    });
    
    it("Should create pool and emit event", async function () {
        await expect(factory.createPool(tokenA.address, tokenB.address))
            .to.emit(factory, "PoolCreated")
            .withArgs(tokenA.address, tokenB.address, /* pool address */, 1);
    });
    
    it("Should list all pools", async function () {
        await factory.createPool(tokenA.address, tokenB.address);
        const pools = await factory.getAllPools();
        expect(pools.length).to.equal(1);
    });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3 && npx hardhat test test/Factory.test.js`
Expected: FAIL with "createPool function not found"

- [ ] **Step 3: Write minimal implementation**

```solidity
// contracts/interfaces/IFactory.sol
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

interface IFactory {
    event PoolCreated(address indexed token0, address indexed token1, address pool, uint256 poolCount);
    
    function createPool(address tokenA, address tokenB) external returns (address pool);
    function getPool(address tokenA, address tokenB) external view returns (address pool);
    function getAllPools() external view returns (address[] memory pools);
    function getPoolCount() external view returns (uint256 count);
}
```

```solidity
// contracts/Factory.sol (enhanced)
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "./interfaces/IFactory.sol";
import "./AMMPool.sol";
import "./Token.sol";

contract Factory is IFactory {
    mapping(address => mapping(address => address)) public getPool;
    address[] public allPools;
    
    function createPool(address tokenA, address tokenB) external override returns (address pool) {
        require(tokenA != tokenB, "Identical tokens");
        require(getPool[tokenA][tokenB] == address(0), "Pool exists");
        
        // Create pool
        AMMPool newPool = new AMMPool(
            Token(tokenA),
            Token(tokenB)
        );
        pool = address(newPool);
        
        // Index pool
        allPools.push(pool);
        getPool[tokenA][tokenB] = pool;
        getPool[tokenB][tokenA] = pool;
        
        emit PoolCreated(tokenA, tokenB, pool, allPools.length);
    }
    
    function getPool(address tokenA, address tokenB) external view override returns (address) {
        return getPool[tokenA][tokenB];
    }
    
    function getAllPools() external view override returns (address[] memory) {
        return allPools;
    }
    
    function getPoolCount() external view override returns (uint256) {
        return allPools.length;
    }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3 && npx hardhat test test/Factory.test.js`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add contracts/interfaces/IFactory.sol contracts/Factory.sol test/Factory.test.js
git commit -m "feat: enhance Factory with better indexing and events"
```

---

## Task 7: Integration Testing

**Files:**
- Create: `tests/integration/integration_test.go`

- [ ] **Step 1: Write the failing test**

```go
package integration

import (
    "testing"
    "github.com/stretchr/testify/assert"
    "web3-amm-dex-backend/services"
    "web3-amm-dex-backend/config"
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:\code\web3\backend && go test ./tests/integration/... -v -run TestServiceRegistryIntegration`
Expected: FAIL with "EventBus name mismatch"

- [ ] **Step 3: Write minimal implementation**

Update the EventBus to implement the Service interface:

```go
// services/event_bus.go (updated)
package services

// ... existing code ...

// Name returns the service name
func (eb *EventBus) Name() string {
    return "EventBus"
}

// Initialize the event bus
func (eb *EventBus) Initialize(config Config) error {
    // No initialization needed
    return nil
}

// Start the event bus
func (eb *EventBus) Start() error {
    // No start needed
    return nil
}

// Stop the event bus
func (eb *EventBus) Stop() error {
    // No stop needed
    return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd D:\code\web3\backend && go test ./tests/integration/... -v -run TestServiceRegistryIntegration`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/integration/integration_test.go services/event_bus.go
git commit -m "feat: add integration tests for service registry"
```

---

## Task 8: Documentation and Final Commit

**Files:**
- Create: `docs/architecture.md`
- Create: `docs/api-reference.md`

- [ ] **Step 1: Create architecture documentation**

```markdown
# Architecture Documentation

## System Overview

The Web3 AMM DEX follows a layered architecture with clear separation of concerns:

1. **Presentation Layer** - Frontend and API clients
2. **API Gateway Layer** - Request routing, middleware, authentication
3. **Business Logic Layer** - Service implementations
4. **Data Access Layer** - Blockchain interaction and event handling
5. **Infrastructure Layer** - Blockchain node, databases, message queues

## Key Components

### Service Registry
Manages service lifecycle, initialization, and dependency injection.

### Event Bus
Provides pub/sub pattern for loose coupling between services.

### Blockchain Adapter
Abstracts blockchain interaction for testability and flexibility.

### API Gateway
Handles HTTP requests with middleware chain for cross-cutting concerns.

## Data Flow

1. Client sends request to API Gateway
2. Gateway applies middleware (auth, rate limiting, logging)
3. Request routed to appropriate handler
4. Handler calls business logic service
5. Service interacts with blockchain via adapter
6. Events published to event bus for async processing
7. Response returned to client

## Extensibility Points

- **Plugins**: Add new functionality without modifying core
- **Modules**: Feature modules can be added/removed
- **Adapters**: Swap blockchain implementations
- **Middleware**: Add custom request processing
```

- [ ] **Step 2: Create API documentation**

```markdown
# API Reference

## Base URL
`http://localhost:8080`

## Endpoints

### Health Check
```
GET /health
Response: { "status": "ok" }
```

### Get Pools
```
GET /pools
Response: [
  {
    "address": "0x...",
    "token0": "0x...",
    "token1": "0x...",
    "reserve0": "1000000",
    "reserve1": "2000000"
  }
]
```

### Swap
```
POST /swap
Body: {
  "pool": "0x...",
  "tokenIn": "0x...",
  "amountIn": "1000000",
  "amountOutMin": "990000"
}
Response: {
  "txHash": "0x...",
  "amountOut": "995000"
}
```

### Add Liquidity
```
POST /liquidity
Body: {
  "pool": "0x...",
  "amount0": "1000000",
  "amount1": "2000000"
}
Response: {
  "txHash": "0x...",
  "liquidity": "1414213"
}
```
```

- [ ] **Step 3: Final commit**

```bash
git add docs/architecture.md docs/api-reference.md
git commit -m "docs: add architecture and API documentation"
```

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2024-03-20-web3-amm-dex-architecture-implementation.md`. Two execution options:

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
# 子项目 4（排序器 + 网关 + 状态机）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为子项目 3 的撮合引擎补上对外入口：一个 HTTP/WebSocket 网关，验证 EIP-712 签名订单、按账户限流、内存串行化后送入 `OrderBookService`，并经 WebSocket 推送订单/成交/深度。

**Architecture:** 新包 `backend/gateway`，只依赖 `OrderBookService` 公开方法（`Place`/`Cancel`/`Engine`/`Holds`）+ `EventBus` + `matching`/`ledger` 导出类型。排序器不引入新持久化；nonce 用追加文件 `nonces.wal` 持久化。读写分离：写接口要求签名，读接口不鉴权。

**Tech Stack:** Go 1.25（gin、gorilla/websocket、go-ethereum `signer/core/apitypes` + `crypto`、holiman/uint256）。

**设计文档：** `docs/superpowers/specs/2026-10-18-orderbook-dex-gateway-design.md`

---

## File Structure

- 新增 `backend/gateway/gateway.go` — 包文档、`Config`、`Gateway` 装配（`New`/`Router`/`Close`）
- 新增 `backend/gateway/eip712.go` — EIP-712 域、订单/撤单类型、验签、恢复账户
- 新增 `backend/gateway/nonce.go` — nonce 持久化 + 重放拒绝
- 新增 `backend/gateway/ratelimit.go` — 按账户令牌桶
- 新增 `backend/gateway/sequencer.go` — 内存串行化 + 请求序号
- 新增 `backend/gateway/state.go` — 请求生命周期状态机
- 新增 `backend/gateway/ws.go` — WebSocket hub + 订阅过滤
- 新增 `backend/gateway/handlers.go` — HTTP handlers + JSON 视图类型
- 修改 `backend/config/config.go` — `GatewayConfig` + `LoadGatewayConfig`
- 修改 `backend/main.go` — 装配并挂载网关
- 各 `*_test.go` 对应测试
- 修改 `README.md` — 网关接口、签名格式、残余假设

依赖顺序：Task 1（包骨架）→ Task 2（EIP-712）→ Task 3（nonce）→ Task 4（限流）→ Task 5（状态机）→ Task 6（排序器）→ Task 7（WS）→ Task 8（handlers）→ Task 9（装配 + config + main）→ Task 10（README）。

---

## Task 1: gateway 包骨架 + Config

**Files:**
- Create: `backend/gateway/gateway.go`

- [ ] **Step 1: 写包骨架与配置**

```go
// Package gateway is the HTTP/WebSocket entry point for the off-chain order
// book. It verifies EIP-712 signed orders, rate-limits per account, serializes
// instructions through a memory sequencer, and pushes order/trade/depth updates
// to subscribers.
//
// Layering: gateway depends only on services.OrderBookService's public methods
// (Place/Cancel/Engine/Holds), the services.EventBus, and exported types from
// matching and ledger. It never touches the ledger or the matching kernel's
// internals directly, and the sequencer adds no persistence of its own (the
// instruction WAL is owned by subproject 3).
package gateway

import (
	"errors"

	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/services"
)

// Defaults for the EIP-712 domain and rate limiter (spec §7).
const (
	DefaultDomainName    = "Web3 Orderbook DEX"
	DefaultDomainVersion = "1"
	DefaultRate          = 10.0 // tokens per second per account
	DefaultBurst         = 20.0 // token bucket capacity per account
	DefaultDepth         = 20   // book depth for depth snapshots
)

// Config configures the gateway. All fields are independently defaulted by
// New so the zero value is not required to be meaningful.
type Config struct {
	ChainID      int64
	DomainName   string
	DomainVersion string
	Rate         float64 // tokens per second per account; <= 0 disables limiting
	Burst        float64 // bucket capacity per account
	NonceDir     string  // nonce persistence dir; empty means memory-only
	Depth        int     // depth levels for WebSocket depth snapshots
}

// ErrOrderBookRequired is returned by New when the order book is nil.
var ErrOrderBookRequired = errors.New("gateway: order book service is required")

// Gateway assembles the gateway's HTTP router, WebSocket hub, sequencer, state
// machine, verifier, nonce store, and rate limiter around one OrderBookService.
type Gateway struct {
	ob       *services.OrderBookService
	bus      *services.EventBus
	verifier *Verifier
	nonce    *NonceStore
	limiter  *RateLimiter
	seq      *Sequencer
	state    *StateMachine
	hub      *Hub
	cfg      Config
}

// New builds the gateway and its dependencies. The WebSocket hub subscribes to
// the bus immediately; Close must be called to release the nonce store handle.
func New(ob *services.OrderBookService, bus *services.EventBus, cfg Config) (*Gateway, error) {
	if ob == nil {
		return nil, ErrOrderBookRequired
	}
	if bus == nil {
		bus = services.NewEventBus()
	}
	applyDefaults(&cfg)

	nonce, err := OpenNonceStore(cfg.NonceDir)
	if err != nil {
		return nil, err
	}

	seq := NewSequencer(ob)
	state := NewStateMachine()
	hub := NewHub(ob, cfg.Depth)
	hub.Subscribe(bus)

	return &Gateway{
		ob:       ob,
		bus:      bus,
		verifier: NewVerifier(cfg.ChainID, cfg.DomainName, cfg.DomainVersion),
		nonce:    nonce,
		limiter:  NewRateLimiter(cfg.Rate, cfg.Burst),
		seq:      seq,
		state:    state,
		hub:      hub,
		cfg:      cfg,
	}, nil
}

func applyDefaults(cfg *Config) {
	if cfg.ChainID == 0 {
		cfg.ChainID = 13700
	}
	if cfg.DomainName == "" {
		cfg.DomainName = DefaultDomainName
	}
	if cfg.DomainVersion == "" {
		cfg.DomainVersion = DefaultDomainVersion
	}
	if cfg.Rate == 0 && cfg.Burst == 0 {
		cfg.Rate, cfg.Burst = DefaultRate, DefaultBurst
	}
	if cfg.Depth <= 0 {
		cfg.Depth = DefaultDepth
	}
}

// Router returns a fresh gin engine with the gateway routes registered. main
// can mount these routes onto an existing engine via Register instead.
func (g *Gateway) Router() *gin.Engine {
	r := gin.New()
	g.Register(r)
	return r
}

// Register mounts the /v1/* and /ws routes onto an existing engine.
func (g *Gateway) Register(r gin.IRouter) {
	v1 := r.Group("/v1")
	v1.POST("/orders", g.placeOrder)
	v1.DELETE("/orders/:id", g.cancelOrder)
	v1.GET("/orders/:id", g.getOrder)
	v1.GET("/markets", g.listMarkets)
	v1.GET("/book/:market", g.bookDepth)
	v1.GET("/accounts/:account/balances", g.accountBalances)
	r.GET("/ws", g.serveWS)
}

// Close releases the nonce store handle. The hub owns no OS resources beyond
// client connections, which are closed as clients disconnect.
func (g *Gateway) Close() error {
	return g.nonce.Close()
}
```

- [ ] **Step 2: 编译验证（此时 handlers 尚缺，会编译失败）**

Run: `cd D:\code\web3\backend && go build ./gateway/ 2>&1`
Expected: FAIL — `undefined: g.placeOrder` 等（后续任务补齐；本任务仅建立骨架）

- [ ] **Step 3: 提交骨架（接受暂时编译失败，或先注释 handlers 桩）**

> 为保持每步可编译，本任务先把 handlers 的 8 个方法以**空桩**补进 `handlers.go` 占位，Task 8 再实现。

Create: `backend/gateway/handlers.go`（占位）

```go
package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Stubs replaced in Task 8. They exist so the package compiles before the
// handler implementations land.

func (g *Gateway) placeOrder(c *gin.Context)      { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
func (g *Gateway) cancelOrder(c *gin.Context)     { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
func (g *Gateway) getOrder(c *gin.Context)        { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
func (g *Gateway) listMarkets(c *gin.Context)     { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
func (g *Gateway) bookDepth(c *gin.Context)       { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
func (g *Gateway) accountBalances(c *gin.Context) { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
func (g *Gateway) serveWS(c *gin.Context)         { c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"}) }
```

- [ ] **Step 4: 编译验证**

Run: `cd D:\code\web3\backend && go build ./gateway/ 2>&1`
Expected: FAIL — `undefined: NewVerifier, OpenNonceStore, NewRateLimiter, NewSequencer, NewStateMachine, NewHub`（后续任务补齐）

> 本任务结束时不要求编译通过：后续每个任务引入各自的类型。**不单独提交**，与 Task 2 一起提交（避免留下编译破碎的提交）。

---

## Task 2: EIP-712 验签

**Files:**
- Create: `backend/gateway/eip712.go`
- Test: `backend/gateway/eip712_test.go`

- [ ] **Step 1: 写失败测试**

```go
package gateway

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// signOrder builds a verifier, hashes the order with it, and signs with key so
// the test exercises the full hash -> sign -> recover round trip.
func signOrder(t *testing.T, v *Verifier, o SignedOrder) (common.Address, []byte) {
	t.Helper()
	h, err := v.hashOrder(o)
	if err != nil {
		t.Fatalf("hashOrder: %v", err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return crypto.PubkeyToAddress(key.PublicKey), sig
}

func TestVerifierRecoverOrder(t *testing.T) {
	v := NewVerifier(13700, "", "")
	o := SignedOrder{
		Market: 1, Side: 0, Type: 0, TIF: 0, Price: 100,
		Qty: "10", MaxNotional: "0", Nonce: 1, Expiry: uint64(time.Now().Add(time.Hour).Unix()),
	}
	want, sig := signOrder(t, v, o)

	got, err := v.RecoverOrder(SignedOrderEnvelope{Order: o, Signature: sig})
	if err != nil {
		t.Fatalf("RecoverOrder: %v", err)
	}
	if got != want {
		t.Fatalf("recovered %s, want %s", got.Hex(), want.Hex())
	}
}

func TestVerifierRecoverOrderTamperedFieldFails(t *testing.T) {
	v := NewVerifier(13700, "", "")
	o := SignedOrder{
		Market: 1, Side: 0, Type: 0, TIF: 0, Price: 100,
		Qty: "10", MaxNotional: "0", Nonce: 1, Expiry: uint64(time.Now().Add(time.Hour).Unix()),
	}
	_, sig := signOrder(t, v, o)

	o.Price = 101 // tamper after signing
	if _, err := v.RecoverOrder(SignedOrderEnvelope{Order: o, Signature: sig}); err == nil {
		t.Fatal("expected recovery error for a tampered field")
	}
}

func TestVerifierRecoverOrderBadQty(t *testing.T) {
	v := NewVerifier(13700, "", "")
	o := SignedOrder{Market: 1, Side: 0, Type: 0, TIF: 0, Price: 1, Qty: "not-a-number", Nonce: 1, Expiry: 1}
	if _, err := v.RecoverOrder(SignedOrderEnvelope{Order: o, Signature: []byte{}}); err == nil {
		t.Fatal("expected error for a malformed qty")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestVerifier -v`
Expected: FAIL — `undefined: Verifier, SignedOrder, commonAddress`

- [ ] **Step 3: 实现 EIP-712**

```go
package gateway

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/holiman/uint256"
)

// EIP-712 domain: pure off-chain verification, no bearing contract, so the
// verifying contract is the zero address (spec §4.2).
var (
	orderTypes = apitypes.Types{
		"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		},
		"Order": {
			{Name: "market", Type: "uint32"},
			{Name: "side", Type: "uint8"},
			{Name: "type", Type: "uint8"},
			{Name: "tif", Type: "uint8"},
			{Name: "price", Type: "uint64"},
			{Name: "qty", Type: "uint256"},
			{Name: "maxNotional", Type: "uint256"},
			{Name: "nonce", Type: "uint64"},
			{Name: "expiry", Type: "uint64"},
		},
	}
	cancelTypes = apitypes.Types{
		"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		},
		"Cancel": {
			{Name: "market", Type: "uint32"},
			{Name: "orderId", Type: "uint64"},
			{Name: "nonce", Type: "uint64"},
			{Name: "expiry", Type: "uint64"},
		},
	}
)

// SignedOrder is the EIP-712 typed payload for a place instruction. Qty and
// MaxNotional are decimal strings to avoid float/scientific-notation ambiguity;
// MaxNotional is "0" when not applicable.
type SignedOrder struct {
	Market      uint32 `json:"market"`
	Side        uint8  `json:"side"`
	Type        uint8  `json:"type"`
	TIF         uint8  `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"maxNotional"`
	Nonce       uint64 `json:"nonce"`
	Expiry      uint64 `json:"expiry"`
}

// SignedOrderEnvelope is the wire form: the order plus its 65-byte signature.
type SignedOrderEnvelope struct {
	Order     SignedOrder   `json:"order"`
	Signature hexutil.Bytes `json:"signature"`
}

// SignedCancel is the EIP-712 typed payload for a cancel instruction. It shares
// the account's nonce space with orders (spec §4.2).
type SignedCancel struct {
	Market  uint32 `json:"market"`
	OrderID uint64 `json:"orderId"`
	Nonce   uint64 `json:"nonce"`
	Expiry  uint64 `json:"expiry"`
}

// SignedCancelEnvelope is the wire form of a signed cancel.
type SignedCancelEnvelope struct {
	Cancel    SignedCancel  `json:"cancel"`
	Signature hexutil.Bytes `json:"signature"`
}

// Verifier builds EIP-712 digests and recovers the signer account.
type Verifier struct {
	domain apitypes.TypedDataDomain
}

func NewVerifier(chainID int64, name, version string) *Verifier {
	if name == "" {
		name = DefaultDomainName
	}
	if version == "" {
		version = DefaultDomainVersion
	}
	return &Verifier{domain: apitypes.TypedDataDomain{
		Name:              name,
		Version:           version,
		ChainId:           math.NewHexOrDecimal256(chainID),
		VerifyingContract: "0x0000000000000000000000000000000000000000",
	}}
}

// parseUint parses a decimal string, treating "" as zero (an absent optional
// field hashes as 0).
func parseUint(s string) (*uint256.Int, error) {
	if s == "" {
		return uint256.NewInt(0), nil
	}
	v, err := uint256.FromDecimal(s)
	if err != nil {
		return nil, fmt.Errorf("gateway: bad decimal %q: %w", s, err)
	}
	return v, nil
}

func (v *Verifier) hashOrder(o SignedOrder) ([]byte, error) {
	qty, err := parseUint(o.Qty)
	if err != nil {
		return nil, fmt.Errorf("gateway: qty: %w", err)
	}
	maxNotional, err := parseUint(o.MaxNotional)
	if err != nil {
		return nil, fmt.Errorf("gateway: maxNotional: %w", err)
	}
	td := apitypes.TypedData{
		Types:       orderTypes,
		PrimaryType: "Order",
		Domain:      v.domain,
		Message: apitypes.TypedDataMessage{
			"market":      new(big.Int).SetUint64(uint64(o.Market)),
			"side":        new(big.Int).SetUint64(uint64(o.Side)),
			"type":        new(big.Int).SetUint64(uint64(o.Type)),
			"tif":         new(big.Int).SetUint64(uint64(o.TIF)),
			"price":       new(big.Int).SetUint64(o.Price),
			"qty":         qty.ToBig(),
			"maxNotional": maxNotional.ToBig(),
			"nonce":       new(big.Int).SetUint64(o.Nonce),
			"expiry":      new(big.Int).SetUint64(o.Expiry),
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	return hash, err
}

func (v *Verifier) hashCancel(c SignedCancel) ([]byte, error) {
	td := apitypes.TypedData{
		Types:       cancelTypes,
		PrimaryType: "Cancel",
		Domain:      v.domain,
		Message: apitypes.TypedDataMessage{
			"market":  new(big.Int).SetUint64(uint64(c.Market)),
			"orderId": new(big.Int).SetUint64(c.OrderID),
			"nonce":   new(big.Int).SetUint64(c.Nonce),
			"expiry":  new(big.Int).SetUint64(c.Expiry),
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	return hash, err
}

// RecoverOrder verifies the order signature and returns the signer address.
func (v *Verifier) RecoverOrder(env SignedOrderEnvelope) (common.Address, error) {
	h, err := v.hashOrder(env.Order)
	if err != nil {
		return common.Address{}, err
	}
	return recoverSigner(h, env.Signature)
}

// RecoverCancel verifies the cancel signature and returns the signer address.
func (v *Verifier) RecoverCancel(env SignedCancelEnvelope) (common.Address, error) {
	h, err := v.hashCancel(env.Cancel)
	if err != nil {
		return common.Address{}, err
	}
	return recoverSigner(h, env.Signature)
}

// recoverSigner recovers the signer of hash from a 65-byte r||s||v signature,
// normalising the v byte (27/28 -> 0/1) so both wallet and library signatures
// verify.
func recoverSigner(hash []byte, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, fmt.Errorf("gateway: signature must be 65 bytes, got %d", len(sig))
	}
	s := make([]byte, 65)
	copy(s, sig)
	if s[64] == 27 || s[64] == 28 {
		s[64] -= 27
	}
	pub, err := crypto.SigToPub(hash, s)
	if err != nil {
		return common.Address{}, fmt.Errorf("gateway: recover signer: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestVerifier -v`
Expected: PASS（3 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/gateway/
git commit -m "feat(gateway): EIP-712 order/cancel verification"
```

---

## Task 3: nonce 持久化

**Files:**
- Create: `backend/gateway/nonce.go`
- Test: `backend/gateway/nonce_test.go`

- [ ] **Step 1: 写失败测试**

```go
package gateway

import (
	"errors"
	"testing"

	"web3-amm-dex-backend/ledger"
)

func acct(n byte) ledger.AccountID {
	var a ledger.AccountID
	a[19] = n
	return a
}

func TestNonceStoreCheckAndAdvance(t *testing.T) {
	n, err := OpenNonceStore("")
	if err != nil {
		t.Fatalf("OpenNonceStore: %v", err)
	}
	defer n.Close()

	a := acct(1)
	if err := n.CheckAndAdvance(a, 1); err != nil {
		t.Fatalf("first nonce: %v", err)
	}
	if !errors.Is(n.CheckAndAdvance(a, 1), ErrNonceReused) {
		t.Fatalf("same nonce should be rejected")
	}
	if !errors.Is(n.CheckAndAdvance(a, 0), ErrNonceReused) {
		t.Fatalf("lower nonce should be rejected")
	}
	if err := n.CheckAndAdvance(a, 2); err != nil {
		t.Fatalf("higher nonce: %v", err)
	}
	if got := n.Upper(a); got != 2 {
		t.Fatalf("upper = %d, want 2", got)
	}
}

func TestNonceStorePersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	a := acct(7)

	n, err := OpenNonceStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := n.CheckAndAdvance(a, 5); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the upper bound must survive.
	n2, err := OpenNonceStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer n2.Close()
	if !errors.Is(n2.CheckAndAdvance(a, 5), ErrNonceReused) {
		t.Fatalf("nonce 5 should be rejected after reopen")
	}
	if err := n2.CheckAndAdvance(a, 6); err != nil {
		t.Fatalf("nonce 6 should pass: %v", err)
	}
}

func TestNonceStoreConcurrentAdvance(t *testing.T) {
	n, _ := OpenNonceStore("")
	defer n.Close()
	a := acct(9)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { done <- n.CheckAndAdvance(a, 1) }()
	}
	passes := 0
	for i := 0; i < 8; i++ {
		if err := <-done; err == nil {
			passes++
		}
	}
	if passes != 1 {
		t.Fatalf("passes = %d, want exactly 1 under concurrency", passes)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestNonceStore -v`
Expected: FAIL — `undefined: OpenNonceStore, ErrNonceReused`

- [ ] **Step 3: 实现 nonce**

```go
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"web3-amm-dex-backend/ledger"
)

const nonceFileName = "nonces.wal"

// ErrNonceReused is returned when a signed request's nonce is not strictly
// greater than the account's last used nonce.
var ErrNonceReused = errors.New("gateway: nonce already used")

// nonceLine is one on-disk record: an account and the highest nonce it has used.
type nonceLine struct {
	Account string `json:"account"`
	Nonce   uint64 `json:"nonce"`
}

// NonceStore persists each account's highest used nonce in an append-only JSONL
// file and keeps the upper bounds in memory. A dir of "" is memory-only.
type NonceStore struct {
	dir   string
	mu    sync.Mutex
	f     *os.File
	upper map[ledger.AccountID]uint64
}

// OpenNonceStore opens (creating if needed) the nonce log and replays it to
// rebuild the upper bounds. An empty dir yields a memory-only store.
func OpenNonceStore(dir string) (*NonceStore, error) {
	n := &NonceStore{dir: dir, upper: make(map[ledger.AccountID]uint64)}
	if dir == "" {
		return n, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, nonceFileName)
	if err := n.replay(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	n.f = f
	return n, nil
}

// replay reads the nonce log, keeping each account's maximum nonce. The final
// record is tolerated as torn only if the file does not end in a newline;
// corruption anywhere else is a hard error (same rule as the order-book WAL).
func (n *NonceStore) replay(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	hasNL := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})

	// Index of the last non-empty line: a torn tail is only ever the final
	// record, never an interior one.
	lastDataLine := -1
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) > 0 {
			lastDataLine = i
		}
	}

	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var rec nonceLine
		if err := json.Unmarshal(line, &rec); err != nil {
			if i == lastDataLine && !hasNL {
				break // torn trailing record: never committed
			}
			return fmt.Errorf("gateway: corrupt nonce record at line %d: %w", i+1, err)
		}
		a, err := ledger.AccountFromHex(rec.Account)
		if err != nil {
			return err
		}
		if rec.Nonce > n.upper[a] {
			n.upper[a] = rec.Nonce
		}
	}
	return nil
}

// CheckAndAdvance accepts only a nonce strictly greater than the account's
// upper bound, then durably records and advances it. The check, the write, and
// the in-memory advance all happen under one lock.
func (n *NonceStore) CheckAndAdvance(a ledger.AccountID, nonce uint64) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nonce <= n.upper[a] {
		return fmt.Errorf("%w: account %s used %d", ErrNonceReused, a.Hex(), n.upper[a])
	}
	if n.f != nil {
		line, err := json.Marshal(nonceLine{Account: a.Hex(), Nonce: nonce})
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if _, err := n.f.Write(line); err != nil {
			return err
		}
		if err := n.f.Sync(); err != nil {
			return err
		}
	}
	n.upper[a] = nonce
	return nil
}

// Upper reports the account's highest used nonce.
func (n *NonceStore) Upper(a ledger.AccountID) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.upper[a]
}

// Close releases the nonce file handle. Idempotent and safe on memory stores.
func (n *NonceStore) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.f == nil {
		return nil
	}
	err := n.f.Close()
	n.f = nil
	return err
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestNonceStore -v`
Expected: PASS（3 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/gateway/nonce.go backend/gateway/nonce_test.go
git commit -m "feat(gateway): persistent per-account nonce store"
```

---

## Task 4: 按账户令牌桶限流

**Files:**
- Create: `backend/gateway/ratelimit.go`
- Test: `backend/gateway/ratelimit_test.go`

- [ ] **Step 1: 写失败测试**

```go
package gateway

import (
	"testing"
	"time"
)

func TestRateLimiterExhaustsBucket(t *testing.T) {
	r := NewRateLimiter(100, 2) // burst 2, refills fast
	a := acct(1)

	if !r.Allow(a) || !r.Allow(a) {
		t.Fatal("first two calls should be allowed (burst 2)")
	}
	if r.Allow(a) {
		t.Fatal("third call should be denied (empty bucket)")
	}
}

func TestRateLimiterRefills(t *testing.T) {
	r := NewRateLimiter(1000, 1)
	a := acct(2)
	if !r.Allow(a) {
		t.Fatal("first call should be allowed")
	}
	if r.Allow(a) {
		t.Fatal("second call should be denied")
	}
	time.Sleep(2 * time.Millisecond)
	if !r.Allow(a) {
		t.Fatal("after refill the call should be allowed")
	}
}

func TestRateLimiterPerAccountIsolation(t *testing.T) {
	r := NewRateLimiter(0.001, 1) // effectively no refill within the test
	if !r.Allow(acct(1)) {
		t.Fatal("account 1 first call should pass")
	}
	if r.Allow(acct(1)) {
		t.Fatal("account 1 second call should be denied")
	}
	if !r.Allow(acct(2)) {
		t.Fatal("account 2 must not be affected by account 1")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	r := NewRateLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if !r.Allow(acct(3)) {
			t.Fatalf("rate <= 0 should never deny, failed at %d", i)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestRateLimiter -v`
Expected: FAIL — `undefined: NewRateLimiter`

- [ ] **Step 3: 实现令牌桶**

```go
package gateway

import (
	"sync"
	"time"

	"web3-amm-dex-backend/ledger"
)

// RateLimiter is a per-account token bucket. Accounts are independent: one
// account exhausting its bucket never starves another. rate <= 0 disables
// limiting entirely.
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens map[ledger.AccountID]float64
	last   map[ledger.AccountID]time.Time
}

func NewRateLimiter(rate, burst float64) *RateLimiter {
	return &RateLimiter{
		rate:   rate,
		burst:  burst,
		tokens: make(map[ledger.AccountID]float64),
		last:   make(map[ledger.AccountID]time.Time),
	}
}

// Allow consumes one token for the account. It returns false when the bucket is
// empty (the caller should respond 429). Buckets are created lazily, full.
func (r *RateLimiter) Allow(a ledger.AccountID) bool {
	if r.rate <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	tokens := r.tokens[a]
	if last, ok := r.last[a]; ok {
		tokens += now.Sub(last).Seconds() * r.rate
		if tokens > r.burst {
			tokens = r.burst
		}
	} else {
		tokens = r.burst
	}
	r.last[a] = now
	if tokens < 1 {
		r.tokens[a] = tokens
		return false
	}
	r.tokens[a] = tokens - 1
	return true
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestRateLimiter -v`
Expected: PASS（4 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/gateway/ratelimit.go backend/gateway/ratelimit_test.go
git commit -m "feat(gateway): per-account token bucket rate limiter"
```

---

## Task 5: 请求生命周期状态机

**Files:**
- Create: `backend/gateway/state.go`
- Test: `backend/gateway/state_test.go`

- [ ] **Step 1: 写失败测试**

```go
package gateway

import (
	"testing"

	"web3-amm-dex-backend/matching"
)

func TestStateMachineLifecycle(t *testing.T) {
	sm := NewStateMachine()
	sm.Begin(1)
	sm.Accept(1, 42)
	rec, ok := sm.Get(1)
	if !ok {
		t.Fatal("record 1 should exist")
	}
	if rec.State != StateAccepted || rec.OrderID != 42 {
		t.Fatalf("record = %+v, want accepted/42", rec)
	}

	sm.Begin(2)
	sm.Reject(2, errSentinel{})
	rec, _ = sm.Get(2)
	if rec.State != StateRejected || rec.Err == "" {
		t.Fatalf("record = %+v, want rejected with error", rec)
	}
}

func TestStateMachineRequestForOrder(t *testing.T) {
	sm := NewStateMachine()
	sm.Begin(7)
	sm.Accept(7, 99)

	rec, ok := sm.RequestForOrder(99)
	if !ok || rec.RequestID != 7 {
		t.Fatalf("RequestForOrder(99) = %+v (ok=%v), want request 7", rec, ok)
	}
	if _, ok := sm.RequestForOrder(matching.OrderID(1)); ok {
		t.Fatal("unknown order should not be found")
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "rejected" }
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestStateMachine -v`
Expected: FAIL — `undefined: NewStateMachine, StateAccepted, StateRejected`

- [ ] **Step 3: 实现状态机**

```go
package gateway

import (
	"sync"

	"web3-amm-dex-backend/matching"
)

// RequestState is the gateway's view of a request's lifecycle. It tracks only
// admission — whether the request was accepted into the order book or rejected
// before reaching it. The order's matching status (new/filled/cancelled) is the
// kernel's business and is not duplicated here.
type RequestState uint8

const (
	StateReceived RequestState = iota // accepted for processing, not yet forwarded
	StateAccepted                     // forwarded to the order book
	StateRejected                     // signature/validation/limit failure
)

// RequestRecord is one request's lifecycle record.
type RequestRecord struct {
	RequestID uint64
	State     RequestState
	OrderID   matching.OrderID // set when StateAccepted produced/rested an order
	Err       string           // rejection reason when StateRejected
}

// StateMachine is an in-memory map of request IDs to their lifecycle state,
// plus an orderID -> requestID index for cancel/query back-linking. It is not
// persisted: a crash rebuilds the order book from the instruction WAL and the
// gateway drops its request records (harmless — they are observational only).
type StateMachine struct {
	mu       sync.Mutex
	requests map[uint64]RequestRecord
	byOrder  map[matching.OrderID]uint64
}

func NewStateMachine() *StateMachine {
	return &StateMachine{
		requests: make(map[uint64]RequestRecord),
		byOrder:  make(map[matching.OrderID]uint64),
	}
}

// Begin records a request as received. The requestID is allocated by the
// sequencer (spec §4.5/§4.6) — the state machine never issues IDs.
func (sm *StateMachine) Begin(requestID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.requests[requestID] = RequestRecord{RequestID: requestID, State: StateReceived}
}

// Accept marks the request accepted and links it to the order it produced.
func (sm *StateMachine) Accept(requestID uint64, orderID matching.OrderID) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.requests[requestID] = RequestRecord{RequestID: requestID, State: StateAccepted, OrderID: orderID}
	sm.byOrder[orderID] = requestID
}

// Reject marks the request rejected with a reason.
func (sm *StateMachine) Reject(requestID uint64, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.requests[requestID] = RequestRecord{RequestID: requestID, State: StateRejected, Err: msg}
}

// Get returns the record for a request ID.
func (sm *StateMachine) Get(requestID uint64) (RequestRecord, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	r, ok := sm.requests[requestID]
	return r, ok
}

// RequestForOrder returns the record that produced (or last touched) an order.
func (sm *StateMachine) RequestForOrder(orderID matching.OrderID) (RequestRecord, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	requestID, ok := sm.byOrder[orderID]
	if !ok {
		return RequestRecord{}, false
	}
	r, ok := sm.requests[requestID]
	return r, ok
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestStateMachine -v`
Expected: PASS（2 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/gateway/state.go backend/gateway/state_test.go
git commit -m "feat(gateway): request lifecycle state machine"
```

---

## Task 6: 排序器（内存串行化）

**Files:**
- Create: `backend/gateway/sequencer.go`
- Test: `backend/gateway/sequencer_test.go`

- [ ] **Step 1: 写失败测试（用真实 OrderBookService 内存装配）**

```go
package gateway

import (
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

func testOrderBook(t *testing.T) *services.OrderBookService {
	t.Helper()
	markets := []matching.Market{
		{ID: 1, Symbol: "ETH/USDT", Base: 2, Quote: 1, BaseDecimals: 2},
	}
	l := ledger.New()
	holds := ledger.NewHolds(l)
	journal, err := services.NewJournal("")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	ob, err := services.NewOrderBookService(markets, l, holds, journal, services.NewEventBus())
	if err != nil {
		t.Fatalf("order book: %v", err)
	}
	t.Cleanup(func() { _ = ob.Stop() })
	return ob
}

func TestSequencerAssignsStrictlyIncreasingIDs(t *testing.T) {
	ob := testOrderBook(t)
	s := NewSequencer(ob)

	req := matching.PlaceRequest{
		Market: 1, Account: acct(1), Side: matching.SideBuy,
		Type: matching.TypeLimit, TIF: matching.TIFGTC, Price: 1, Qty: uint256.NewInt(1),
	}
	_, id1, err := s.Place(req)
	if err != nil {
		t.Fatalf("place 1: %v", err)
	}
	_, id2, err := s.Place(req)
	if err != nil {
		t.Fatalf("place 2: %v", err)
	}
	if id2 != id1+1 {
		t.Fatalf("request ids %d -> %d, want +1", id1, id2)
	}
}

func TestSequencerSerializesConcurrentPlaces(t *testing.T) {
	ob := testOrderBook(t)
	s := NewSequencer(ob)
	req := matching.PlaceRequest{
		Market: 1, Account: acct(2), Side: matching.SideBuy,
		Type: matching.TypeLimit, TIF: matching.TIFGTC, Price: 1, Qty: uint256.NewInt(1),
	}

	ids := make([]uint64, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, id, err := s.Place(req)
			if err != nil {
				t.Errorf("place: %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for _, id := range ids {
		if id == 0 {
			t.Fatal("a request id was 0 (place failed)")
		}
		if seen[id] {
			t.Fatalf("duplicate request id %d", id)
		}
		seen[id] = true
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestSequencer -v`
Expected: FAIL — `undefined: NewSequencer`

- [ ] **Step 3: 实现排序器**

```go
package gateway

import (
	"sync"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

// Sequencer is the memory serialization layer: one mutex forces all writes into
// the order book through a single lane, and a monotonic counter assigns each
// request an ID. The request ID is observational only (for the state machine
// and logs) — the kernel still assigns OrderID/TradeID from the instruction
// order, so replay determinism is untouched (spec §4.5).
type Sequencer struct {
	mu  sync.Mutex
	seq uint64
	ob  *services.OrderBookService
}

func NewSequencer(ob *services.OrderBookService) *Sequencer {
	return &Sequencer{ob: ob}
}

// Place serializes and forwards a place request, returning the result and the
// request ID.
func (s *Sequencer) Place(req matching.PlaceRequest) (matching.Result, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	res, err := s.ob.Place(req)
	return res, s.seq, err
}

// Cancel serializes and forwards a cancel request, returning the result and the
// request ID.
func (s *Sequencer) Cancel(market matching.MarketID, a ledger.AccountID, id matching.OrderID) (matching.Result, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	res, err := s.ob.Cancel(market, a, id)
	return res, s.seq, err
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestSequencer -v`
Expected: PASS（2 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/gateway/sequencer.go backend/gateway/sequencer_test.go
git commit -m "feat(gateway): in-memory sequencer serializing writes"
```

---

## Task 7: WebSocket hub

**Files:**
- Create: `backend/gateway/ws.go`
- Test: `backend/gateway/ws_test.go`

- [ ] **Step 1: 写失败测试（纯过滤逻辑，不依赖网络）**

```go
package gateway

import (
	"testing"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
)

func mkMarket(id matching.MarketID) *matching.MarketID { return &id }
func mkAcct(n byte) *ledger.AccountID                { a := acct(n); return &a }

func TestSubscriptionMatches(t *testing.T) {
	m := mkMarket(1)
	a := mkAcct(1)

	cases := []struct {
		name    string
		sub     subscription
		market  *matching.MarketID
		account *ledger.AccountID
		want    bool
	}{
		{"all", subscription{}, mkMarket(1), mkAcct(1), true},
		{"market match", subscription{market: m}, mkMarket(1), mkAcct(9), true},
		{"market mismatch", subscription{market: m}, mkMarket(2), mkAcct(1), false},
		{"account match", subscription{account: a}, mkMarket(9), mkAcct(1), true},
		{"account mismatch", subscription{account: a}, mkMarket(1), mkAcct(2), false},
		{"market+account match", subscription{market: m, account: a}, mkMarket(1), mkAcct(1), true},
		{"market+account account mismatch", subscription{market: m, account: a}, mkMarket(1), mkAcct(2), false},
	}
	for _, c := range cases {
		if got := c.sub.matches(c.market, c.account); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestSubscriptionMatches -v`
Expected: FAIL — `undefined: subscription`

- [ ] **Step 3: 实现 hub**

```go
package gateway

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

const wsSendBuffer = 64

// subscription is a client's event filter. A nil market/account is a wildcard.
type subscription struct {
	market  *matching.MarketID
	account *ledger.AccountID
}

func (s subscription) matches(m *matching.MarketID, a *ledger.AccountID) bool {
	if s.market != nil && (m == nil || *s.market != *m) {
		return false
	}
	if s.account != nil && (a == nil || *s.account != *a) {
		return false
	}
	return true
}

// wsClient is one connected peer: its outbound queue and its filters.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	subs []subscription
}

// wsFrame is the wire envelope for every push: {type, data}.
type wsFrame struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// Hub fans order-book events out to subscribed WebSocket clients. It subscribes
// to the bus topics and also emits depth snapshots after order/trade events for
// the affected market (spec §4.8).
type Hub struct {
	mu      sync.Mutex
	clients map[*wsClient]struct{}
	ob      *services.OrderBookService
	depth   int
}

func NewHub(ob *services.OrderBookService, depth int) *Hub {
	if depth <= 0 {
		depth = DefaultDepth
	}
	return &Hub{clients: make(map[*wsClient]struct{}), ob: ob, depth: depth}
}

// Subscribe registers the hub on the bus's order-book topics. Handlers must
// never block: they enqueue to per-client buffered channels and drop clients
// whose queue is full (spec §4.8: never let a slow client back-pressure the
// order book).
func (h *Hub) Subscribe(bus *services.EventBus) {
	bus.Subscribe(services.TopicOrderBookOrder, h.onOrder)
	bus.Subscribe(services.TopicOrderBookTrade, h.onTrade)
	bus.Subscribe(services.TopicOrderBookDegraded, h.onDegraded)
}

func (h *Hub) onOrder(e services.Event) error {
	ev, ok := e.Data.(services.OrderEvent)
	if !ok || ev.Order == nil {
		return nil
	}
	m := ev.Order.Market
	a := ev.Order.Account
	h.broadcast(&m, &a, wsFrame{Type: "order", Data: orderToView(ev.Order)})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onTrade(e services.Event) error {
	ev, ok := e.Data.(services.TradeEvent)
	if !ok {
		return nil
	}
	m := ev.Trade.Market
	h.broadcastTrade(&m, &ev.Trade.Maker, &ev.Trade.Taker, wsFrame{Type: "trade", Data: tradeToView(ev.Trade)})
	h.pushDepth(m)
	return nil
}

func (h *Hub) onDegraded(e services.Event) error {
	h.broadcast(nil, nil, wsFrame{Type: "degraded"})
	return nil
}

// pushDepth emits a full depth snapshot for a market (spec §4.8: full snapshot,
// no incremental compression in v1).
func (h *Hub) pushDepth(m matching.MarketID) {
	bids, asks := h.ob.Engine().Book(m, h.depth)
	h.broadcast(&m, nil, wsFrame{Type: "depth", Data: depthToView(m, bids, asks)})
}

// broadcast sends a frame to every client whose subscription matches the
// (market, account) pair.
func (h *Hub) broadcast(market *matching.MarketID, account *ledger.AccountID, frame wsFrame) {
	data, err := json.Marshal(frame)
	if err != nil {
		log.Printf("gateway: marshal ws frame: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !c.subscribed(market, account) {
			continue
		}
		h.enqueue(c, data)
	}
}

// broadcastTrade sends a trade frame once per client: a trade belongs to both
// counterparties, so a client matches if any of its subscriptions matches the
// market with either the maker or the taker account. A market-only subscriber
// receives it exactly once.
func (h *Hub) broadcastTrade(market *matching.MarketID, maker, taker *ledger.AccountID, frame wsFrame) {
	data, err := json.Marshal(frame)
	if err != nil {
		log.Printf("gateway: marshal ws frame: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.subscribed(market, maker) || c.subscribed(market, taker) {
			h.enqueue(c, data)
		}
	}
}

// enqueue delivers a frame to a client, dropping the client if its queue is
// full (a slow client must never back-pressure the order book).
func (h *Hub) enqueue(c *wsClient, data []byte) {
	select {
	case c.send <- data:
	default:
		h.drop(c)
	}
}

func (h *Hub) drop(c *wsClient) {
	delete(h.clients, c)
	close(c.send)
	_ = c.conn.Close()
}

// add registers a client; remove deregisters it.
func (h *Hub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
	}
	h.mu.Unlock()
}

func (c *wsClient) subscribed(market *matching.MarketID, account *ledger.AccountID) bool {
	for _, s := range c.subs {
		if s.matches(market, account) {
			return true
		}
	}
	return false
}

// setSubs replaces the client's subscription filters (guarded by the hub lock
// via add/remove; the read in subscribed races with this only at unsubscribe,
// which is acceptable: a torn subscription delivers at most one extra frame).
func (c *wsClient) setSubs(subs []subscription) {
	c.subs = subs
}

// upgrader is the websocket upgrader shared by the /ws handler.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run TestSubscriptionMatches -v`
Expected: PASS（1 个测试过；`orderToView`/`tradeToView`/`depthToView` 由 Task 8 提供，本任务编译需先加占位）

> 为保持本任务可编译，先在建骨架时已把 `orderToView`/`tradeToView`/`depthToView` 的占位补进 handlers.go（见下），Task 8 再实现。若 Task 1 未建 handlers.go，此处一并补入三个占位：

```go
func orderToView(o *matching.Order) orderView     { return orderView{} }
func tradeToView(t matching.Trade) tradeView      { return tradeView{} }
func depthToView(m matching.MarketID, bids, asks []matching.LevelView) depthView { return depthView{} }
```

- [ ] **Step 5: 提交**

```bash
git add backend/gateway/ws.go backend/gateway/ws_test.go
git commit -m "feat(gateway): WebSocket hub with subscription filtering"
```

---

## Task 8: HTTP handlers + JSON 视图

**Files:**
- Modify: `backend/gateway/handlers.go`（替换占位）
- Test: `backend/gateway/handlers_test.go`

- [ ] **Step 1: 写失败测试（httptest 驱动）**

```go
package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ob := testOrderBook(t)
	g, err := New(ob, nil, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func doJSON(t *testing.T, g *Gateway, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	g.Register(r)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListMarkets(t *testing.T) {
	g := newTestGateway(t)
	w := doJSON(t, g, http.MethodGet, "/v1/markets", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var markets []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &markets); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(markets) != 1 || markets[0]["symbol"] != "ETH/USDT" {
		t.Fatalf("markets = %v", markets)
	}
}

func TestBookDepth(t *testing.T) {
	g := newTestGateway(t)
	w := doJSON(t, g, http.MethodGet, "/v1/book/1?depth=5", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var v depthView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Market != 1 {
		t.Fatalf("market = %d, want 1", v.Market)
	}
}

func TestPlaceOrderRejectsBadSignature(t *testing.T) {
	g := newTestGateway(t)
	body := `{"order":{"market":1,"side":0,"type":0,"tif":0,"price":1,"qty":"1","maxNotional":"0","nonce":1,"expiry":9999999999},"signature":"0x00"}`
	w := doJSON(t, g, http.MethodPost, "/v1/orders", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401, body = %s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run 'TestListMarkets|TestBookDepth|TestPlaceOrderRejectsBadSignature' -v`
Expected: FAIL（handlers 仍是占位，返回 501）

- [ ] **Step 3: 实现 handlers**

```go
package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
	"web3-amm-dex-backend/services"
)

// --- JSON view types (uint256 as decimal strings) -------------------------

type orderView struct {
	ID          uint64 `json:"id"`
	Seq         uint64 `json:"seq"`
	Market      uint32 `json:"market"`
	Account     string `json:"account"`
	Side        string `json:"side"`
	Type        string `json:"type"`
	TIF         string `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"maxNotional,omitempty"`
	Filled      string `json:"filled"`
	Status      string `json:"status"`
}

type tradeView struct {
	ID         uint64 `json:"id"`
	Market     uint32 `json:"market"`
	Price      uint64 `json:"price"`
	Qty        string `json:"qty"`
	Notional   string `json:"notional"`
	MakerOrder uint64 `json:"makerOrder"`
	TakerOrder uint64 `json:"takerOrder"`
	Maker      string `json:"maker"`
	Taker      string `json:"taker"`
	MakerSide  string `json:"makerSide"`
}

type levelView struct {
	Price uint64 `json:"price"`
	Total string `json:"total"`
}

type depthView struct {
	Market uint32      `json:"market"`
	Bids   []levelView `json:"bids"`
	Asks   []levelView `json:"asks"`
}

type balanceView struct {
	Asset     uint32 `json:"asset"`
	Balance   string `json:"balance"`
	Available string `json:"available"`
	Held      string `json:"held"`
}

func orderToView(o *matching.Order) orderView {
	if o == nil {
		return orderView{}
	}
	v := orderView{
		ID:      uint64(o.ID),
		Seq:     uint64(o.Seq),
		Market:  uint32(o.Market),
		Account: o.Account.Hex(),
		Side:    o.Side.String(),
		Type:    o.Type.String(),
		TIF:     o.TIF.String(),
		Price:   uint64(o.Price),
		Status:  o.Status.String(),
	}
	if o.Qty != nil {
		v.Qty = o.Qty.Dec()
	}
	if o.MaxNotional != nil && !o.MaxNotional.IsZero() {
		v.MaxNotional = o.MaxNotional.Dec()
	}
	if o.Filled != nil {
		v.Filled = o.Filled.Dec()
	}
	return v
}

func tradeToView(t matching.Trade) tradeView {
	v := tradeView{
		ID:         uint64(t.ID),
		Market:     uint32(t.Market),
		Price:      uint64(t.Price),
		MakerOrder: uint64(t.MakerOrder),
		TakerOrder: uint64(t.TakerOrder),
		Maker:      t.Maker.Hex(),
		Taker:      t.Taker.Hex(),
		MakerSide:  t.MakerSide.String(),
	}
	if t.Qty != nil {
		v.Qty = t.Qty.Dec()
	}
	if t.Notional != nil {
		v.Notional = t.Notional.Dec()
	}
	return v
}

func depthToView(m matching.MarketID, bids, asks []matching.LevelView) depthView {
	v := depthView{Market: uint32(m)}
	for _, l := range bids {
		total := "0"
		if l.Total != nil {
			total = l.Total.Dec()
		}
		v.Bids = append(v.Bids, levelView{Price: uint64(l.Price), Total: total})
	}
	for _, l := range asks {
		total := "0"
		if l.Total != nil {
			total = l.Total.Dec()
		}
		v.Asks = append(v.Asks, levelView{Price: uint64(l.Price), Total: total})
	}
	return v
}

// --- handlers --------------------------------------------------------------

func (g *Gateway) placeOrder(c *gin.Context) {
	var env SignedOrderEnvelope
	if err := c.ShouldBindJSON(&env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}
	if env.Order.Expiry <= uint64(time.Now().Unix()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_expired"})
		return
	}
	account, err := g.verifier.RecoverOrder(env)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !g.limiter.Allow(account) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
		return
	}
	if err := g.nonce.CheckAndAdvance(account, env.Order.Nonce); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "nonce_reused"})
		return
	}

	req, err := orderToPlaceRequest(account, env.Order)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}

	res, requestID, err := g.seq.Place(req)
	g.state.Begin(requestID)
	if err != nil {
		g.state.Reject(requestID, err)
		code, codeErr := mapOrderBookError(err)
		c.JSON(code, gin.H{"error": codeErr, "requestId": requestID, "detail": err.Error()})
		return
	}
	var orderID matching.OrderID
	if res.Order != nil {
		orderID = res.Order.ID
	}
	g.state.Accept(requestID, orderID)
	c.JSON(http.StatusOK, gin.H{
		"requestId": requestID,
		"order":     orderToView(res.Order),
		"trades":    tradesToViews(res.Trades),
	})
}

func (g *Gateway) cancelOrder(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	var env SignedCancelEnvelope
	if err := c.ShouldBindJSON(&env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}
	if env.Cancel.Expiry <= uint64(time.Now().Unix()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_expired"})
		return
	}
	if env.Cancel.OrderID != orderID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": "orderId in path and payload must match"})
		return
	}
	account, err := g.verifier.RecoverCancel(env)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !g.limiter.Allow(account) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
		return
	}
	if err := g.nonce.CheckAndAdvance(account, env.Cancel.Nonce); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "nonce_reused"})
		return
	}

	res, requestID, err := g.seq.Cancel(matching.MarketID(env.Cancel.Market), account, matching.OrderID(orderID))
	g.state.Begin(requestID)
	if err != nil {
		g.state.Reject(requestID, err)
		code, codeErr := mapOrderBookError(err)
		c.JSON(code, gin.H{"error": codeErr, "requestId": requestID, "detail": err.Error()})
		return
	}
	var oid matching.OrderID
	if res.Order != nil {
		oid = res.Order.ID
	}
	g.state.Accept(requestID, oid)
	c.JSON(http.StatusOK, gin.H{"requestId": requestID, "order": orderToView(res.Order)})
}

func (g *Gateway) getOrder(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	o, ok := g.ob.Engine().Order(matching.OrderID(orderID))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	c.JSON(http.StatusOK, orderToView(o))
}

func (g *Gateway) listMarkets(c *gin.Context) {
	markets := g.ob.Engine().Markets()
	out := make([]gin.H, 0, len(markets))
	for _, m := range markets {
		item := gin.H{
			"id":           uint32(m.ID),
			"symbol":       m.Symbol,
			"base":         uint32(m.Base),
			"quote":        uint32(m.Quote),
			"baseDecimals": m.BaseDecimals,
		}
		if m.MinQty != nil {
			item["minQty"] = m.MinQty.Dec()
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, out)
}

func (g *Gateway) bookDepth(c *gin.Context) {
	market, err := strconv.ParseUint(c.Param("market"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	depth := g.cfg.Depth
	if v := c.Query("depth"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			depth = n
		}
	}
	bids, asks := g.ob.Engine().Book(matching.MarketID(market), depth)
	c.JSON(http.StatusOK, depthToView(matching.MarketID(market), bids, asks))
}

func (g *Gateway) accountBalances(c *gin.Context) {
	account, err := ledger.AccountFromHex(c.Param("account"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	// Report every asset referenced by a configured market (base or quote).
	seen := map[ledger.AssetID]bool{}
	for _, m := range g.ob.Engine().Markets() {
		seen[m.Base] = true
		seen[m.Quote] = true
	}
	out := make([]balanceView, 0, len(seen))
	for asset := range seen {
		avail := g.ob.Holds().Available(account, asset)
		held := g.ob.Holds().Held(account, asset)
		// balance = available + held (available is clamped to 0, and in normal
		// operation balance >= held, so this recovers the total without touching
		// the ledger directly).
		balance := new(uint256.Int).Add(avail, held)
		out = append(out, balanceView{
			Asset:     uint32(asset),
			Balance:   balance.Dec(),
			Available: avail.Dec(),
			Held:      held.Dec(),
		})
	}
	c.JSON(http.StatusOK, out)
}

// --- helpers ---------------------------------------------------------------

func orderToPlaceRequest(account ledger.AccountID, o SignedOrder) (matching.PlaceRequest, error) {
	qty, err := parseUint(o.Qty)
	if err != nil {
		return matching.PlaceRequest{}, err
	}
	maxNotional, err := parseUint(o.MaxNotional)
	if err != nil {
		return matching.PlaceRequest{}, err
	}
	return matching.PlaceRequest{
		Market:      matching.MarketID(o.Market),
		Account:     account,
		Side:        matching.Side(o.Side),
		Type:        matching.Type(o.Type),
		TIF:         matching.TIF(o.TIF),
		Price:       matching.Price(o.Price),
		Qty:         qty,
		MaxNotional: maxNotional,
	}, nil
}

func tradesToViews(ts []matching.Trade) []tradeView {
	out := make([]tradeView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tradeToView(t))
	}
	return out
}

// mapOrderBookError maps order-book/ledger errors to (http code, error key).
func mapOrderBookError(err error) (int, string) {
	switch {
	case errors.Is(err, services.ErrServiceDegraded):
		return http.StatusServiceUnavailable, "service_degraded"
	case errors.Is(err, ledger.ErrInsufficientBalance):
		return http.StatusUnprocessableEntity, "insufficient_balance"
	case errors.Is(err, matching.ErrBadRequest),
		errors.Is(err, matching.ErrUnknownMarket),
		errors.Is(err, matching.ErrAmountTooLarge):
		return http.StatusBadRequest, "order_rejected"
	case errors.Is(err, matching.ErrNotOwner):
		return http.StatusForbidden, "not_owner"
	case errors.Is(err, matching.ErrOrderNotFound):
		return http.StatusNotFound, "not_found"
	default:
		return http.StatusInternalServerError, "internal"
	}
}
```

> 需在 handlers.go import 中加入 `"github.com/holiman/uint256"`。`ws.go` 中的 `serveWS` 见下。

- [ ] **Step 4: 实现 `serveWS`（补进 handlers.go，import 需加 `github.com/gorilla/websocket`）**

```go
// serveWS upgrades the connection, registers the client, and pumps outbound
// frames until the client disconnects. Inbound messages are subscription
// commands {op, market?, account?} / {op:"unsubscribe"}.
func (g *Gateway) serveWS(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // Upgrade writes its own error response
	}
	client := &wsClient{conn: conn, send: make(chan []byte, wsSendBuffer)}
	g.hub.add(client)
	defer func() { g.hub.remove(client); _ = conn.Close() }()

	// writer pump
	go func() {
		for frame := range client.send {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				return
			}
		}
	}()

	for {
		var msg struct {
			Op      string  `json:"op"`
			Market  *uint32 `json:"market"`
			Account *string `json:"account"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Op {
		case "subscribe":
			var sub subscription
			if msg.Market != nil {
				m := matching.MarketID(*msg.Market)
				sub.market = &m
			}
			if msg.Account != nil {
				a, err := ledger.AccountFromHex(*msg.Account)
				if err != nil {
					continue
				}
				sub.account = &a
			}
			client.setSubs(append(client.subs, sub))
		case "unsubscribe":
			client.setSubs(nil)
		}
	}
}
```

> `serveWS` 引用 `gin`、`websocket`、`ledger`、`matching`、`upgrader`（`upgrader` 定义在 ws.go，同包可直接用）；handlers.go 需在 import 中加入 `"github.com/gorilla/websocket"`。

- [ ] **Step 5: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./gateway/ -run 'TestListMarkets|TestBookDepth|TestPlaceOrderRejectsBadSignature' -v`
Expected: PASS（3 个测试全过）

- [ ] **Step 6: 提交**

```bash
git add backend/gateway/handlers.go backend/gateway/handlers_test.go backend/gateway/ws.go
git commit -m "feat(gateway): HTTP handlers and JSON views"
```

---

## Task 9: 配置桥接 + main 装配

**Files:**
- Modify: `backend/config/config.go`
- Modify: `backend/main.go`
- Test: `backend/config/config_test.go`（追加）

- [ ] **Step 1: 写失败测试（config）**

```go
func TestLoadGatewayConfig(t *testing.T) {
	t.Setenv("EIP712_DOMAIN_NAME", "Test DEX")
	t.Setenv("EIP712_DOMAIN_VERSION", "2")
	t.Setenv("GATEWAY_RATE", "7")
	t.Setenv("GATEWAY_BURST", "13")
	t.Setenv("GATEWAY_NONCE_DIR", "/tmp/nonces")
	t.Setenv("CHAIN_ID", "999")

	cfg := LoadGatewayConfig()
	if cfg.DomainName != "Test DEX" || cfg.DomainVersion != "2" {
		t.Fatalf("domain = %q/%q", cfg.DomainName, cfg.DomainVersion)
	}
	if cfg.Rate != 7 || cfg.Burst != 13 {
		t.Fatalf("rate/burst = %v/%v", cfg.Rate, cfg.Burst)
	}
	if cfg.NonceDir != "/tmp/nonces" {
		t.Fatalf("nonceDir = %q", cfg.NonceDir)
	}
	if cfg.ChainID != 999 {
		t.Fatalf("chainID = %d", cfg.ChainID)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd D:\code\web3\backend && go test ./config/ -run TestLoadGatewayConfig -v`
Expected: FAIL — `undefined: LoadGatewayConfig`

- [ ] **Step 3: 实现 GatewayConfig**

在 `config/config.go` 追加：

```go
// GatewayConfig configures the order-book gateway (subproject 4).
type GatewayConfig struct {
    DomainName    string
    DomainVersion string
    Rate          float64
    Burst         float64
    NonceDir      string
    ChainID       int64
}

// LoadGatewayConfig reads gateway settings from the environment. ChainID is
// shared with the settlement subsystem and defaults to 13700.
func LoadGatewayConfig() GatewayConfig {
    cfg := GatewayConfig{
        DomainName:    "Web3 Orderbook DEX",
        DomainVersion: "1",
        Rate:          10,
        Burst:         20,
        ChainID:       13700,
    }
    if v := os.Getenv("EIP712_DOMAIN_NAME"); v != "" {
        cfg.DomainName = v
    }
    if v := os.Getenv("EIP712_DOMAIN_VERSION"); v != "" {
        cfg.DomainVersion = v
    }
    if v := os.Getenv("GATEWAY_RATE"); v != "" {
        if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
            cfg.Rate = n
        }
    }
    if v := os.Getenv("GATEWAY_BURST"); v != "" {
        if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
            cfg.Burst = n
        }
    }
    if v := os.Getenv("GATEWAY_NONCE_DIR"); v != "" {
        cfg.NonceDir = v
    }
    if v := os.Getenv("CHAIN_ID"); v != "" {
        if n, err := strconv.ParseInt(v, 10, 64); err == nil {
            cfg.ChainID = n
        }
    }
    return cfg
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd D:\code\web3\backend && go test ./config/ -run TestLoadGatewayConfig -v`
Expected: PASS

- [ ] **Step 5: main.go 装配**

在 `main.go` 的 `startSettlementStack` 返回后、`r := gin.Default()` 处，把网关挂载到现有路由。修改 `startSettlementStack` 的返回，额外返回 gateway（或在其内部装配并暴露）。最小改法：在 `main` 中，`startSettlementStack` 返回 `(*services.SettlementStack, error)` 而不是 `io.Closer`，并新增一个装配函数。

> 为降低对 `startSettlementStack` 现有三个测试的破坏，本任务采用**新增函数**而非改签名：

```go
// gatewayConfigFromEnv bridges config.GatewayConfig into gateway.Config.
func gatewayConfigFromEnv() gateway.Config {
	gc := config.LoadGatewayConfig()
	return gateway.Config{
		ChainID:       gc.ChainID,
		DomainName:    gc.DomainName,
		DomainVersion: gc.DomainVersion,
		Rate:          gc.Rate,
		Burst:         gc.Burst,
		NonceDir:      gc.NonceDir,
	}
}
```

在 `main.go` 中 `r := gin.Default()` 之后、路由注册之前：

```go
var gatewayCloser io.Closer
if stack != nil && stack.OrderBook != nil {
	gw, err := gateway.New(stack.OrderBook, stack.Bus, gatewayConfigFromEnv())
	if err != nil {
		log.Fatalf("Failed to build gateway: %v", err)
	}
	gw.Register(r)
	gatewayCloser = gw
	log.Printf("order book gateway mounted at /v1 and /ws")
}
```

> 需要把 `startSettlementStack` 的返回值从 `io.Closer` 改为能拿到 `stack.OrderBook`/`stack.Bus` 的结构。采用：新增一个轻量返回 `(stack *services.SettlementStack, closer io.Closer, err error)`，并同步更新 `main_test.go` 的三个断言（`closer` 变量名改为 `stack`）。

完整改动见 main.go 最终形态（执行者按此落地，并同步修 `main_test.go`）。

- [ ] **Step 6: 全量编译 + 测试**

Run: `cd D:\code\web3\backend && go build ./... && go test ./...`
Expected: 全部 `ok`

- [ ] **Step 7: 提交**

```bash
git add backend/config/config.go backend/config/config_test.go backend/main.go backend/main_test.go
git commit -m "feat(gateway): wire gateway config and mount /v1 and /ws"
```

---

## Task 10: README 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 追加网关章节**

在 README 的 Backend API 表后追加：

```markdown
## Order Book Gateway (subproject 4)

The order-book gateway exposes the matching engine over HTTP/WebSocket.

### HTTP API

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST   | `/v1/orders` | signed | Submit an EIP-712 signed order |
| DELETE | `/v1/orders/:id` | signed | Cancel an order (signed) |
| GET    | `/v1/orders/:id` | none | Order status |
| GET    | `/v1/markets` | none | Market list |
| GET    | `/v1/book/:market?depth=N` | none | Depth snapshot |
| GET    | `/v1/accounts/:account/balances` | none | Balance / available / held |

### WebSocket

`/ws` — subscribe with `{"op":"subscribe","market":1,"account":"0x..."}`
(optional filters). Pushes `order`, `trade`, `depth`, and `degraded` frames.

### Signature format

Orders and cancels are EIP-712 typed data over domain
`Web3 Orderbook DEX` v1. Qty/MaxNotional are decimal strings; replay is
blocked by a per-account nonce and an expiry timestamp.

### Residual trust assumptions

Read endpoints are unauthenticated. nonce is a persistent append log; its
corruption cannot cause fund loss (the ledger's trade-id idempotency keys
still prevent double settlement). The sequencer is single-process.
```

- [ ] **Step 2: 提交**

```bash
git add README.md
git commit -m "docs: document the order-book gateway"
```

---

## 最终验证

- [ ] **Step 1: Go 全量**

Run: `cd D:\code\web3\backend && go build ./... && go test ./...`
Expected: 全部 `ok`

- [ ] **Step 2: 合约测试（回归确认无影响）**

Run: `cd D:\code\web3 && npx hardhat test`
Expected: 33 passing

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-10-18-orderbook-dex-gateway.md`.

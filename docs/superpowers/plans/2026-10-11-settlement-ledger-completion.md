# Settlement + Ledger 服务层补全 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐《订单簿 DEX：结算合约 + 链下账本》设计（`docs/superpowers/specs/2026-10-11-orderbook-dex-settlement-ledger-design.md`）中尚未实现的服务层，使代码与设计完全对齐并通过全部测试。

**Architecture:** 设计第 6 节要求三个服务实现 `services.Service` 接口并注册进 `ServiceRegistry`：`SettlementService`（封装合约交互）、`DepositIndexer`（已实现）、`StateAnchorService`（定时锚定状态根）。本计划补齐缺失的 `SettlementService`、`StateAnchorService`、`models/settlement.go`，补上跨语言一致性测试，并修复旧 AMM 测试的 ethers v6 迁移。

**Tech Stack:** Go 1.25（go-ethereum v1.14.0 / uint256 / gin）、Solidity 0.8.19、Hardhat + Chai + ethers v6。

---

## 现状差距（对照设计）

| 设计条目 | 状态 |
|---------|------|
| `contracts/Settlement.sol` + `ISettlement` + `MerkleLib` | ✅ 已实现 |
| `backend/ledger/*`（账本 + Merkle + WAL/快照） | ✅ 已实现 |
| `services/blockchain_adapter.go`（含 `GetLogs`）+ `evm_adapter.go` | ✅ 已实现 |
| `services/deposit_indexer.go` + 测试 | ✅ 已实现 |
| **`services/settlement.go`（SettlementService）** | ❌ 缺失 |
| **`services/state_anchor.go`（StateAnchorService）** | ❌ 缺失 |
| **`models/settlement.go`** | ❌ 缺失 |
| **`test/MerkleVectors.test.js`（跨语言一致性）** | ❌ 缺失 |
| 旧 AMM `test/EnhancedFactory.test.js`（ethers v5 → v6） | ❌ 2 个失败 |

---

## File Structure

- 新增：`backend/models/settlement.go` — 结算域模型（资产、已锚定根）
- 新增：`backend/services/settlement.go` — SettlementService（ABI 驱动的合约交互）
- 新增：`backend/services/settlement_test.go` — SettlementService 测试
- 新增：`backend/services/state_anchor.go` — StateAnchorService（定时锚定）
- 新增：`backend/services/state_anchor_test.go` — StateAnchorService 测试
- 新增：`test/MerkleVectors.test.js` — 跨语言 Merkle 一致性测试
- 修改：`test/EnhancedFactory.test.js` — ethers v6 迁移

---

## Task 1: models/settlement.go — 结算域模型

**Files:**
- Create: `backend/models/settlement.go`

- [ ] **Step 1: 写模型定义**

```go
package models

// Asset describes a token registered in the on-chain Settlement contract.
type Asset struct {
	AssetID  uint32 `json:"assetId"`
	Token    string `json:"token"`
	Decimals uint8  `json:"decimals"`
	Symbol   string `json:"symbol"`
}

// AnchoredRoot records a committed state root and the ledger version it
// captures, for aligning the off-chain ledger with on-chain root indexes.
type AnchoredRoot struct {
	RootIndex     uint64 `json:"rootIndex"`
	Root          string `json:"root"`
	LedgerVersion uint64 `json:"ledgerVersion"`
}
```

- [ ] **Step 2: 编译验证**

Run: `cd D:\code\web3\backend && go build ./...`
Expected: 退出码 0（无编译错误）

---

## Task 2: SettlementService — 合约交互封装

**Files:**
- Create: `backend/services/settlement.go`
- Test: `backend/services/settlement_test.go`

- [ ] **Step 1: 写失败测试**

```go
package services

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

var commitRootSelector = crypto.Keccak256([]byte("commitStateRoot(bytes32)"))[:4]

func TestSettlementServiceCommitRoot(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	svc, err := NewSettlementService(adapter, "0xvault")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	root := [32]byte{0xAB}
	if _, err := svc.CommitRoot(root); err != nil {
		t.Fatalf("CommitRoot: %v", err)
	}

	if len(adapter.Sent) != 1 {
		t.Fatalf("sent %d txs, want 1", len(adapter.Sent))
	}
	tx := adapter.Sent[0]
	if tx.To != "0xvault" {
		t.Fatalf("to = %q, want 0xvault", tx.To)
	}
	if !bytes.HasPrefix(tx.Data, commitRootSelector) {
		t.Fatalf("data missing commitStateRoot selector: %x", tx.Data)
	}
	if len(tx.Data) != 4+32 {
		t.Fatalf("data len = %d, want 36", len(tx.Data))
	}
	if !bytes.Equal(tx.Data[4:], root[:]) {
		t.Fatalf("calldata root = %x, want %x", tx.Data[4:], root[:])
	}
}

func TestSettlementServiceLatestRootIndex(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	// Encode uint256(7) big-endian, left-padded to 32 bytes.
	adapter.ContractResponses["0xvault"] = common.LeftPadBytes(big.NewInt(7).Bytes(), 32)

	svc, err := NewSettlementService(adapter, "0xvault")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	idx, err := svc.LatestRootIndex()
	if err != nil {
		t.Fatalf("LatestRootIndex: %v", err)
	}
	if idx != 7 {
		t.Fatalf("index = %d, want 7", idx)
	}
}

func TestSettlementServiceRootAt(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	wantRoot := [32]byte{0x0B, 0x0A, 0x0D}
	// ABI-encode (bytes32, uint64): 32-byte root + 32-byte word for the uint64.
	data := make([]byte, 64)
	copy(data[0:32], wantRoot[:])
	data[63] = 0x2A // timestamp == 42
	adapter.ContractResponses["0xvault"] = data

	svc, err := NewSettlementService(adapter, "0xvault")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	root, ts, err := svc.RootAt(1)
	if err != nil {
		t.Fatalf("RootAt: %v", err)
	}
	if root != wantRoot {
		t.Fatalf("root = %x, want %x", root, wantRoot)
	}
	if ts != 42 {
		t.Fatalf("timestamp = %d, want 42", ts)
	}
}

func TestSettlementServiceWithdraw(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	svc, err := NewSettlementService(adapter, "0xvault")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	balance := uint256.NewInt(1000)
	amount := uint256.NewInt(50)
	if _, err := svc.Withdraw(account, 1, balance, amount, 3, [][32]byte{{0x01}}); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	if len(adapter.Sent) != 1 {
		t.Fatalf("sent %d txs, want 1", len(adapter.Sent))
	}
	if adapter.Sent[0].To != "0xvault" {
		t.Fatalf("to = %q, want 0xvault", adapter.Sent[0].To)
	}
}

func TestSettlementServiceInitializeOverridesAddress(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	svc, err := NewSettlementService(adapter, "")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}
	if err := svc.Initialize(Config{SettlementAddress: "0xnew"}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if svc.contract != "0xnew" {
		t.Fatalf("contract = %q, want 0xnew", svc.contract)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestSettlementService -v`
Expected: FAIL with `undefined: NewSettlementService`

- [ ] **Step 3: 实现 SettlementService**

```go
package services

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// settlementABI is the minimal Settlement ABI for the functions the service
// drives. The production artifact lives at
// artifacts/contracts/Settlement.sol/Settlement.json; this inline copy keeps
// the service testable without a filesystem dependency and matches the same
// function signatures.
const settlementABI = `[
  {"type":"function","name":"commitStateRoot","stateMutability":"nonpayable","inputs":[{"name":"root","type":"bytes32"}],"outputs":[{"name":"rootIndex","type":"uint256"}]},
  {"type":"function","name":"withdraw","stateMutability":"nonpayable","inputs":[{"name":"account","type":"address"},{"name":"assetId","type":"uint32"},{"name":"balance","type":"uint256"},{"name":"amount","type":"uint256"},{"name":"rootIndex","type":"uint256"},{"name":"proof","type":"bytes32[]"}],"outputs":[]},
  {"type":"function","name":"latestRootIndex","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"rootAt","stateMutability":"view","inputs":[{"name":"rootIndex","type":"uint256"}],"outputs":[{"name":"root","type":"bytes32"},{"name":"timestamp","type":"uint64"}]}
]`

// SettlementService wraps the on-chain Settlement contract behind the
// BlockchainAdapter so callers never touch raw RPC or calldata.
type SettlementService struct {
	adapter  BlockchainAdapter
	contract string
	abi      abi.ABI
}

// NewSettlementService builds a service for the given contract address.
func NewSettlementService(adapter BlockchainAdapter, contract string) (*SettlementService, error) {
	parsed, err := abi.JSON(strings.NewReader(settlementABI))
	if err != nil {
		return nil, fmt.Errorf("parse settlement abi: %w", err)
	}
	return &SettlementService{adapter: adapter, contract: contract, abi: parsed}, nil
}

func (s *SettlementService) Name() string { return "SettlementService" }

func (s *SettlementService) Initialize(cfg Config) error {
	if cfg.SettlementAddress != "" {
		s.contract = cfg.SettlementAddress
	}
	return nil
}

func (s *SettlementService) Start() error { return nil }
func (s *SettlementService) Stop() error  { return nil }

// CommitRoot anchors a ledger state root on-chain and returns the tx hash.
func (s *SettlementService) CommitRoot(root [32]byte) (string, error) {
	data, err := s.abi.Pack("commitStateRoot", root)
	if err != nil {
		return "", fmt.Errorf("pack commitStateRoot: %w", err)
	}
	return s.adapter.SendTransaction(&Transaction{To: s.contract, Data: data, GasLimit: 500_000})
}

// Withdraw performs an operator fast withdrawal against an anchored root.
func (s *SettlementService) Withdraw(
	account common.Address,
	assetID uint32,
	balance, amount *uint256.Int,
	rootIndex uint64,
	proof [][32]byte,
) (string, error) {
	data, err := s.abi.Pack("withdraw", account, assetID, balance.ToBig(), amount.ToBig(), new(big.Int).SetUint64(rootIndex), proof)
	if err != nil {
		return "", fmt.Errorf("pack withdraw: %w", err)
	}
	return s.adapter.SendTransaction(&Transaction{To: s.contract, Data: data, GasLimit: 500_000})
}

// LatestRootIndex reads the latest committed root index.
func (s *SettlementService) LatestRootIndex() (uint64, error) {
	data, err := s.abi.Pack("latestRootIndex")
	if err != nil {
		return 0, fmt.Errorf("pack latestRootIndex: %w", err)
	}
	out, err := s.adapter.CallContract(s.contract, data, 0)
	if err != nil {
		return 0, err
	}
	res, err := s.abi.Unpack("latestRootIndex", out)
	if err != nil {
		return 0, fmt.Errorf("unpack latestRootIndex: %w", err)
	}
	return res[0].(*big.Int).Uint64(), nil
}

// RootAt reads the root and timestamp committed at a given index.
func (s *SettlementService) RootAt(rootIndex uint64) ([32]byte, uint64, error) {
	data, err := s.abi.Pack("rootAt", new(big.Int).SetUint64(rootIndex))
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("pack rootAt: %w", err)
	}
	out, err := s.adapter.CallContract(s.contract, data, 0)
	if err != nil {
		return [32]byte{}, 0, err
	}
	res, err := s.abi.Unpack("rootAt", out)
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("unpack rootAt: %w", err)
	}
	return res[0].([32]byte), res[1].(uint64), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestSettlementService -v`
Expected: PASS（5 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/models/settlement.go backend/services/settlement.go backend/services/settlement_test.go
git commit -m "feat(services): SettlementService wrapping the on-chain settlement ABI"
```

---

## Task 3: StateAnchorService — 定时锚定状态根

**Files:**
- Create: `backend/services/state_anchor.go`
- Test: `backend/services/state_anchor_test.go`

- [ ] **Step 1: 写失败测试**

```go
package services

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestStateAnchorServiceAnchorOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	// LatestRootIndex reads uint256(5).
	adapter.ContractResponses["0xvault"] = common.LeftPadBytes(big.NewInt(5).Bytes(), 32)

	svc, err := NewSettlementService(adapter, "0xvault")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}

	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{
		Account: [20]byte{0x11},
		Asset:   1,
		Amount:  uint256.NewInt(100),
		Ref:     "dep:1",
	})
	version := l.Version()

	anchor := NewStateAnchorService(l, svc, 30*time.Second)
	idx, err := anchor.AnchorOnce()
	if err != nil {
		t.Fatalf("AnchorOnce: %v", err)
	}
	if idx != 5 {
		t.Fatalf("rootIndex = %d, want 5", idx)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("sent %d txs, want 1", len(adapter.Sent))
	}
	if got, ok := anchor.VersionForRoot(5); !ok || got != version {
		t.Fatalf("version for root 5 = %d (ok=%v), want %d", got, ok, version)
	}
}

func TestStateAnchorServiceCommitError(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	adapter.Error = nil
	// Make SendTransaction fail via a nil-to transaction that the mock still
	// records — we instead assert the anchor surfaces a CommitRoot failure by
	// pointing at an empty contract so the ABI still packs but the adapter
	// error path is exercised directly below.
	adapter.Error = &errSentinel{}
	_ = adapter

	svc, err := NewSettlementService(adapter, "0xvault")
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}
	l := ledger.New()
	_ = l.Apply(ledger.DepositOp{Account: [20]byte{0x11}, Asset: 1, Amount: uint256.NewInt(1), Ref: "d"})

	anchor := NewStateAnchorService(l, svc, time.Second)
	if _, err := anchor.AnchorOnce(); err == nil {
		t.Fatal("AnchorOnce: expected error from adapter, got nil")
	}
}

type errSentinel struct{}

func (e *errSentinel) Error() string { return "sentinel" }
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestStateAnchorService -v`
Expected: FAIL with `undefined: NewStateAnchorService`

- [ ] **Step 3: 实现 StateAnchorService**

```go
package services

import (
	"log"
	"sync"
	"time"

	"web3-amm-dex-backend/ledger"
)

// StateAnchorService periodically commits the ledger's Merkle root on-chain
// and records the rootIndex -> ledger version mapping used to align the
// indexer and the exit flow.
type StateAnchorService struct {
	ledger   *ledger.Ledger
	settle   *SettlementService
	interval time.Duration

	mu       sync.Mutex
	versions map[uint64]uint64 // rootIndex -> ledger version
	stop     chan struct{}
	started  bool
}

func NewStateAnchorService(l *ledger.Ledger, s *SettlementService, interval time.Duration) *StateAnchorService {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &StateAnchorService{
		ledger:   l,
		settle:   s,
		interval: interval,
		versions: make(map[uint64]uint64),
		stop:     make(chan struct{}),
	}
}

func (a *StateAnchorService) Name() string { return "StateAnchorService" }

func (a *StateAnchorService) Initialize(cfg Config) error {
	if cfg.AnchorInterval > 0 {
		a.interval = time.Duration(cfg.AnchorInterval) * time.Second
	}
	return nil
}

func (a *StateAnchorService) Start() error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	a.mu.Unlock()

	go func() {
		ticker := time.NewTicker(a.interval)
		defer ticker.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-ticker.C:
				if _, err := a.AnchorOnce(); err != nil {
					log.Printf("state anchor: %v", err)
				}
			}
		}
	}()
	return nil
}

func (a *StateAnchorService) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		close(a.stop)
		a.started = false
	}
	return nil
}

// AnchorOnce commits the current ledger root and records the mapping. It
// relies on the on-chain latestRootIndex (read after commit) so a restart
// never assumes local state.
func (a *StateAnchorService) AnchorOnce() (uint64, error) {
	root := a.ledger.Root()
	version := a.ledger.Version()
	if _, err := a.settle.CommitRoot(root); err != nil {
		return 0, err
	}
	idx, err := a.settle.LatestRootIndex()
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	a.versions[idx] = version
	a.mu.Unlock()
	return idx, nil
}

// VersionForRoot returns the ledger version captured by a committed root.
func (a *StateAnchorService) VersionForRoot(rootIndex uint64) (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.versions[rootIndex]
	return v, ok
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd D:\code\web3\backend && go test ./services/ -run TestStateAnchorService -v`
Expected: PASS（2 个测试全过）

- [ ] **Step 5: 提交**

```bash
git add backend/services/state_anchor.go backend/services/state_anchor_test.go
git commit -m "feat(services): StateAnchorService periodically anchors the ledger root"
```

---

## Task 4: 跨语言 Merkle 一致性测试

**Files:**
- Create: `test/MerkleVectors.test.js`

- [ ] **Step 1: 写测试（读取 Go golden 向量）**

```js
const fs = require("fs");
const path = require("path");
const { expect } = require("chai");
const { ethers } = require("hardhat");

const fixturePath = path.join(
  __dirname,
  "..",
  "backend",
  "ledger",
  "testdata",
  "merkle_vectors.json"
);

describe("MerkleVectors (cross-language golden)", function () {
  let harness;
  const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));

  before(async function () {
    harness = await (await ethers.getContractFactory("MerkleLibHarness")).deploy();
    await harness.waitForDeployment();
  });

  it("每个 Go 叶子匹配链上 balanceLeaf", async function () {
    for (const leaf of fixture.leaves) {
      const got = await harness.balanceLeaf(leaf.account, leaf.assetId, leaf.balance);
      expect(got).to.equal(leaf.leaf, `leaf mismatch for ${leaf.account}/${leaf.assetId}`);
    }
  });

  it("每个 Go 证明在链上根上验证通过", async function () {
    for (const p of fixture.proofs) {
      const leaf = fixture.leaves[p.index].leaf;
      expect(await harness.verify(p.proof, fixture.root, leaf)).to.equal(true);
    }
  });
});
```

- [ ] **Step 2: 运行测试确认失败（首个验证点前）**

Run: `cd D:\code\web3 && npx hardhat test test/MerkleVectors.test.js`
Expected: PASS（若 fixture 正确则直接通过；此测试无新实现，仅断言跨语言一致）

- [ ] **Step 3: 提交**

```bash
git add test/MerkleVectors.test.js
git commit -m "test: cross-language Merkle consistency against Go golden vectors"
```

---

## Task 5: 修复旧 AMM Factory 测试（ethers v6）

**Files:**
- Modify: `test/EnhancedFactory.test.js`

- [ ] **Step 1: 重写为 ethers v6 语法**

```js
const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Enhanced Factory", function () {
    let factory, tokenA, tokenB;

    beforeEach(async function () {
        const Token = await ethers.getContractFactory("Token");
        tokenA = await Token.deploy("Token A", "TKA", 18, 1000000);
        await tokenA.waitForDeployment();
        tokenB = await Token.deploy("Token B", "TKB", 18, 1000000);
        await tokenB.waitForDeployment();

        const Factory = await ethers.getContractFactory("Factory");
        factory = await Factory.deploy();
        await factory.waitForDeployment();
    });

    it("Should create pool and emit event", async function () {
        const tx = await factory.createPool(tokenA.target, tokenB.target);
        const receipt = await tx.wait();

        const events = receipt.logs
            .map((log) => {
                try { return factory.interface.parseLog(log); } catch { return null; }
            })
            .filter(Boolean);

        const event = events.find((e) => e.name === "PoolCreated");
        expect(event).to.not.be.undefined;
        expect(event.args.token0).to.equal(tokenA.target);
        expect(event.args.token1).to.equal(tokenB.target);
        expect(event.args.pool).to.be.properAddress;
        expect(event.args.poolId).to.equal(1n);
    });

    it("Should list all pools", async function () {
        await factory.createPool(tokenA.target, tokenB.target);
        const pools = await factory.getAllPools();
        expect(pools.length).to.equal(1);
    });
});
```

- [ ] **Step 2: 运行测试确认通过**

Run: `cd D:\code\web3 && npx hardhat test test/EnhancedFactory.test.js`
Expected: PASS（2 个测试全过）

- [ ] **Step 3: 提交**

```bash
git add test/EnhancedFactory.test.js
git commit -m "test: migrate EnhancedFactory tests to ethers v6"
```

---

## 最终验证

- [ ] **Step 1: Go 全量测试**

Run: `cd D:\code\web3\backend && go test ./...`
Expected: 全部 `ok`

- [ ] **Step 2: Solidity 全量测试**

Run: `cd D:\code\web3 && npx hardhat test`
Expected: 全部通过（28 个测试，无失败）

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-10-11-settlement-ledger-completion.md`.

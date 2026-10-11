package services

import (
	"strings"
	"testing"

	"github.com/holiman/uint256"
	"web3-amm-dex-backend/config"
	"web3-amm-dex-backend/ledger"
)

const testSettlementAddress = "0x00000000000000000000000000000000000000AA"

// testStackConfig is a fully-populated services.Config for stack tests. Tests
// inject a mock adapter, so the real EVM-only fields (RPCURL/OperatorKey) are
// present but never used to dial a node.
func testStackConfig(dataDir string) Config {
	return Config{
		RPCURL:                "http://localhost:8545",
		OperatorKey:           "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318",
		SettlementAddress:     testSettlementAddress,
		DataDir:               dataDir,
		RequiredConfirmations: 2,
		AnchorInterval:        3600,
		ChainID:               13700,
	}
}

func mustBuildStack(t *testing.T, cfg Config, adapter BlockchainAdapter) *SettlementStack {
	t.Helper()
	st, err := BuildSettlementStack(cfg, adapter)
	if err != nil {
		t.Fatalf("BuildSettlementStack: %v", err)
	}
	return st
}

// TestBuildSettlementStackRegistersAllServices proves the three settlement
// services (plus the injected adapter/ledger/store) are wired into a fresh
// registry and reachable by name.
func TestBuildSettlementStackRegistersAllServices(t *testing.T) {
	cfg := testStackConfig(t.TempDir())
	mock := NewMockBlockchainAdapter()

	st := mustBuildStack(t, cfg, mock)
	defer st.Close()

	if st.Adapter != BlockchainAdapter(mock) {
		t.Errorf("stack.Adapter = %v, want the injected mock", st.Adapter)
	}
	if st.Store == nil {
		t.Fatal("stack.Store is nil")
	}
	if st.Ledger == nil {
		t.Fatal("stack.Ledger is nil")
	}
	if st.Settlement == nil || st.Indexer == nil || st.Anchor == nil {
		t.Fatal("stack has a nil service")
	}

	for _, name := range []string{"SettlementService", "DepositIndexer", "StateAnchorService"} {
		svc, ok := st.Registry.Get(name)
		if !ok {
			t.Errorf("registry is missing service %q", name)
			continue
		}
		if svc.Name() != name {
			t.Errorf("Get(%q).Name() = %q", name, svc.Name())
		}
	}
}

// TestBuildSettlementStackLifecycle drives the registry through the full
// Initialize/Start/Stop cycle. A panic (e.g. a second Start spawning a loop
// that closes an already-closed channel) fails the test.
func TestBuildSettlementStackLifecycle(t *testing.T) {
	cfg := testStackConfig(t.TempDir())
	st := mustBuildStack(t, cfg, NewMockBlockchainAdapter())
	defer st.Close()

	if err := st.Registry.InitializeAll(cfg); err != nil {
		t.Fatalf("InitializeAll: %v", err)
	}
	if err := st.Registry.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if err := st.Registry.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}

// TestBuildSettlementStackRecoversLedgerFromWAL seeds a DataDir with a WAL the
// same way production does (apply through a store-backed ledger, then release
// the file handle) and asserts BuildSettlementStack rebuilds the balance AND
// the idempotency-key set via the ledger's Recover path.
func TestBuildSettlementStackRecoversLedgerFromWAL(t *testing.T) {
	dir := t.TempDir()

	account, err := ledger.AccountFromHex("0x1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	const asset = ledger.AssetID(7)
	deposit := ledger.DepositOp{Account: account, Asset: asset, Amount: uint256.NewInt(500), Ref: "seed-deposit"}

	seed, err := ledger.NewFileStore(dir)
	if err != nil {
		t.Fatalf("seed NewFileStore: %v", err)
	}
	if err := ledger.New().WithStore(seed).Apply(deposit); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	st := mustBuildStack(t, testStackConfig(dir), NewMockBlockchainAdapter())
	defer st.Close()

	if got := st.Ledger.Balance(account, asset); got.Cmp(deposit.Amount) != 0 {
		t.Fatalf("recovered balance = %s, want %s", got, deposit.Amount)
	}

	// Recover must also restore the applied-key set: replaying the seeded op
	// is a no-op rather than a double credit.
	if err := st.Ledger.Apply(deposit); err != nil {
		t.Fatalf("replaying seeded op: %v", err)
	}
	if got := st.Ledger.Balance(account, asset); got.Cmp(deposit.Amount) != 0 {
		t.Fatalf("balance after replaying applied op = %s, want %s", got, deposit.Amount)
	}
}

// TestSettlementStackCloseClosesStoreAndIsIdempotent proves Close releases the
// WAL handle (a later write fails) and that a second Close is safe.
func TestSettlementStackCloseClosesStoreAndIsIdempotent(t *testing.T) {
	cfg := testStackConfig(t.TempDir())
	st := mustBuildStack(t, cfg, NewMockBlockchainAdapter())

	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	entry := ledger.WALEntry{
		Seq: 999,
		Op: ledger.OpRecord{
			Type:    "deposit",
			Account: "0x1111111111111111111111111111111111111111",
			Asset:   1,
			Amount:  "1",
			Ref:     "after-close",
		},
	}
	if err := st.Store.AppendBatch([]ledger.WALEntry{entry}); err == nil {
		t.Fatal("AppendBatch after Close succeeded; the WAL handle was not released")
	}

	if err := st.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

// TestBuildSettlementStackWithoutAdapterRequiresConfig covers the known plan
// gap: with no injected adapter BuildSettlementStack must fail fast with a
// clear, field-specific error instead of letting NewEVMAdapter nil-panic or
// fail deep inside parsing.
func TestBuildSettlementStackWithoutAdapterRequiresConfig(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing operator key", func(c *Config) { c.OperatorKey = "" }, "operator"},
		{"missing rpc url", func(c *Config) { c.RPCURL = "" }, "rpc"},
		{"missing settlement address", func(c *Config) { c.SettlementAddress = "" }, "settlement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testStackConfig(t.TempDir())
			tc.mutate(&cfg)

			_, err := BuildSettlementStack(cfg, nil)
			if err == nil {
				t.Fatalf("BuildSettlementStack(cfg, nil) = nil error, want a clear error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestConfigFromSettlementBridgesFields pins the config -> services mapping.
func TestConfigFromSettlementBridgesFields(t *testing.T) {
	in := config.SettlementConfig{
		ContractAddress:       testSettlementAddress,
		OperatorKey:           "0xdeadbeef",
		ChainID:               13700,
		DataDir:               "some/data/dir",
		Confirmations:         3,
		AnchorIntervalSeconds: 45,
	}
	got := ConfigFromSettlement(in)

	if got.SettlementAddress != in.ContractAddress {
		t.Errorf("SettlementAddress = %q, want %q", got.SettlementAddress, in.ContractAddress)
	}
	if got.OperatorKey != in.OperatorKey {
		t.Errorf("OperatorKey = %q, want %q", got.OperatorKey, in.OperatorKey)
	}
	if got.ChainID != in.ChainID {
		t.Errorf("ChainID = %d, want %d", got.ChainID, in.ChainID)
	}
	if got.DataDir != in.DataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, in.DataDir)
	}
	if got.RequiredConfirmations != in.Confirmations {
		t.Errorf("RequiredConfirmations = %d, want %d", got.RequiredConfirmations, in.Confirmations)
	}
	if got.AnchorInterval != in.AnchorIntervalSeconds {
		t.Errorf("AnchorInterval = %d, want %d", got.AnchorInterval, in.AnchorIntervalSeconds)
	}
}

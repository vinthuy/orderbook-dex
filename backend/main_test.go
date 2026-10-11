package main

import (
	"os"
	"strings"
	"testing"

	"web3-amm-dex-backend/services"
)

// emptyGetenv is the getenv stub used by the tests: it reports every variable
// as unset, so the only environment the orchestration sees is what the test
// registers through t.Setenv (which config.LoadSettlementConfig reads).
func emptyGetenv(string) string { return "" }

// TestStartSettlementStackDisabledWithoutAddress covers the "settlement turned
// off" path: with no SETTLEMENT_ADDRESS the function must return successfully
// with a nil closer (nothing to shut down later) and must not touch the ledger
// DataDir at all.
func TestStartSettlementStackDisabledWithoutAddress(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("SETTLEMENT_ADDRESS", "")
	t.Setenv("OPERATOR_KEY", "")
	t.Setenv("DATA_DIR", dataDir)

	closer, err := startSettlementStack(emptyGetenv, nil)
	if err != nil {
		t.Fatalf("startSettlementStack with settlement disabled returned error: %v", err)
	}
	if closer != nil {
		t.Fatalf("closer = %v, want nil when settlement is disabled", closer)
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dataDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("DataDir %q has %d entries; nothing should have been started", dataDir, len(entries))
	}
}

// TestStartSettlementStackMissingOperatorKeyReturnsClearError covers the
// misconfiguration path with the real (nil) adapter: a settlement address
// without an operator key must produce a clear, field-specific error and no
// panic. It also proves startSettlementStack supplies RPCURL itself — otherwise
// BuildSettlementStack would fail on the missing RPCURL first and the error
// would blame "rpc".
func TestStartSettlementStackMissingOperatorKeyReturnsClearError(t *testing.T) {
	t.Setenv("SETTLEMENT_ADDRESS", "0x1111111111111111111111111111111111111111")
	t.Setenv("OPERATOR_KEY", "")
	t.Setenv("DATA_DIR", t.TempDir())

	closer, err := startSettlementStack(emptyGetenv, nil)
	if err == nil {
		t.Fatal("startSettlementStack with a missing OPERATOR_KEY returned nil error, want a clear error")
	}
	if closer != nil {
		t.Fatalf("closer = %v, want nil on startup failure", closer)
	}

	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "operator") {
		t.Fatalf("error %q does not mention the operator key", err)
	}
	if strings.Contains(lower, "rpc") {
		t.Fatalf("error %q blames RPCURL; startSettlementStack must supply it from the app config", err)
	}
}

// TestStartSettlementStackWithMockAdapterStartsAndCloses covers the happy path
// with an injected mock adapter: the stack builds, initializes, starts, and
// shuts down cleanly. OPERATOR_KEY is intentionally unset, which also proves
// the injected adapter bypasses the EVM-only requirement.
func TestStartSettlementStackWithMockAdapterStartsAndCloses(t *testing.T) {
	t.Setenv("SETTLEMENT_ADDRESS", "0x1111111111111111111111111111111111111111")
	t.Setenv("OPERATOR_KEY", "")
	t.Setenv("DATA_DIR", t.TempDir())

	mock := services.NewMockBlockchainAdapter()
	closer, err := startSettlementStack(emptyGetenv, mock)
	if err != nil {
		t.Fatalf("startSettlementStack with mock adapter: %v", err)
	}
	if closer == nil {
		t.Fatal("closer is nil, want a running settlement stack")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

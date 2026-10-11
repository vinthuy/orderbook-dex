package services

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

func TestSettlementServiceCommitRootEncodesCalldata(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	svc, err := NewSettlementService(adapter, ledger.New(), contract)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var root [32]byte
	root[31] = 7
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 11, root)

	txHash, rootIndex, err := svc.CommitRoot(root)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if txHash != adapter.SendTxHash {
		t.Fatalf("txHash = %q, want %q", txHash, adapter.SendTxHash)
	}
	if rootIndex != 11 {
		t.Fatalf("rootIndex = %d, want 11 (from the StateRootCommitted event)", rootIndex)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx, got %d", len(adapter.Sent))
	}
	data := adapter.Sent[0].Data
	if len(data) != 4+32 {
		t.Fatalf("calldata len = %d, want 36", len(data))
	}
	if data[35] != 7 {
		t.Fatalf("root not encoded into calldata")
	}
}

// TestSettlementServiceCommitRootWaitsForReceipt covers the mock's pending ->
// mined progression: the receipt is reported pending three times before it is
// mined, so CommitRoot must poll past all three to succeed.
func TestSettlementServiceCommitRootWaitsForReceipt(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	var root [32]byte
	root[31] = 9
	adapter.PendingReceipts[adapter.SendTxHash] = 3
	adapter.Receipts[adapter.SendTxHash] = minedCommitReceipt(contract, 4, root)

	svc, err := NewSettlementService(adapter, ledger.New(), contract)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := svc.Initialize(Config{ReceiptTimeoutSeconds: 2, ReceiptPollMillis: 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	_, rootIndex, err := svc.CommitRoot(root)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if rootIndex != 4 {
		t.Fatalf("rootIndex = %d, want 4", rootIndex)
	}
	if adapter.ReceiptCalls < 4 {
		t.Fatalf("GetTransactionReceipt calls = %d, want >= 4 (3 pending + 1 mined)", adapter.ReceiptCalls)
	}
}

// TestSettlementServiceCommitRootRevertedErrors covers a status-0 receipt: the
// commit reverted on-chain (e.g. `insolvent`) and must surface an error rather
// than a bogus rootIndex.
func TestSettlementServiceCommitRootRevertedErrors(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	adapter.Receipts[adapter.SendTxHash] = &Receipt{Status: 0}

	svc, err := NewSettlementService(adapter, ledger.New(), contract)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var root [32]byte
	if _, _, err := svc.CommitRoot(root); err == nil {
		t.Fatal("expected error for a reverted commit")
	}
}

func TestSettlementServiceLatestRootIndex(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// latestRootIndex() returns uint256 3
	resp := make([]byte, 32)
	resp[31] = 3
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	svc, _ := NewSettlementService(adapter, ledger.New(), contract)
	got, err := svc.LatestRootIndex()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got != 3 {
		t.Fatalf("rootIndex = %d, want 3", got)
	}
}

func TestSettlementServiceRootAt(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	contract := "0x00000000000000000000000000000000000000aa"
	// rootAt(1) -> (bytes32 root=0x0b0a0d, uint64 timestamp=42)
	resp := make([]byte, 64)
	resp[29] = 0x0b
	resp[30] = 0x0a
	resp[31] = 0x0d
	resp[63] = 42
	adapter.ContractResponses[common.HexToAddress(contract).Hex()] = resp

	svc, _ := NewSettlementService(adapter, ledger.New(), contract)
	root, ts, err := svc.RootAt(1)
	if err != nil {
		t.Fatalf("rootAt: %v", err)
	}
	if root[29] != 0x0b || root[30] != 0x0a || root[31] != 0x0d {
		t.Fatalf("root = %x, want ...0b0a0d", root)
	}
	if ts != 42 {
		t.Fatalf("timestamp = %d, want 42", ts)
	}
}

func TestSettlementServiceWithdrawBuildsProofCalldata(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, _ := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(100), 1, "w1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx")
	}
	// selector for withdraw(address,uint32,uint256,uint256,uint256,bytes32[])
	data := adapter.Sent[0].Data
	if len(data) < 4 {
		t.Fatalf("no selector")
	}
	// account address must appear in the ABI-encoded args
	if !bytes.Contains(data, acc[:]) {
		t.Fatalf("account not encoded in calldata")
	}
	if new(big.Int).SetBytes(data[4:36]).Sign() == 0 {
		t.Fatalf("account word should be non-zero")
	}
}

// TestSettlementServiceWithdrawEncodesAccountWord strengthens the brief's
// withdraw test: with the brief's degenerate values (account == 0x..01 and
// assetId == 1) the two leading ABI words are byte-identical, so
// bytes.Contains(data, acc[:]) cannot tell "account encoded in slot 0" from
// "assetId encoded in slot 0". Using distinctive account bytes makes the
// assertion non-vacuous: the account word must land exactly in the first slot.
func TestSettlementServiceWithdrawEncodesAccountWord(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[0] = 0x11
	acc[19] = 0x22
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(100), 1, "w1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx")
	}
	data := adapter.Sent[0].Data
	// selector + 6 static args (account, assetId, balance, amount, rootIndex,
	// proof offset) + the empty proof's length word.
	if want := 4 + 6*32 + 32; len(data) != want {
		t.Fatalf("calldata len = %d, want %d", len(data), want)
	}
	wantAccount := make([]byte, 32)
	copy(wantAccount[12:], acc[:])
	if !bytes.Equal(data[4:36], wantAccount) {
		t.Fatalf("account word = %x, want %x", data[4:36], wantAccount)
	}
	wantAsset := make([]byte, 32)
	wantAsset[31] = 1
	if !bytes.Equal(data[36:68], wantAsset) {
		t.Fatalf("assetId word = %x, want %x", data[36:68], wantAsset)
	}
}

// TestSettlementServiceWithdrawGuardBlocksMismatchedBalance verifies the
// load-bearing guard: when the ledger balance differs from the balance being
// published, Withdraw must refuse before any transaction is submitted, with the
// brief's verbatim error message.
func TestSettlementServiceWithdrawGuardBlocksMismatchedBalance(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Ledger says 100 but the caller publishes 99: must be rejected.
	_, err = svc.Withdraw(acc, 1, uint256.NewInt(99), uint256.NewInt(99), 1, "w1")
	if err == nil {
		t.Fatalf("expected balance mismatch error")
	}
	if want := "withdraw: balance mismatch (ledger=100 arg=99)"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if len(adapter.Sent) != 0 {
		t.Fatalf("guard must not submit a tx, got %d", len(adapter.Sent))
	}
	// Guard must also leave the ledger untouched: no debit, no version bump.
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(100)) != 0 {
		t.Fatalf("guard must not debit, balance = %s, want 100", got)
	}
	if v := l.Version(); v != 1 {
		t.Fatalf("guard must not apply an op, version = %d, want 1", v)
	}
}

// --- Task 5: fast withdraw debits the ledger before the on-chain submit ---

func testSettlementABI(t *testing.T) abi.ABI {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(settlementABIJSON))
	if err != nil {
		t.Fatalf("abi: %v", err)
	}
	return parsed
}

// extractWithdrawProof unpacks the bytes32[] proof (6th arg) from withdraw calldata.
func extractWithdrawProof(t *testing.T, data []byte) [][32]byte {
	t.Helper()
	if len(data) < 4 {
		t.Fatalf("calldata too short: %d", len(data))
	}
	method, ok := testSettlementABI(t).Methods["withdraw"]
	if !ok {
		t.Fatalf("withdraw method missing from ABI")
	}
	vals, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("unpack withdraw: %v", err)
	}
	if len(vals) != 6 {
		t.Fatalf("withdraw args = %d, want 6", len(vals))
	}
	proof, ok := vals[5].([][32]byte)
	if !ok {
		t.Fatalf("proof arg type = %T, want [][32]byte", vals[5])
	}
	return proof
}

// testLeafHash mirrors ledger.leafHash (keccak256(account || assetId || balance)).
func testLeafHash(a ledger.AccountID, asset ledger.AssetID, balance *uint256.Int) [32]byte {
	var buf [84]byte
	copy(buf[0:20], a[:])
	var assetBytes [32]byte
	binary.BigEndian.PutUint32(assetBytes[28:], uint32(asset))
	copy(buf[20:52], assetBytes[:])
	b := balance.Bytes32()
	copy(buf[52:84], b[:])
	h := crypto.Keccak256(buf[:])
	var out [32]byte
	copy(out[:], h)
	return out
}

func testHashPair(a, b [32]byte) [32]byte {
	var buf [64]byte
	if bytes.Compare(a[:], b[:]) <= 0 {
		copy(buf[0:32], a[:])
		copy(buf[32:64], b[:])
	} else {
		copy(buf[0:32], b[:])
		copy(buf[32:64], a[:])
	}
	h := crypto.Keccak256(buf[:])
	var out [32]byte
	copy(out[:], h)
	return out
}

func testVerifyProof(proof [][32]byte, root, leaf [32]byte) bool {
	computed := leaf
	for _, p := range proof {
		computed = testHashPair(computed, p)
	}
	return computed == root
}

// TestSettlementServiceWithdrawDebitsLedgerExactlyOnce covers requirement (a):
// a successful withdraw reduces the ledger balance, applies exactly one
// WithdrawOp (version advances by one), and the next version's root no longer
// reflects the withdrawn amount.
func TestSettlementServiceWithdrawDebitsLedgerExactlyOnce(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	rootBefore := l.Root()
	versionBefore := l.Version()

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx, got %d", len(adapter.Sent))
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("balance = %s, want 60", got)
	}
	if v := l.Version(); v != versionBefore+1 {
		t.Fatalf("version = %d, want %d (exactly one WithdrawOp)", v, versionBefore+1)
	}
	rootAfter := l.Root()
	if rootAfter == rootBefore {
		t.Fatalf("root did not change after debit")
	}
	// The old 100-balance leaf must no longer be what the root commits to.
	if oldLeaf := testLeafHash(acc, 1, uint256.NewInt(100)); oldLeaf == rootAfter {
		t.Fatalf("next version's root still contains the withdrawn balance")
	}
	if _, proven, err := l.Proof(acc, 1); err != nil || proven.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("proof balance = %v (err %v), want 60", proven, err)
	}
}

// TestSettlementServiceWithdrawProofTakenBeforeDebit covers requirement (2): the
// proof is captured from the PRE-debit tree. A full withdrawal is used because
// debiting removes the leaf entirely, so an implementation that proved after the
// debit would get ErrNoLeaf and fail; a partial withdraw leaves the sibling path
// unchanged and would not catch the ordering bug. The mock does not verify
// Merkle proofs, so we verify the packed proof against the pre-debit root here.
func TestSettlementServiceWithdrawProofTakenBeforeDebit(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc, sibling ledger.AccountID
	acc[19] = 1
	sibling[19] = 2
	_ = l.Apply(
		ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"},
		ledger.DepositOp{Account: sibling, Asset: 1, Amount: uint256.NewInt(100), Ref: "d2"},
	)

	rootBefore := l.Root()

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Full withdrawal: amount == balance, so the debit removes the leaf.
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(100), 1, "w1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("expected 1 sent tx, got %d", len(adapter.Sent))
	}

	proof := extractWithdrawProof(t, adapter.Sent[0].Data)
	if len(proof) == 0 {
		t.Fatalf("proof is empty; test would be vacuous")
	}
	leaf := testLeafHash(acc, 1, uint256.NewInt(100))
	if !testVerifyProof(proof, rootBefore, leaf) {
		t.Fatalf("packed proof does not verify against the pre-debit root")
	}
	rootAfter := l.Root()
	if rootAfter == rootBefore {
		t.Fatalf("root did not change after a full debit")
	}
	if testVerifyProof(proof, rootAfter, leaf) {
		t.Fatalf("packed proof verifies against the post-debit root; proof was taken too late")
	}
}

// TestSettlementServiceWithdrawSameRefRetryDoesNotResend covers the pinned
// withdrawal contract: replaying the same ref must not debit twice AND must not
// broadcast a second transaction — it converges on the first tx. (This replaces
// the pre-fix expectation that a retry resent a tx, which was the double-pay
// bug this fix eliminates.)
func TestSettlementServiceWithdrawSameRefRetryDoesNotResend(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	firstHash, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1")
	if err != nil {
		t.Fatalf("first withdraw: %v", err)
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("balance after first withdraw = %s, want 60", got)
	}
	versionAfterFirst := l.Version()
	adapter.Receipts[firstHash] = &Receipt{Status: 1}

	// Retry with the SAME ref: no second debit and no second broadcast.
	retryHash, err := svc.Withdraw(acc, 1, uint256.NewInt(60), uint256.NewInt(40), 1, "w1")
	if err != nil {
		t.Fatalf("retry withdraw: %v", err)
	}
	if retryHash != firstHash {
		t.Fatalf("retry hash = %q, want %q", retryHash, firstHash)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("retry must not resend a tx, sent = %d, want 1", len(adapter.Sent))
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("retry must not debit again, balance = %s, want 60", got)
	}
	if v := l.Version(); v != versionAfterFirst {
		t.Fatalf("retry must not apply an op, version = %d, want %d", v, versionAfterFirst)
	}
}

// TestSettlementServiceWithdrawInsufficientBalance covers requirement (c): an
// amount above the proven balance is rejected with ErrInsufficientBalance, no
// tx, and no debit.
func TestSettlementServiceWithdrawInsufficientBalance(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	versionBefore := l.Version()
	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(150), 1, "w1")
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("error = %v, want ErrInsufficientBalance", err)
	}
	if len(adapter.Sent) != 0 {
		t.Fatalf("insufficient balance must not submit a tx, got %d", len(adapter.Sent))
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(100)) != 0 {
		t.Fatalf("insufficient balance must not debit, balance = %s, want 100", got)
	}
	if v := l.Version(); v != versionBefore {
		t.Fatalf("insufficient balance must not apply an op, version = %d, want %d", v, versionBefore)
	}
}

// TestSettlementServiceWithdrawSendFailurePersistsDebit covers requirement (e):
// a SendTransaction failure returns an error but the debit is persisted (here
// proven by recovering the ledger from its WAL), and a same-ref retry does not
// debit a second time.
func TestSettlementServiceWithdrawSendFailurePersistsDebit(t *testing.T) {
	dir := t.TempDir()
	store, err := ledger.NewFileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	l := ledger.New().WithStore(store)
	var acc ledger.AccountID
	acc[19] = 1
	if err := l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"}); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	adapter := NewMockBlockchainAdapter()
	adapter.Error = errors.New("rpc down")

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1"); err == nil {
		t.Fatalf("expected send failure")
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("debit must persist after a failed send, balance = %s, want 60", got)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Rebuild from the WAL to prove the debit (and its idempotency key) survived.
	recovered, err := ledger.Recover(store)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := recovered.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("recovered balance = %s, want 60 (debit not persisted)", got)
	}

	svc2, err := NewSettlementService(adapter, recovered, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := svc2.Withdraw(acc, 1, uint256.NewInt(60), uint256.NewInt(40), 1, "w1"); err == nil {
		t.Fatalf("expected send failure on retry")
	}
	if got := recovered.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("retry with same ref must not debit again, balance = %s, want 60", got)
	}
}

// --- Bugbot finding #1: a not-found receipt must be treated as PENDING ---

// notFoundReceiptAdapter mimics the REAL node: while a tx is in the mempool,
// GetTransactionReceipt returns an error wrapping ethereum.NotFound (exactly
// what ethclient.TransactionReceipt does), NOT (nil, nil). It is a test-local
// adapter so the shared MockBlockchainAdapter keeps its (nil, nil) behaviour for
// every existing test.
type notFoundReceiptAdapter struct {
	*MockBlockchainAdapter
	notFoundPolls int
	mined         *Receipt
}

func (a *notFoundReceiptAdapter) GetTransactionReceipt(string) (*Receipt, error) {
	a.MockBlockchainAdapter.ReceiptCalls++
	if a.notFoundPolls > 0 {
		a.notFoundPolls--
		return nil, fmt.Errorf("eth_getTransactionReceipt: %w", ethereum.NotFound)
	}
	return a.mined, nil
}

// failingReceiptAdapter returns a genuine, non-NotFound error (e.g. the node
// refusing the connection) on every poll.
type failingReceiptAdapter struct {
	*MockBlockchainAdapter
	err error
}

func (a *failingReceiptAdapter) GetTransactionReceipt(string) (*Receipt, error) {
	return nil, a.err
}

// TestSettlementServiceCommitRootToleratesNotFoundReceiptError covers the HIGH
// regression: a real adapter reports a still-pending tx as a NotFound error, so
// CommitRoot must keep polling past those errors and only succeed on the mined
// receipt. Before the fix it aborted on the first poll.
func TestSettlementServiceCommitRootToleratesNotFoundReceiptError(t *testing.T) {
	contract := "0x00000000000000000000000000000000000000aa"
	var root [32]byte
	root[31] = 5

	base := NewMockBlockchainAdapter()
	adapter := &notFoundReceiptAdapter{
		MockBlockchainAdapter: base,
		notFoundPolls:         3,
		mined:                 minedCommitReceipt(contract, 7, root),
	}

	svc, err := NewSettlementService(adapter, ledger.New(), contract)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := svc.Initialize(Config{ReceiptTimeoutSeconds: 2, ReceiptPollMillis: 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	_, rootIndex, err := svc.CommitRoot(root)
	if err != nil {
		t.Fatalf("a NotFound receipt means PENDING and must not abort CommitRoot: %v", err)
	}
	if rootIndex != 7 {
		t.Fatalf("rootIndex = %d, want 7 (from the StateRootCommitted event)", rootIndex)
	}
	if base.ReceiptCalls != 4 {
		t.Fatalf("GetTransactionReceipt calls = %d, want 4 (3 pending + 1 mined)", base.ReceiptCalls)
	}
	if len(base.Sent) != 1 {
		t.Fatalf("expected 1 sent tx, got %d", len(base.Sent))
	}
}

// TestSettlementServiceCommitRootGenuineErrorFailsFast guards the other half:
// only NotFound means pending. A genuine error (connection refused, bad RPC)
// must abort immediately, not wait out the timeout.
func TestSettlementServiceCommitRootGenuineErrorFailsFast(t *testing.T) {
	contract := "0x00000000000000000000000000000000000000aa"
	var root [32]byte
	wantErr := errors.New("connection refused")

	adapter := &failingReceiptAdapter{MockBlockchainAdapter: NewMockBlockchainAdapter(), err: wantErr}

	svc, err := NewSettlementService(adapter, ledger.New(), contract)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Deliberately long: a correct fail-fast path returns at once, while one
	// that (wrongly) treats any error as pending would block for the timeout.
	if err := svc.Initialize(Config{ReceiptTimeoutSeconds: 30, ReceiptPollMillis: 1000}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	start := time.Now()
	_, _, err = svc.CommitRoot(root)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("CommitRoot did not fail fast on a genuine error (took %s)", elapsed)
	}
}

// --- Bugbot finding: Withdraw must pin the first attempt per ref (double-pay) ---

// extractWithdrawRootIndex unpacks the rootIndex (5th arg) from withdraw calldata.
func extractWithdrawRootIndex(t *testing.T, data []byte) uint64 {
	t.Helper()
	if len(data) < 4 {
		t.Fatalf("calldata too short: %d", len(data))
	}
	method, ok := testSettlementABI(t).Methods["withdraw"]
	if !ok {
		t.Fatalf("withdraw method missing from ABI")
	}
	vals, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("unpack withdraw: %v", err)
	}
	if len(vals) != 6 {
		t.Fatalf("withdraw args = %d, want 6", len(vals))
	}
	n, ok := vals[4].(*big.Int)
	if !ok {
		t.Fatalf("rootIndex arg type = %T, want *big.Int", vals[4])
	}
	return n.Uint64()
}

// TestSettlementServiceWithdrawRetryAfterSuccessDoesNotRebroadcast covers the
// HIGH double-pay finding: once a ref has been broadcast, a retry with the same
// ref must converge on the original transaction even when the caller passes a
// NEWER rootIndex and different balance/amount. Before the fix the retry built a
// fresh tx (a second Sent entry) using the caller's newer root, which the chain
// can accept and pay a second time.
func TestSettlementServiceWithdrawRetryAfterSuccessDoesNotRebroadcast(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	firstHash, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1")
	if err != nil {
		t.Fatalf("first withdraw: %v", err)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("first withdraw must broadcast 1 tx, got %d", len(adapter.Sent))
	}
	// The retry polls the already-broadcast tx's receipt.
	adapter.Receipts[firstHash] = &Receipt{Status: 1}

	// Retry with a NEWER rootIndex and different balance/amount: none of it may
	// reach the wire.
	retryHash, err := svc.Withdraw(acc, 1, uint256.NewInt(60), uint256.NewInt(10), 7, "w1")
	if err != nil {
		t.Fatalf("retry withdraw: %v", err)
	}
	if retryHash != firstHash {
		t.Fatalf("retry hash = %q, want the original %q", retryHash, firstHash)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("retry must not rebroadcast, sent = %d, want 1", len(adapter.Sent))
	}
	if got := extractWithdrawRootIndex(t, adapter.Sent[0].Data); got != 1 {
		t.Fatalf("broadcast rootIndex = %d, want the original 1", got)
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("retry must not debit again, balance = %s, want 60", got)
	}
}

// TestSettlementServiceWithdrawRetryAfterFailedSubmitReusesPinnedParams covers
// the other half of the finding: when the first submit fails, the retry must
// re-send the PINNED calldata (original rootIndex + proof), not the caller's
// fresh arguments. Before the fix the retry re-encoded the newer rootIndex,
// which is exactly the double-pay vector once the first tx is mined.
func TestSettlementServiceWithdrawRetryAfterFailedSubmitReusesPinnedParams(t *testing.T) {
	adapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Fail the first submit only; a later submit succeeds.
	attempts := 0
	adapter.SendHandler = func(tx *Transaction) (string, error) {
		attempts++
		if attempts == 1 {
			return "", errors.New("rpc down")
		}
		return adapter.SendTxHash, nil
	}

	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1"); err == nil {
		t.Fatalf("expected the first submit to fail")
	}
	if len(adapter.Sent) != 0 {
		t.Fatalf("a failed submit must not be recorded, sent = %d", len(adapter.Sent))
	}

	// Retry the same ref with a NEWER rootIndex. The retry must re-send the
	// pinned record's calldata, so the newer rootIndex and the caller's balance
	// never reach the wire.
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(60), uint256.NewInt(40), 2, "w1"); err != nil {
		t.Fatalf("retry withdraw: %v", err)
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("balance = %s, want 60 (debited exactly once)", got)
	}
	if v := l.Version(); v != 2 {
		t.Fatalf("version = %d, want 2 (a single WithdrawOp)", v)
	}
	if len(adapter.Sent) != 1 {
		t.Fatalf("a tx must be sent on retry, sent = %d, want 1", len(adapter.Sent))
	}
	if got := extractWithdrawRootIndex(t, adapter.Sent[0].Data); got != 1 {
		t.Fatalf("retry broadcast rootIndex = %d, want the pinned original 1 (not the newer 2)", got)
	}
}

// TestSettlementServiceWithdrawDuplicateRefWithoutRecordIsRefused is the core
// double-pay regression: after a restart the in-memory withdrawal record is gone
// while the ledger still durably remembers the applied ref, so Withdraw must
// refuse with ErrWithdrawRecoveryRequired instead of re-broadcasting the
// caller's fresh arguments with a newer rootIndex.
func TestSettlementServiceWithdrawDuplicateRefWithoutRecordIsRefused(t *testing.T) {
	firstAdapter := NewMockBlockchainAdapter()
	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	// Consume the ref through one service: the debit lands in the ledger and the
	// tx is broadcast, but the pinned record lives only in this service.
	svc, err := NewSettlementService(firstAdapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1"); err != nil {
		t.Fatalf("first withdraw: %v", err)
	}

	// A fresh service over the SAME ledger models a process restart: the ledger
	// applied-key set survives, the in-memory map does not.
	adapter := NewMockBlockchainAdapter()
	restarted, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = restarted.Withdraw(acc, 1, uint256.NewInt(60), uint256.NewInt(40), 7, "w1")
	if !errors.Is(err, ErrWithdrawRecoveryRequired) {
		t.Fatalf("error = %v, want ErrWithdrawRecoveryRequired", err)
	}
	if len(adapter.Sent) != 0 {
		t.Fatalf("a recovery-required ref must not be broadcast, sent = %d, want 0", len(adapter.Sent))
	}
}

// concurrentReceiptAdapter serialises GetTransactionReceipt so the mock's
// non-atomic ReceiptCalls counter stays race-free while many goroutines converge
// on the same broadcast. It exists only for the concurrency test.
type concurrentReceiptAdapter struct {
	*MockBlockchainAdapter
	mu sync.Mutex
}

func (a *concurrentReceiptAdapter) GetTransactionReceipt(hash string) (*Receipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.MockBlockchainAdapter.GetTransactionReceipt(hash)
}

// TestSettlementServiceWithdrawConcurrentSameRefSendsOnce runs several
// concurrent Withdraw calls with the same ref against one service: exactly one
// transaction may be broadcast, every caller must observe the same hash, and the
// ledger must be debited exactly once.
func TestSettlementServiceWithdrawConcurrentSameRefSendsOnce(t *testing.T) {
	base := NewMockBlockchainAdapter()
	base.Receipts[base.SendTxHash] = &Receipt{Status: 1}
	adapter := &concurrentReceiptAdapter{MockBlockchainAdapter: base}

	l := ledger.New()
	var acc ledger.AccountID
	acc[19] = 1
	_ = l.Apply(ledger.DepositOp{Account: acc, Asset: 1, Amount: uint256.NewInt(100), Ref: "d1"})

	svc, err := NewSettlementService(adapter, l, "0x00000000000000000000000000000000000000aa")
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	const n = 8
	hashes := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			hashes[i], errs[i] = svc.Withdraw(acc, 1, uint256.NewInt(100), uint256.NewInt(40), 1, "w1")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if len(base.Sent) != 1 {
		t.Fatalf("exactly one tx must be broadcast, got %d", len(base.Sent))
	}
	for i, h := range hashes {
		if h != hashes[0] {
			t.Fatalf("goroutine %d hash = %q, want %q", i, h, hashes[0])
		}
	}
	if got := l.Balance(acc, 1); got.Cmp(uint256.NewInt(60)) != 0 {
		t.Fatalf("balance = %s, want 60 (a single debit)", got)
	}
}

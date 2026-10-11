package services

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

const settlementABIJSON = `[
  {"type":"function","name":"commitStateRoot","stateMutability":"nonpayable","inputs":[{"name":"root","type":"bytes32"}],"outputs":[{"name":"rootIndex","type":"uint256"}]},
  {"type":"function","name":"withdraw","stateMutability":"nonpayable","inputs":[
     {"name":"account","type":"address"},
     {"name":"assetId","type":"uint32"},
     {"name":"balance","type":"uint256"},
     {"name":"amount","type":"uint256"},
     {"name":"rootIndex","type":"uint256"},
     {"name":"proof","type":"bytes32[]"}],"outputs":[]},
  {"type":"function","name":"latestRootIndex","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"rootAt","stateMutability":"view","inputs":[{"name":"rootIndex","type":"uint256"}],"outputs":[{"name":"root","type":"bytes32"},{"name":"timestamp","type":"uint64"}]},
  {"type":"event","name":"StateRootCommitted","inputs":[
     {"name":"rootIndex","type":"uint256","indexed":true},
     {"name":"root","type":"bytes32","indexed":false},
     {"name":"timestamp","type":"uint64","indexed":false}]}
]`

// defaultReceiptTimeout / defaultReceiptPoll bound how long CommitRoot waits
// for a mined receipt. They are overridable via services.Config.
const (
	defaultReceiptTimeout = 60 * time.Second
	defaultReceiptPoll    = time.Second
)

// SettlementService encodes and submits Settlement contract calls.
type SettlementService struct {
	adapter  BlockchainAdapter
	abi      abi.ABI
	contract common.Address
	ledger   *ledger.Ledger

	receiptTimeout time.Duration
	receiptPoll    time.Duration
}

func NewSettlementService(adapter BlockchainAdapter, l *ledger.Ledger, contract string) (*SettlementService, error) {
	parsed, err := abi.JSON(strings.NewReader(settlementABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse settlement abi: %w", err)
	}
	return &SettlementService{
		adapter:        adapter,
		abi:            parsed,
		contract:       common.HexToAddress(contract),
		ledger:         l,
		receiptTimeout: defaultReceiptTimeout,
		receiptPoll:    defaultReceiptPoll,
	}, nil
}

func (s *SettlementService) Name() string { return "SettlementService" }

func (s *SettlementService) Initialize(cfg Config) error {
	if cfg.ReceiptTimeoutSeconds > 0 {
		s.receiptTimeout = time.Duration(cfg.ReceiptTimeoutSeconds) * time.Second
	}
	if cfg.ReceiptPollMillis > 0 {
		s.receiptPoll = time.Duration(cfg.ReceiptPollMillis) * time.Millisecond
	}
	return nil
}

func (s *SettlementService) Start() error { return nil }
func (s *SettlementService) Stop() error  { return nil }

// CommitRoot submits commitStateRoot(root), waits for the transaction to be
// mined, and returns the on-chain rootIndex decoded from the StateRootCommitted
// event emitted by commitStateRoot.
//
// It deliberately does NOT read latestRootIndex() after submitting. Against a
// real node that view still reports the pre-commit index while the tx is only
// in the mempool, so recording it produced a version -> rootIndex mapping that
// was off by one; it also could not tell an `insolvent` revert from a success.
// The mined event is the only unambiguous source of the index, and decoding it
// proves the commit actually landed.
func (s *SettlementService) CommitRoot(root [32]byte) (txHash string, rootIndex uint64, err error) {
	data, err := s.abi.Pack("commitStateRoot", root)
	if err != nil {
		return "", 0, err
	}
	txHash, err = s.adapter.SendTransaction(&Transaction{
		To:       s.contract.Hex(),
		Data:     data,
		GasLimit: 500_000,
	})
	if err != nil {
		return "", 0, err
	}

	receipt, err := s.waitForReceipt(txHash)
	if err != nil {
		return "", 0, err
	}
	if receipt.Status != 1 {
		return "", 0, fmt.Errorf("commitStateRoot: tx %s reverted (status %d)", txHash, receipt.Status)
	}

	rootIndex, err = s.decodeStateRootCommitted(receipt, root)
	if err != nil {
		return "", 0, err
	}
	return txHash, rootIndex, nil
}

// waitForReceipt polls until the tx is mined or the configured timeout elapses.
//
// A still-pending transaction is signalled in two ways, and BOTH must keep
// polling rather than fail:
//   - (nil, nil): the BlockchainAdapter contract's pending signal (what
//     MockBlockchainAdapter returns);
//   - an error wrapping ethereum.NotFound: what ethclient.TransactionReceipt
//     returns from a real node while the tx is still in the mempool.
//
// The second case is defence in depth: the EVMAdapter already normalises
// NotFound to (nil, nil), but tolerating the raw error here means any future
// adapter that returns it as-is still works. Any other error is a genuine
// failure (connection refused, bad RPC, ...) and aborts the wait immediately.
func (s *SettlementService) waitForReceipt(txHash string) (*Receipt, error) {
	deadline := time.Now().Add(s.receiptTimeout)
	poll := s.receiptPoll
	if poll <= 0 {
		poll = defaultReceiptPoll
	}
	for {
		receipt, err := s.adapter.GetTransactionReceipt(txHash)
		if err != nil {
			if !errors.Is(err, ethereum.NotFound) {
				return nil, err
			}
			// NotFound means still pending: fall through to the poll/backoff.
			receipt = nil
		}
		if receipt != nil {
			return receipt, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("commitStateRoot: timed out after %s waiting for receipt of %s", s.receiptTimeout, txHash)
		}
		if poll < remaining {
			time.Sleep(poll)
		} else {
			time.Sleep(remaining)
		}
	}
}

// decodeStateRootCommitted finds the StateRootCommitted event the settlement
// contract emitted for this commit, verifies the decoded root matches the root
// we submitted, and returns its indexed rootIndex (Topics[1]).
func (s *SettlementService) decodeStateRootCommitted(receipt *Receipt, want [32]byte) (uint64, error) {
	event, ok := s.abi.Events["StateRootCommitted"]
	if !ok {
		return 0, errors.New("commitStateRoot: StateRootCommitted event missing from ABI")
	}
	for _, lg := range receipt.Logs {
		if lg == nil {
			continue
		}
		// Only accept the event emitted by our settlement contract.
		if common.HexToAddress(lg.Address) != s.contract {
			continue
		}
		if len(lg.Topics) < 2 || common.HexToHash(lg.Topics[0]) != event.ID {
			continue
		}
		// rootIndex is the single indexed argument, so it arrives in Topics[1].
		rootIndex := new(big.Int).SetBytes(common.HexToHash(lg.Topics[1]).Bytes()).Uint64()

		vals, err := event.Inputs.NonIndexed().Unpack(lg.Data)
		if err != nil {
			return 0, fmt.Errorf("commitStateRoot: decode StateRootCommitted: %w", err)
		}
		if len(vals) != 2 {
			return 0, fmt.Errorf("commitStateRoot: StateRootCommitted: got %d non-indexed args, want 2", len(vals))
		}
		gotRoot, ok := vals[0].([32]byte)
		if !ok {
			return 0, errors.New("commitStateRoot: StateRootCommitted: bad root type")
		}
		if gotRoot != want {
			return 0, fmt.Errorf("commitStateRoot: event root %x != submitted root %x", gotRoot, want)
		}
		return rootIndex, nil
	}
	return 0, fmt.Errorf("commitStateRoot: receipt for tx %s has no StateRootCommitted event", receipt.TxHash)
}

// LatestRootIndex reads latestRootIndex() from the contract.
func (s *SettlementService) LatestRootIndex() (uint64, error) {
	data, err := s.abi.Pack("latestRootIndex")
	if err != nil {
		return 0, err
	}
	out, err := s.adapter.CallContract(s.contract.Hex(), data, 0)
	if err != nil {
		return 0, err
	}
	vals, err := s.abi.Unpack("latestRootIndex", out)
	if err != nil {
		return 0, err
	}
	if len(vals) != 1 {
		return 0, errors.New("latestRootIndex: unexpected output")
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return 0, errors.New("latestRootIndex: bad type")
	}
	return n.Uint64(), nil
}

// RootAt reads the root and timestamp committed at a given index.
func (s *SettlementService) RootAt(rootIndex uint64) ([32]byte, uint64, error) {
	data, err := s.abi.Pack("rootAt", new(big.Int).SetUint64(rootIndex))
	if err != nil {
		return [32]byte{}, 0, err
	}
	out, err := s.adapter.CallContract(s.contract.Hex(), data, 0)
	if err != nil {
		return [32]byte{}, 0, err
	}
	vals, err := s.abi.Unpack("rootAt", out)
	if err != nil {
		return [32]byte{}, 0, err
	}
	if len(vals) != 2 {
		return [32]byte{}, 0, errors.New("rootAt: unexpected output")
	}
	root, ok := vals[0].([32]byte)
	if !ok {
		return [32]byte{}, 0, errors.New("rootAt: bad root type")
	}
	ts, ok := vals[1].(uint64)
	if !ok {
		return [32]byte{}, 0, errors.New("rootAt: bad timestamp type")
	}
	return root, ts, nil
}

// Withdraw submits a fast withdrawal using the ledger's Merkle proof.
//
// The ledger debit happens BEFORE the on-chain submit, and the debit is
// idempotent by the caller-supplied ref. A failed submit is therefore retried
// by reusing the SAME ref rather than being rolled back and re-submitted (which
// could double-spend).
func (s *SettlementService) Withdraw(account ledger.AccountID, asset ledger.AssetID, balance, amount *uint256.Int, rootIndex uint64, ref string) (string, error) {
	// (a) Capture the proof BEFORE debiting. Debiting advances the ledger
	// version, so a proof taken after the debit would commit to a different
	// root than the rootIndex the contract verifies against. The mock adapter
	// does not check Merkle proofs, so this ordering is load-bearing and is
	// asserted by TestSettlementServiceWithdrawProofTakenBeforeDebit.
	proof, proven, err := s.ledger.Proof(account, asset)
	if err != nil {
		return "", err
	}
	// Guard: never sign a proof for a balance we did not intend to publish.
	if proven.Cmp(balance) != 0 {
		return "", fmt.Errorf("withdraw: balance mismatch (ledger=%s arg=%s)", proven, balance)
	}
	// Guard: the amount must be positive and must not exceed the proven
	// (published) balance. A short amount is rejected before any debit or tx.
	if amount.Sign() <= 0 {
		return "", fmt.Errorf("withdraw: amount must be positive (got %s)", amount)
	}
	if amount.Cmp(balance) > 0 {
		return "", ledger.ErrInsufficientBalance
	}

	// (b) Debit the ledger before submitting on-chain. WithdrawOp is idempotent
	// by ref: replaying the same ref is a no-op, so a retry after a failed
	// submit never debits twice.
	if err := s.ledger.Apply(ledger.WithdrawOp{Account: account, Asset: asset, Amount: amount, Ref: ref}); err != nil {
		return "", err
	}

	proofHashes := make([][32]byte, len(proof))
	copy(proofHashes, proof)

	data, err := s.abi.Pack(
		"withdraw",
		common.BytesToAddress(account[:]),
		uint32(asset),
		balance.ToBig(),
		amount.ToBig(),
		new(big.Int).SetUint64(rootIndex),
		proofHashes,
	)
	if err != nil {
		return "", err
	}
	// (c) Submit using the proof captured in (a) and the caller's rootIndex.
	return s.adapter.SendTransaction(&Transaction{
		To:       s.contract.Hex(),
		Data:     data,
		GasLimit: 500_000,
	})
}

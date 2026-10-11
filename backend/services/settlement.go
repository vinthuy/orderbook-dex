package services

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

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
  {"type":"function","name":"latestRootIndex","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]}
]`

// SettlementService encodes and submits Settlement contract calls.
type SettlementService struct {
	adapter  BlockchainAdapter
	abi      abi.ABI
	contract common.Address
	ledger   *ledger.Ledger
}

func NewSettlementService(adapter BlockchainAdapter, l *ledger.Ledger, contract string) (*SettlementService, error) {
	parsed, err := abi.JSON(strings.NewReader(settlementABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse settlement abi: %w", err)
	}
	return &SettlementService{
		adapter:  adapter,
		abi:      parsed,
		contract: common.HexToAddress(contract),
		ledger:   l,
	}, nil
}

func (s *SettlementService) Name() string                { return "SettlementService" }
func (s *SettlementService) Initialize(cfg Config) error { return nil }
func (s *SettlementService) Start() error                { return nil }
func (s *SettlementService) Stop() error                 { return nil }

// CommitRoot submits commitStateRoot(root) and returns the tx hash.
func (s *SettlementService) CommitRoot(root [32]byte) (string, error) {
	data, err := s.abi.Pack("commitStateRoot", root)
	if err != nil {
		return "", err
	}
	return s.adapter.SendTransaction(&Transaction{
		To:       s.contract.Hex(),
		Data:     data,
		GasLimit: 500_000,
	})
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

// Withdraw submits a fast withdrawal using the ledger's Merkle proof.
func (s *SettlementService) Withdraw(account ledger.AccountID, asset ledger.AssetID, balance, amount *uint256.Int, rootIndex uint64) (string, error) {
	proof, proven, err := s.ledger.Proof(account, asset)
	if err != nil {
		return "", err
	}
	// Guard: never sign a proof for a balance we did not intend to publish.
	if proven.Cmp(balance) != 0 {
		return "", fmt.Errorf("withdraw: balance mismatch (ledger=%s arg=%s)", proven, balance)
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
	return s.adapter.SendTransaction(&Transaction{
		To:       s.contract.Hex(),
		Data:     data,
		GasLimit: 500_000,
	})
}

package services

import (
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

var depositTopic = crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)"))

// DepositIndexer credits on-chain Deposit events into the off-chain ledger.
type DepositIndexer struct {
	adapter       BlockchainAdapter
	ledger        *ledger.Ledger
	contract      string
	confirmations uint64

	mu        sync.Mutex
	nextBlock uint64
	interval  time.Duration
	stop      chan struct{}
	started   bool
}

func NewDepositIndexer(adapter BlockchainAdapter, l *ledger.Ledger, contract string, confirmations uint64) *DepositIndexer {
	if confirmations == 0 {
		confirmations = 1
	}
	return &DepositIndexer{
		adapter:       adapter,
		ledger:        l,
		contract:      contract,
		confirmations: confirmations,
		nextBlock:     1,
		interval:      5 * time.Second,
		stop:          make(chan struct{}),
	}
}

func (d *DepositIndexer) Name() string { return "DepositIndexer" }

func (d *DepositIndexer) Initialize(cfg Config) error {
	if cfg.RequiredConfirmations > 0 {
		d.confirmations = cfg.RequiredConfirmations
	}
	if cfg.SettlementAddress != "" {
		d.contract = cfg.SettlementAddress
	}
	if cfg.AnchorInterval > 0 {
		d.interval = time.Duration(cfg.AnchorInterval) * time.Second
	}
	return nil
}

func (d *DepositIndexer) Start() error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = true
	d.mu.Unlock()

	go func() {
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-ticker.C:
				_ = d.PollOnce()
			}
		}
	}()
	return nil
}

func (d *DepositIndexer) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		close(d.stop)
		d.started = false
	}
	return nil
}

// PollOnce fetches and applies all confirmed Deposit logs once.
func (d *DepositIndexer) PollOnce() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	latest, err := d.adapter.GetBlockNumber()
	if err != nil {
		return err
	}
	if latest < d.confirmations {
		return nil
	}
	to := latest - d.confirmations
	if to < d.nextBlock {
		return nil
	}

	logs, err := d.adapter.GetLogs(d.nextBlock, to, d.contract, [][]string{{depositTopic.Hex()}})
	if err != nil {
		return err
	}
	for _, l := range logs {
		op, err := decodeDeposit(l)
		if err != nil {
			return err
		}
		if err := d.ledger.Apply(op); err != nil {
			return err
		}
	}
	d.nextBlock = to + 1
	return nil
}

func decodeDeposit(l Log) (ledger.DepositOp, error) {
	if len(l.Topics) < 3 {
		return ledger.DepositOp{}, errors.New("deposit log: missing topics")
	}
	if len(l.Data) < 64 {
		return ledger.DepositOp{}, errors.New("deposit log: short data")
	}
	var account ledger.AccountID
	copy(account[:], common.HexToHash(l.Topics[1]).Bytes()[12:32])

	assetID := uint32(new(big.Int).SetBytes(common.HexToHash(l.Topics[2]).Bytes()).Uint64())
	amount, overflow := uint256.FromBig(new(big.Int).SetBytes(l.Data[0:32]))
	if overflow {
		return ledger.DepositOp{}, errors.New("deposit log: amount overflow")
	}

	ref := l.TxHash + ":" + big.NewInt(int64(l.LogIndex)).String()
	return ledger.DepositOp{Account: account, Asset: ledger.AssetID(assetID), Amount: amount, Ref: ref}, nil
}

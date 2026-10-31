package services

import (
	"errors"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"web3-amm-dex-backend/ledger"
)

var depositTopic = crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)"))

// runner owns the polling-goroutine lifecycle shared by DepositIndexer and
// StateAnchorService so their restart semantics cannot drift.
//
// A runner may be started, stopped, and started again any number of times.
// start creates a fresh stop channel under the mutex, so a restarted goroutine
// never observes a channel that a previous Stop already closed. stop closes the
// current channel exactly once and is safe to call before the first start or
// twice in a row.
type runner struct {
	mu       sync.Mutex
	stopCh   chan struct{}
	started  bool
	loops    int
	interval time.Duration
}

// setInterval records the cadence the next start should use. It does not
// restart a running loop: cadence changes take effect on the next Start.
func (r *runner) setInterval(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interval = d
}

// start launches poll on a periodic ticker and returns true, or does nothing
// and returns false when already running (a repeated Start is an idempotent
// no-op that must not spawn a second goroutine).
func (r *runner) start(interval time.Duration, poll func()) bool {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return false
	}
	r.stopCh = make(chan struct{})
	stop := r.stopCh
	r.started = true
	r.loops++
	r.mu.Unlock()

	go func() {
		defer r.loopDone()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
	return true
}

// stop signals the running loop to exit. It is idempotent and safe when the
// runner was never started.
func (r *runner) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return
	}
	close(r.stopCh)
	r.started = false
}

func (r *runner) loopDone() {
	r.mu.Lock()
	r.loops--
	r.mu.Unlock()
}

// activeLoops reports how many loop goroutines are currently alive. Tests use
// it to prove a repeated Start does not spawn a second goroutine.
func (r *runner) activeLoops() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loops
}

// DepositIndexer credits on-chain Deposit events into the off-chain ledger.
type DepositIndexer struct {
	adapter       BlockchainAdapter
	ledger        *ledger.Ledger
	contract      string
	confirmations uint64

	mu        sync.Mutex
	nextBlock uint64
	interval  time.Duration
	run       runner
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
	interval := d.interval
	d.mu.Unlock()

	d.run.start(interval, func() {
		if err := d.PollOnce(); err != nil {
			log.Printf("deposit indexer: poll: %v", err)
		}
	})
	return nil
}

func (d *DepositIndexer) Stop() error {
	d.run.stop()
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

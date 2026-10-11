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

package services

import (
	"log"
	"sync"
	"time"

	"web3-amm-dex-backend/ledger"
)

// RootRecord links a committed on-chain root to the ledger version it represents.
type RootRecord struct {
	Root      [32]byte
	Version   uint64
	RootIndex uint64
}

// StateAnchorService periodically commits the ledger's Merkle root on-chain and
// records the version -> rootIndex mapping used to align the indexer and the
// exit flow.
type StateAnchorService struct {
	settlement *SettlementService
	ledger     *ledger.Ledger

	mu      sync.Mutex
	records []RootRecord
	byVer   map[uint64]uint64 // version -> rootIndex

	interval time.Duration
	stop     chan struct{}
	started  bool
}

func NewStateAnchorService(settlement *SettlementService, l *ledger.Ledger) *StateAnchorService {
	return &StateAnchorService{
		settlement: settlement,
		ledger:     l,
		byVer:      make(map[uint64]uint64),
		interval:   30 * time.Second,
		stop:       make(chan struct{}),
	}
}

func (s *StateAnchorService) Name() string { return "StateAnchorService" }

func (s *StateAnchorService) Initialize(cfg Config) error {
	if cfg.AnchorInterval > 0 {
		s.interval = time.Duration(cfg.AnchorInterval) * time.Second
	}
	return nil
}

func (s *StateAnchorService) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				if _, err := s.AnchorOnce(); err != nil {
					log.Printf("state anchor: %v", err)
				}
			}
		}
	}()
	return nil
}

func (s *StateAnchorService) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		close(s.stop)
		s.started = false
	}
	return nil
}

// AnchorOnce commits the current ledger root and records the mapping. It relies
// on the on-chain latestRootIndex (read after commit) so a restart never assumes
// local state.
func (s *StateAnchorService) AnchorOnce() (*RootRecord, error) {
	root := s.ledger.Root()
	version := s.ledger.Version()

	if _, err := s.settlement.CommitRoot(root); err != nil {
		return nil, err
	}
	rootIndex, err := s.settlement.LatestRootIndex()
	if err != nil {
		return nil, err
	}

	rec := RootRecord{Root: root, Version: version, RootIndex: rootIndex}
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.byVer[version] = rootIndex
	s.mu.Unlock()
	return &rec, nil
}

// RootIndexForVersion returns the on-chain root index committed for a ledger
// version, and whether that version has been anchored.
func (s *StateAnchorService) RootIndexForVersion(version uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byVer[version]
	return idx, ok
}

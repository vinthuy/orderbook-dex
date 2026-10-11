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
	run      runner
}

func NewStateAnchorService(settlement *SettlementService, l *ledger.Ledger) *StateAnchorService {
	return &StateAnchorService{
		settlement: settlement,
		ledger:     l,
		byVer:      make(map[uint64]uint64),
		interval:   30 * time.Second,
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
	interval := s.interval
	s.mu.Unlock()

	s.run.start(interval, func() {
		if _, err := s.AnchorOnce(); err != nil {
			log.Printf("state anchor: %v", err)
		}
	})
	return nil
}

func (s *StateAnchorService) Stop() error {
	s.run.stop()
	return nil
}

// AnchorOnce commits the current ledger root and records the mapping. The
// rootIndex comes back from CommitRoot (decoded from the mined
// StateRootCommitted event), so a commit whose view still lags — or that
// reverted — can never be recorded as if it had landed.
func (s *StateAnchorService) AnchorOnce() (*RootRecord, error) {
	// Root and version are read under ONE lock: separate Root()/Version()
	// calls could interleave with a concurrent Apply and record a
	// (root, version) pair that never existed together.
	root, version := s.ledger.RootAndVersion()

	_, rootIndex, err := s.settlement.CommitRoot(root)
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

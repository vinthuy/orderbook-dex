package services

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/holiman/uint256"

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
	// history retains the leaf set for each anchored version, so a proof can be
	// produced for a root that is no longer current. Without it a user cannot
	// withdraw against an older root and the exit path depends on the operator
	// anchoring again at exactly the right moment.
	history *ledger.RootHistory

	mu      sync.Mutex
	// anchorMu serialises AnchorOnce so two concurrent anchors cannot commit roots
	// in an order unrelated to the versions they snapshot.
	anchorMu sync.Mutex
	records []RootRecord
	byVer   map[uint64]uint64 // version -> rootIndex

	interval time.Duration
	run      runner
}

func NewStateAnchorService(settlement *SettlementService, l *ledger.Ledger) *StateAnchorService {
	return &StateAnchorService{
		settlement: settlement,
		ledger:     l,
		history:    ledger.NewRootHistory(64),
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
	// AnchorOnce must not run concurrently with itself: two anchors interleaving
	// would commit roots in an order unrelated to the versions they snapshot, and
	// byVer would then map a version to the wrong on-chain index.
	s.anchorMu.Lock()
	defer s.anchorMu.Unlock()

	// Build the snapshot and take the root from the SAME read. Reading the root
	// first and snapshotting later would let an Apply in between (a fill, a funding
	// round, a deposit) advance the version, so the record would describe a
	// different state than the root that gets committed — leaving the anchored
	// version provable-only-in-theory.
	rec, version, root := s.history.RecordWithin(s.ledger, time.Now().Unix())

	_, rootIndex, err := s.settlement.CommitRoot(root)
	if err != nil {
		// Nothing was committed, so no record is stored: a record for an unanchored
		// version would be a proof the contract cannot verify.
		return nil, err
	}

	// The committed root and the snapshot root are the same value by construction;
	// assert rather than assume, because a mismatch here means a proof would fail
	// on-chain and the caller has no way to discover that.
	if rec.Root != root {
		return nil, fmt.Errorf("state anchor: snapshot root %x != committed root %x", rec.Root, root)
	}
	rec.RootIndex = rootIndex
	s.history.Add(rec)
	out := RootRecord{Root: root, Version: version, RootIndex: rootIndex}
	s.mu.Lock()
	s.records = append(s.records, out)
	s.byVer[version] = rootIndex
	s.mu.Unlock()
	return &out, nil
}

// RootIndexForVersion returns the on-chain root index committed for a ledger
// version, and whether that version has been anchored.
func (s *StateAnchorService) RootIndexForVersion(version uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byVer[version]
	return idx, ok
}

// ProofAt builds the withdrawal proof a user can present to Settlement.withdraw
// for a specific anchored version.
//
// This is the self-custody guarantee made concrete: given an anchored root the
// user holds the (balance, proof, rootIndex) triple needed to call withdraw
// on-chain, without trusting the operator to produce it at the right moment.
// Without a retained root the exit path silently depended on the operator
// anchoring again before the user asked.
func (s *StateAnchorService) ProofAt(a ledger.AccountID, asset ledger.AssetID, version uint64) (*WithdrawalProof, error) {
	// Walk back from the requested version until a record exists that was ALSO
	// committed. A snapshot alone is not enough: a proof against a root the contract
	// does not know verifies locally and reverts on-chain, wasting the user's gas.
	rec, ok := s.history.LatestAtOrBefore(version)
	for ok {
		if rootIndex, anchored := s.RootIndexForVersion(rec.Version); anchored {
			bal, proof, err := rec.Proof(a, asset)
			if err != nil {
				return nil, err
			}
			return &WithdrawalProof{
				Account: a, Asset: asset, Balance: bal, RootIndex: rootIndex,
				Root: rec.Root, Version: rec.Version, Proof: proof,
			}, nil
		}
		if rec.Version == 0 {
			break
		}
		rec, ok = s.history.LatestAtOrBefore(rec.Version - 1)
	}
	return nil, ledger.ErrNoRootAtVersion
}

// AnchoredVersions lists the retained anchored versions, newest first.
func (s *StateAnchorService) AnchoredVersions() []uint64 {
	recs := s.history.All()
	out := make([]uint64, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		out = append(out, recs[i].Version)
	}
	return out
}

// WithdrawalProof is everything Settlement.withdraw needs plus the root it was
// built against, so a client can re-verify locally before spending gas.
type WithdrawalProof struct {
	Account   ledger.AccountID
	Asset     ledger.AssetID
	Balance   *uint256.Int
	RootIndex uint64
	Root      [32]byte
	Version   uint64
	Proof     [][32]byte
}


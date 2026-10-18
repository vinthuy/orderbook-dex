package indexer

import (
	"encoding/binary"
	"errors"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

// On-chain event topic hashes (keccak256 of the ABI signature).
var (
	depositTopic  = crypto.Keccak256Hash([]byte("Deposit(address,uint32,uint256,uint256)"))
	withdrawTopic = crypto.Keccak256Hash([]byte("Withdrawal(address,uint32,uint256,uint256)"))
	exitTopic     = crypto.Keccak256Hash([]byte("ExitFinalized(address,uint32,uint256,uint256)"))
	rootTopic     = crypto.Keccak256Hash([]byte("StateRootCommitted(uint256,bytes32,uint64)"))
)

// IndexerService persists trades (from the EventBus) and on-chain funding/root
// events (from the BlockchainAdapter) into the Store. It is a services.Service.
type IndexerService struct {
	store         *Store
	bus           *services.EventBus
	adapter       services.BlockchainAdapter
	chain         string
	confirmations uint64

	mu        sync.Mutex
	nextBlock uint64
	interval  time.Duration
	stop      chan struct{}
	started   bool

	degradedMu  sync.Mutex
	degradedErr error
}

func New(store *Store, bus *services.EventBus, adapter services.BlockchainAdapter, chain string, confirmations uint64) *IndexerService {
	if confirmations == 0 {
		confirmations = 1
	}
	return &IndexerService{
		store:         store,
		bus:           bus,
		adapter:       adapter,
		chain:         chain,
		confirmations: confirmations,
		nextBlock:     1,
		interval:      5 * time.Second,
		stop:          make(chan struct{}),
	}
}

func (s *IndexerService) Name() string { return "IndexerService" }

func (s *IndexerService) Initialize(cfg services.Config) error {
	if cfg.RequiredConfirmations > 0 {
		s.confirmations = cfg.RequiredConfirmations
	}
	if cfg.SettlementAddress != "" {
		s.chain = cfg.SettlementAddress
	}
	return nil
}

// SetPollInterval sets the chain-event poll interval.
func (s *IndexerService) SetPollInterval(d time.Duration) {
	if d > 0 {
		s.interval = d
	}
}

func (s *IndexerService) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.stop = make(chan struct{})
	stop := s.stop
	s.mu.Unlock()

	if s.bus != nil {
		s.bus.Subscribe(services.TopicOrderBookTrade, s.HandleTrade)
	}
	if s.adapter != nil {
		go func() {
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if err := s.PollOnce(); err != nil {
						log.Printf("indexer: poll: %v", err)
					}
				}
			}
		}()
	}
	return nil
}

func (s *IndexerService) Stop() error {
	s.mu.Lock()
	if s.started {
		close(s.stop)
		s.started = false
	}
	s.mu.Unlock()
	return nil
}

// HandleTrade persists one fill. It is the callback main passes to the stack so
// the indexer subscribes before the order book replays its journal; the bus also
// invokes it for live trades.
func (s *IndexerService) HandleTrade(e services.Event) error {
	if err := s.handleTrade(e); err != nil {
		s.setDegraded(err)
		return err
	}
	return nil
}

// SetAdapter sets the blockchain adapter used for chain-event polling. main
// calls it after the stack is built.
func (s *IndexerService) SetAdapter(a services.BlockchainAdapter) { s.adapter = a }

// Degraded reports the first index-write error, or nil if none occurred.
func (s *IndexerService) Degraded() error {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	return s.degradedErr
}

func (s *IndexerService) setDegraded(err error) {
	s.degradedMu.Lock()
	if s.degradedErr == nil {
		s.degradedErr = err
	}
	s.degradedMu.Unlock()
	log.Printf("indexer: degraded: %v", err)
}

// handleTrade persists one fill.
func (s *IndexerService) handleTrade(e services.Event) error {
	ev, ok := e.Data.(services.TradeEvent)
	if !ok {
		return errors.New("indexer: trade event has wrong type")
	}
	t := ev.Trade
	return s.store.PutTrade(TradeRecord{
		TradeID:   uint64(t.ID),
		Market:    uint32(t.Market),
		Price:     uint64(t.Price),
		Qty:       t.Qty.Dec(),
		Notional:  t.Notional.Dec(),
		Maker:     t.Maker.Hex(),
		Taker:     t.Taker.Hex(),
		MakerSide: t.MakerSide.String(),
	})
}

// PollOnce fetches and persists all confirmed funding/root logs once, then
// advances the cursor. It is idempotent across runs thanks to the cursor and
// the deterministic store keys.
func (s *IndexerService) PollOnce() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.adapter == nil {
		return nil
	}

	// Seed the cursor from the store on first run so a restart resumes where it
	// left off rather than re-scanning from genesis.
	if s.nextBlock == 1 {
		if h, err := s.store.LastProcessedBlock(); err == nil && h > 0 {
			s.nextBlock = h + 1
		}
	}

	latest, err := s.adapter.GetBlockNumber()
	if err != nil {
		return err
	}
	if latest < s.confirmations {
		return nil
	}
	to := latest - s.confirmations
	if to < s.nextBlock {
		return nil
	}

	topics := [][]string{{
		depositTopic.Hex(), withdrawTopic.Hex(), exitTopic.Hex(), rootTopic.Hex(),
	}}
	logs, err := s.adapter.GetLogs(s.nextBlock, to, s.chain, topics)
	if err != nil {
		return err
	}
	for _, l := range logs {
		if err := s.persistLog(l); err != nil {
			return err
		}
	}
	s.nextBlock = to + 1
	return s.store.SetLastProcessedBlock(to)
}

// persistLog decodes one funding/root log and stores it. Unknown topics are
// ignored (defensive; the filter should have excluded them).
func (s *IndexerService) persistLog(l services.Log) error {
	ev, err := decodeEvent(l)
	if err != nil {
		return err
	}
	if ev == nil {
		return nil // not a recognized funding/root event
	}
	switch ev.Kind {
	case "root":
		return s.store.PutRootEvent(RootEvent{
			RootIndex: ev.RootIndex,
			Root:      ev.Root,
			Timestamp: ev.Timestamp,
			Block:     l.BlockNumber,
		})
	default:
		return s.store.PutFundEvent(FundEvent{
			Kind:      ev.Kind,
			TxHash:    l.TxHash,
			LogIndex:  l.LogIndex,
			Block:     l.BlockNumber,
			Account:   ev.Account,
			Asset:     ev.Asset,
			Amount:    ev.Amount,
			Seq:       ev.Seq,
			RootIndex: ev.RootIndex,
		})
	}
}

// decodedEvent is the intermediate parsed form of one chain log.
type decodedEvent struct {
	Kind      string // deposit | withdrawal | exit | root
	Account   string
	Asset     uint32
	Amount    string
	Seq       uint64
	RootIndex uint64
	Root      string
	Timestamp uint64
}

func decodeEvent(l services.Log) (*decodedEvent, error) {
	if len(l.Topics) < 1 {
		return nil, errors.New("indexer: log missing topic0")
	}
	t0 := l.Topics[0]
	switch {
	case t0 == depositTopic.Hex():
		a, asset, amount, err := decodeAccountAssetAmount(l)
		if err != nil {
			return nil, err
		}
		seq := uint64(0)
		if len(l.Data) >= 64 {
			seq = binary.BigEndian.Uint64(l.Data[56:64])
		}
		return &decodedEvent{Kind: "deposit", Account: a, Asset: asset, Amount: amount, Seq: seq}, nil
	case t0 == withdrawTopic.Hex():
		a, asset, amount, err := decodeAccountAssetAmount(l)
		if err != nil {
			return nil, err
		}
		rootIdx := uint64(0)
		if len(l.Data) >= 64 {
			rootIdx = binary.BigEndian.Uint64(l.Data[56:64])
		}
		return &decodedEvent{Kind: "withdrawal", Account: a, Asset: asset, Amount: amount, RootIndex: rootIdx}, nil
	case t0 == exitTopic.Hex():
		a, asset, amount, err := decodeAccountAssetAmount(l)
		if err != nil {
			return nil, err
		}
		rootIdx := uint64(0)
		if len(l.Data) >= 64 {
			rootIdx = binary.BigEndian.Uint64(l.Data[56:64])
		}
		return &decodedEvent{Kind: "exit", Account: a, Asset: asset, Amount: amount, RootIndex: rootIdx}, nil
	case t0 == rootTopic.Hex():
		if len(l.Data) < 64 {
			return nil, errors.New("indexer: root log short data")
		}
		root := "0x" + common.Bytes2Hex(l.Data[0:32])
		ts := binary.BigEndian.Uint64(l.Data[56:64])
		rootIdx := uint64(0)
		if len(l.Topics) >= 2 {
			rootIdx = new(big.Int).SetBytes(common.HexToHash(l.Topics[1]).Bytes()).Uint64()
		}
		return &decodedEvent{Kind: "root", RootIndex: rootIdx, Root: root, Timestamp: ts}, nil
	default:
		return nil, nil
	}
}

func decodeAccountAssetAmount(l services.Log) (string, uint32, string, error) {
	if len(l.Topics) < 3 {
		return "", 0, "", errors.New("indexer: fund log missing topics")
	}
	if len(l.Data) < 32 {
		return "", 0, "", errors.New("indexer: fund log short data")
	}
	var a ledger.AccountID
	copy(a[:], common.HexToHash(l.Topics[1]).Bytes()[12:32])
	asset := uint32(new(big.Int).SetBytes(common.HexToHash(l.Topics[2]).Bytes()).Uint64())
	amount := new(big.Int).SetBytes(l.Data[0:32]).String()
	return a.Hex(), asset, amount, nil
}

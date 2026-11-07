package services

import (
	"math/big"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// maxHistoryPerAccount bounds memory per account per category. A client only pages
// through the recent past; retaining unbounded history in process memory would grow
// without limit on a busy market.
const maxHistoryPerAccount = 2000

// TradeHistoryEntry is one fill as seen by one account: the shared trade plus that
// account's role, the fee it paid and the PnL it realized.
type TradeHistoryEntry struct {
	TradeID     perp.TradeID
	Market      perp.MarketID
	Price       perp.Price
	Qty         *uint256.Int
	Notional    *uint256.Int
	Side        perp.Side // this account's side on the fill
	Role        perp.FeeRole
	Fee         *uint256.Int
	RealizedPnL *big.Int // signed quote; nil when the fill closed nothing
	At          int64
}

// FundingHistoryEntry is one funding payment this account made or received.
type FundingHistoryEntry struct {
	Market perp.MarketID
	Index  uint64
	Rate   string
	Mark   perp.Price
	Amount *uint256.Int // magnitude
	Sign   int          // +1 received, -1 paid
	At     int64
}

// LiquidationHistoryEntry is one forced close on this account.
type LiquidationHistoryEntry struct {
	Market  perp.MarketID
	Size    *uint256.Int
	Mark    perp.Price
	Equity  *big.Int
	Fee     *uint256.Int
	Partial bool
	At      int64
}

// ADLHistoryEntry is one auto-deleveraging event on this account.
type ADLHistoryEntry struct {
	Market perp.MarketID
	Qty    *uint256.Int
	Mark   perp.Price
	At     int64
}

// tradeKey identifies one account's row within a trade, so a fee or PnL settlement
// can be attached to the right entry without guessing.
type tradeKey struct {
	tradeID perp.TradeID
	account ledger.AccountID
}

// PerpHistory records the per-account history the API serves.
//
// It is fed from the same EventBus topics the WebSocket hub consumes, so a history
// row cannot disagree with a frame a client received. It lives in memory: the
// durable record is the instruction WAL and the on-chain ledger, and a second
// persistent copy here would be a second source of truth for the same facts.
type PerpHistory struct {
	mu sync.RWMutex

	trades       map[ledger.AccountID][]TradeHistoryEntry
	funding      map[ledger.AccountID][]FundingHistoryEntry
	liquidations map[ledger.AccountID][]LiquidationHistoryEntry
	adls         map[ledger.AccountID][]ADLHistoryEntry

	// rowIndex maps (trade, account) to that row's position in the account's slice,
	// so settling a fee or PnL is an O(1) update rather than a scan.
	rowIndex map[tradeKey]int
	// seen trades already recorded, so the explicit record made inside the locked
	// order path and the later event-driven record for the same fill cannot both
	// add a row.
	seen map[perp.TradeID]struct{}
}

// NewPerpHistory builds an empty history store.
func NewPerpHistory() *PerpHistory {
	return &PerpHistory{
		trades:       make(map[ledger.AccountID][]TradeHistoryEntry),
		funding:      make(map[ledger.AccountID][]FundingHistoryEntry),
		liquidations: make(map[ledger.AccountID][]LiquidationHistoryEntry),
		adls:         make(map[ledger.AccountID][]ADLHistoryEntry),
		rowIndex:     make(map[tradeKey]int),
		seen:         make(map[perp.TradeID]struct{}),
	}
}

// Subscribe wires the store to every topic it records.
func (h *PerpHistory) Subscribe(bus *EventBus) {
	bus.Subscribe(TopicPerpTrade, h.onTrade)
	bus.Subscribe(TopicPerpFunding, h.onFunding)
	bus.Subscribe(TopicPerpLiquidation, h.onLiquidation)
	bus.Subscribe(TopicPerpADL, h.onADL)
}

func (h *PerpHistory) onTrade(e Event) error {
	t, ok := e.Data.(perp.Trade)
	if !ok {
		return nil
	}
	at := e.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	h.RecordTrade(t, at)
	return nil
}

// RecordTrade appends a fill to both participants' histories.
func (h *PerpHistory) RecordTrade(t perp.Trade, at time.Time) {
	if t.Qty == nil || t.Qty.IsZero() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, dup := h.seen[t.ID]; dup {
		return
	}
	h.seen[t.ID] = struct{}{}
	// Bound the dedup set: trade ids are monotonically increasing, so keeping only
	// the newest ids above the highest-seen watermark mirrors the row window and
	// cannot resurrect a trimmed trade as a duplicate.
	if len(h.seen) > maxHistoryPerAccount*4 {
		var maxID perp.TradeID
		for id := range h.seen {
			if id > maxID {
				maxID = id
			}
		}
		cutoff := maxID
		if uint64(cutoff) > uint64(maxHistoryPerAccount) {
			cutoff = perp.TradeID(uint64(cutoff) - uint64(maxHistoryPerAccount))
		}
		trimmed := make(map[perp.TradeID]struct{}, maxHistoryPerAccount*2)
		for id := range h.seen {
			if id > cutoff {
				trimmed[id] = struct{}{}
			}
		}
		h.seen = trimmed
	}

	notional := uint256.NewInt(0)
	if t.Notional != nil {
		notional.Set(t.Notional)
	}

	maker := TradeHistoryEntry{
		TradeID: t.ID, Market: t.Market, Price: t.Price,
		Qty: new(uint256.Int).Set(t.Qty), Notional: new(uint256.Int).Set(notional),
		Side: t.MakerSide, Role: perp.FeeMaker, At: at.Unix(),
	}
	h.trades[t.Maker] = appendCapped(h.trades[t.Maker], maker)
	h.rowIndex[tradeKey{tradeID: t.ID, account: t.Maker}] = len(h.trades[t.Maker]) - 1

	takerSide := perp.SideLong
	if t.MakerSide == perp.SideLong {
		takerSide = perp.SideShort
	}
	taker := TradeHistoryEntry{
		TradeID: t.ID, Market: t.Market, Price: t.Price,
		Qty: new(uint256.Int).Set(t.Qty), Notional: new(uint256.Int).Set(notional),
		Side: takerSide, Role: perp.FeeTaker, At: at.Unix(),
	}
	h.trades[t.Taker] = appendCapped(h.trades[t.Taker], taker)
	h.rowIndex[tradeKey{tradeID: t.ID, account: t.Taker}] = len(h.trades[t.Taker]) - 1
}

// AttachSettlement enriches one account's row for a trade with the fee it paid and
// the PnL it realized. Called by the service after the ledger has accepted them, so
// a history row only claims money that actually moved.
func (h *PerpHistory) AttachSettlement(tradeID perp.TradeID, account ledger.AccountID, fee *uint256.Int, pnl *big.Int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx, ok := h.rowIndex[tradeKey{tradeID: tradeID, account: account}]
	if !ok {
		return
	}
	rows := h.trades[account]
	if idx >= len(rows) {
		return
	}
	if fee != nil && !fee.IsZero() {
		rows[idx].Fee = new(uint256.Int).Set(fee)
	}
	if pnl != nil {
		rows[idx].RealizedPnL = new(big.Int).Set(pnl)
	}
}

// appendCapped appends and trims to maxHistoryPerAccount.
func appendCapped[T any](s []T, v T) []T {
	s = append(s, v)
	if len(s) > maxHistoryPerAccount {
		s = s[len(s)-maxHistoryPerAccount:]
	}
	return s
}

func (h *PerpHistory) onFunding(e Event) error {
	r, ok := e.Data.(FundingResult)
	if !ok {
		return nil
	}
	at := e.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range r.Legs {
		h.funding[l.Payer] = appendCapped(h.funding[l.Payer], FundingHistoryEntry{
			Market: r.Market, Index: r.Index, Rate: r.Rate, Mark: r.Mark,
			Amount: new(uint256.Int).Set(l.Amount), Sign: -1, At: at.Unix(),
		})
		h.funding[l.Receiver] = appendCapped(h.funding[l.Receiver], FundingHistoryEntry{
			Market: r.Market, Index: r.Index, Rate: r.Rate, Mark: r.Mark,
			Amount: new(uint256.Int).Set(l.Amount), Sign: 1, At: at.Unix(),
		})
	}
	return nil
}

func (h *PerpHistory) onLiquidation(e Event) error {
	ev, ok := e.Data.(LiquidationEvent)
	if !ok {
		return nil
	}
	at := ev.At
	if at == 0 {
		at = time.Now().Unix()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.liquidations[ev.Account] = appendCapped(h.liquidations[ev.Account], LiquidationHistoryEntry{
		Market: ev.Market, Size: cloneAmt(ev.Size), Mark: ev.Mark,
		Equity: ev.Equity, Fee: cloneAmt(ev.Fee), Partial: ev.Partial, At: at,
	})
	return nil
}

func (h *PerpHistory) onADL(e Event) error {
	ev, ok := e.Data.(ADLEvent)
	if !ok {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.adls[ev.Account] = appendCapped(h.adls[ev.Account], ADLHistoryEntry{
		Market: ev.Market, Qty: cloneAmt(ev.Qty), Mark: ev.Mark, At: time.Now().Unix(),
	})
	return nil
}

// pnlToSigned converts a PnLSettlement amount into a signed value.
//
// uint256 stores a negative amount as its two's-complement bit pattern, so
// ToBig() on a negative value yields a huge positive number and Sign() is the only
// reliable direction signal. The magnitude must therefore be recovered by
// negating on the uint256 side first, then widening.
func pnlToSigned(p *uint256.Int) *big.Int {
	if p == nil {
		return nil
	}
	negative := p.Sign() < 0
	mag := new(uint256.Int).Set(p)
	if negative {
		mag.Neg(mag) // back to the magnitude
	}
	out := new(big.Int).Set(mag.ToBig())
	if negative {
		out.Neg(out)
	}
	return out
}

// cloneAmt returns a defensive copy of an amount.
func cloneAmt(v *uint256.Int) *uint256.Int {
	if v == nil {
		return nil
	}
	return new(uint256.Int).Set(v)
}

// Trades returns an account's fills, newest first, optionally filtered by market.
func (h *PerpHistory) Trades(a ledger.AccountID, market *perp.MarketID, limit int) []TradeHistoryEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	src := h.trades[a]
	out := make([]TradeHistoryEntry, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		if market != nil && src[i].Market != *market {
			continue
		}
		e := src[i]
		e.Qty = cloneAmt(e.Qty)
		e.Notional = cloneAmt(e.Notional)
		e.Fee = cloneAmt(e.Fee)
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Funding returns an account's funding payments, newest first.
func (h *PerpHistory) Funding(a ledger.AccountID, limit int) []FundingHistoryEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	src := h.funding[a]
	out := make([]FundingHistoryEntry, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		e := src[i]
		e.Amount = cloneAmt(e.Amount)
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Liquidations returns an account's forced closes, newest first.
func (h *PerpHistory) Liquidations(a ledger.AccountID, limit int) []LiquidationHistoryEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	src := h.liquidations[a]
	out := make([]LiquidationHistoryEntry, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		e := src[i]
		e.Size = cloneAmt(e.Size)
		e.Fee = cloneAmt(e.Fee)
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ADLs returns an account's auto-deleveraging events, newest first.
func (h *PerpHistory) ADLs(a ledger.AccountID, limit int) []ADLHistoryEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	src := h.adls[a]
	out := make([]ADLHistoryEntry, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		e := src[i]
		e.Qty = cloneAmt(e.Qty)
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}


// Package indexer persists order-book history (trades, on-chain funding
// events, state roots) and can rebuild an independent ledger from on-chain
// events to reconcile against the running ledger. It never mutates the
// running ledger or the matching kernel.
package indexer

import "web3-amm-dex-backend/ledger"

// TradeRecord is the persisted form of one on-chain-book fill.
type TradeRecord struct {
	TradeID   uint64 `json:"tradeId"`
	Market    uint32 `json:"market"`
	Price     uint64 `json:"price"`
	Qty       string `json:"qty"`
	Notional  string `json:"notional"`
	Maker     string `json:"maker"`
	Taker     string `json:"taker"`
	MakerSide string `json:"makerSide"`
}

// FundEvent is one on-chain funding event (deposit / withdrawal / forced exit).
type FundEvent struct {
	Kind      string `json:"kind"` // deposit | withdrawal | exit
	TxHash    string `json:"txHash"`
	LogIndex  uint   `json:"logIndex"`
	Block     uint64 `json:"block"`
	Account   string `json:"account"`
	Asset     uint32 `json:"asset"`
	Amount    string `json:"amount"`
	Seq       uint64 `json:"seq,omitempty"`       // deposit only
	RootIndex uint64 `json:"rootIndex,omitempty"` // withdrawal/exit only
}

// RootEvent is one committed state root.
type RootEvent struct {
	RootIndex uint64 `json:"rootIndex"`
	Root      string `json:"root"`
	Timestamp uint64 `json:"timestamp"`
	Block     uint64 `json:"block"`
}

// AssetDelta reports the per-asset reconciliation result between a rebuilt
// ledger and the running ledger.
type AssetDelta struct {
	Asset   ledger.AssetID `json:"asset"`
	Rebuilt string         `json:"rebuilt"`
	Running string         `json:"running"`
	Match   bool           `json:"match"`
}

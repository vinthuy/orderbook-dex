package models

// Asset describes a token registered in the on-chain Settlement contract.
type Asset struct {
	AssetID  uint32 `json:"assetId"`
	Token    string `json:"token"`
	Decimals uint8  `json:"decimals"`
	Symbol   string `json:"symbol"`
}

// AnchoredRoot records a committed state root and the ledger version it
// captures, for aligning the off-chain ledger with on-chain root indexes.
type AnchoredRoot struct {
	RootIndex     uint64 `json:"rootIndex"`
	Root          string `json:"root"`
	LedgerVersion uint64 `json:"ledgerVersion"`
}

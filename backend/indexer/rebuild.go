package indexer

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

// Rebuild replays the settlement contract's funding events into a fresh ledger,
// yielding per-asset total balances. Matching legs are zero-sum and absent from
// the chain, so only per-asset totals are meaningful (spec §6.1). It is
// read-only with respect to the running ledger.
func Rebuild(adapter services.BlockchainAdapter, chain string, fromBlock uint64) (*ledger.Ledger, error) {
	l := ledger.New()
	if adapter == nil {
		return nil, errors.New("indexer: rebuild requires an adapter")
	}
	latest, err := adapter.GetBlockNumber()
	if err != nil {
		return nil, err
	}
	topics := [][]string{{
		depositTopic.Hex(), withdrawTopic.Hex(), exitTopic.Hex(),
	}}
	logs, err := adapter.GetLogs(fromBlock, latest, chain, topics)
	if err != nil {
		return nil, err
	}
	// Rebuild in event order (block, then log index) so deposits/withdrawals
	// apply in the order they occurred.
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].LogIndex < logs[j].LogIndex
	})
	for _, lg := range logs {
		ev, err := decodeEvent(lg)
		if err != nil {
			return nil, err
		}
		if ev == nil || ev.Kind == "root" {
			continue
		}
		account, err := ledger.AccountFromHex(ev.Account)
		if err != nil {
			return nil, err
		}
		amount, err := uint256.FromDecimal(ev.Amount)
		if err != nil {
			return nil, fmt.Errorf("indexer: rebuild: bad amount %q: %w", ev.Amount, err)
		}
		ref := lg.TxHash + ":" + strconv.Itoa(int(lg.LogIndex))
		switch ev.Kind {
		case "deposit":
			if err := l.Apply(ledger.DepositOp{Account: account, Asset: ledger.AssetID(ev.Asset), Amount: amount, Ref: ref}); err != nil {
				return nil, err
			}
		case "withdrawal", "exit":
			if err := l.Apply(ledger.WithdrawOp{Account: account, Asset: ledger.AssetID(ev.Asset), Amount: amount, Ref: ref}); err != nil {
				return nil, err
			}
		}
	}
	return l, nil
}

// CompareTotals compares the rebuilt ledger's per-asset totals against the
// running ledger's, returning one delta per asset present in either ledger.
func CompareTotals(rebuilt, running *ledger.Ledger) ([]AssetDelta, error) {
	if rebuilt == nil || running == nil {
		return nil, errors.New("indexer: compare requires both ledgers")
	}
	assets := map[ledger.AssetID]bool{}
	for _, a := range rebuilt.Assets() {
		assets[a] = true
	}
	for _, a := range running.Assets() {
		assets[a] = true
	}
	sorted := make([]ledger.AssetID, 0, len(assets))
	for a := range assets {
		sorted = append(sorted, a)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := make([]AssetDelta, 0, len(sorted))
	for _, a := range sorted {
		rb := rebuilt.TotalBalance(a).Dec()
		rn := running.TotalBalance(a).Dec()
		out = append(out, AssetDelta{Asset: a, Rebuilt: rb, Running: rn, Match: rb == rn})
	}
	return out, nil
}

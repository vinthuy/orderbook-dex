package indexer

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

// Rebuild replays the settlement contract's funding events into a fresh ledger,
// yielding per-asset total balances. Matching legs are zero-sum and absent from
// the chain, so per-account balances are NOT reconstructed — instead each
// asset's net flow (Σdeposit − Σwithdrawal − Σexit) is accumulated and credited
// to a single synthetic account, giving the correct per-asset total without the
// per-account non-negativity constraint that would reject a profitable trader.
func Rebuild(adapter services.BlockchainAdapter, chain string, fromBlock uint64, confirmations uint64) (*ledger.Ledger, error) {
	l := ledger.New()
	if adapter == nil {
		return nil, errors.New("indexer: rebuild requires an adapter")
	}
	latest, err := adapter.GetBlockNumber()
	if err != nil {
		return nil, err
	}
	if confirmations == 0 {
		confirmations = 1
	}
	if latest < confirmations {
		return l, nil // not enough confirmed blocks yet
	}
	to := latest - confirmations

	topics := [][]string{{depositTopic.Hex(), withdrawTopic.Hex(), exitTopic.Hex()}}
	logs, err := adapter.GetLogs(fromBlock, to, chain, topics)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].LogIndex < logs[j].LogIndex
	})

	net := make(map[ledger.AssetID]*big.Int)
	for _, lg := range logs {
		ev, err := decodeEvent(lg)
		if err != nil {
			return nil, err
		}
		if ev == nil || ev.Kind == "root" {
			continue
		}
		amount, ok := new(big.Int).SetString(ev.Amount, 10)
		if !ok {
			return nil, fmt.Errorf("indexer: rebuild: bad amount %q", ev.Amount)
		}
		asset := ledger.AssetID(ev.Asset)
		cur := net[asset]
		if cur == nil {
			cur = new(big.Int)
			net[asset] = cur
		}
		switch ev.Kind {
		case "deposit":
			cur.Add(cur, amount)
		case "withdrawal", "exit":
			cur.Sub(cur, amount)
		}
	}

	synth := ledger.AccountID{} // zero account: a synthetic vault bucket, never a real trader
	for asset, n := range net {
		if n.Sign() < 0 {
			return nil, fmt.Errorf("indexer: rebuild: asset %d has negative net %s", asset, n.String())
		}
		if n.Sign() == 0 {
			continue
		}
		amt, overflow := uint256.FromBig(n)
		if overflow {
			return nil, fmt.Errorf("indexer: rebuild: asset %d net overflows uint256", asset)
		}
		if err := l.Apply(ledger.DepositOp{Account: synth, Asset: asset, Amount: amt, Ref: fmt.Sprintf("rebuild:%d", asset)}); err != nil {
			return nil, err
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

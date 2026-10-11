package ledger

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

type leafEntry struct {
	account AccountID
	asset   AssetID
	balance *uint256.Int
}

// leafHash must match MerkleLib.balanceLeaf on-chain:
// keccak256(bytes20(account) || bytes32(uint256(assetId)) || bytes32(balance))
func leafHash(a AccountID, asset AssetID, balance *uint256.Int) [32]byte {
	var buf [84]byte
	copy(buf[0:20], a[:])
	var assetBytes [32]byte
	binary.BigEndian.PutUint32(assetBytes[28:], uint32(asset))
	copy(buf[20:52], assetBytes[:])
	b := balance.Bytes32()
	copy(buf[52:84], b[:])
	h := crypto.Keccak256(buf[:])
	var out [32]byte
	copy(out[:], h)
	return out
}

func hashPair(a, b [32]byte) [32]byte {
	var buf [64]byte
	if bytes.Compare(a[:], b[:]) <= 0 {
		copy(buf[0:32], a[:])
		copy(buf[32:64], b[:])
	} else {
		copy(buf[0:32], b[:])
		copy(buf[32:64], a[:])
	}
	h := crypto.Keccak256(buf[:])
	var out [32]byte
	copy(out[:], h)
	return out
}

// reduceLevel performs exactly one Merkle level reduction: adjacent nodes are
// combined with hashPair, and a trailing odd node is promoted unchanged (never
// duplicated). This is the single definition of the tree rule shared by both
// merkleRoot and merkleProof, and it must stay identical to
// MerkleLib.computeRoot on-chain.
func reduceLevel(level [][32]byte) [][32]byte {
	next := make([][32]byte, 0, (len(level)+1)/2)
	for i := 0; i < len(level); i += 2 {
		if i+1 == len(level) {
			next = append(next, level[i]) // promote odd node unchanged
		} else {
			next = append(next, hashPair(level[i], level[i+1]))
		}
	}
	return next
}

func merkleRoot(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return [32]byte{}
	}
	level := leaves
	for len(level) > 1 {
		level = reduceLevel(level)
	}
	return level[0]
}

func merkleProof(leaves [][32]byte, index int) [][32]byte {
	var proof [][32]byte
	level := leaves
	idx := index
	for len(level) > 1 {
		if idx%2 == 0 {
			if idx+1 < len(level) {
				proof = append(proof, level[idx+1])
			}
		} else {
			proof = append(proof, level[idx-1])
		}
		level = reduceLevel(level)
		idx /= 2
	}
	return proof
}

func verifyProof(proof [][32]byte, root, leaf [32]byte) bool {
	computed := leaf
	for _, p := range proof {
		computed = hashPair(computed, p)
	}
	return computed == root
}

// leafEntries returns all non-zero balances sorted by (account, asset).
func (l *Ledger) leafEntries() []leafEntry {
	var out []leafEntry
	for a, m := range l.balances {
		for asset, b := range m {
			if b.IsZero() {
				continue
			}
			out = append(out, leafEntry{a, asset, b})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if c := bytes.Compare(out[i].account[:], out[j].account[:]); c != 0 {
			return c < 0
		}
		return out[i].asset < out[j].asset
	})
	return out
}

func (l *Ledger) leavesLocked() [][32]byte {
	entries := l.leafEntries()
	leaves := make([][32]byte, len(entries))
	for i, e := range entries {
		leaves[i] = leafHash(e.account, e.asset, e.balance)
	}
	return leaves
}

// Root returns the deterministic Merkle root of all balances.
func (l *Ledger) Root() [32]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return merkleRoot(l.leavesLocked())
}

// Proof returns the Merkle proof and balance for one (account, asset).
func (l *Ledger) Proof(a AccountID, asset AssetID) ([][32]byte, *uint256.Int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.leafEntries()
	leaves := make([][32]byte, len(entries))
	idx := -1
	for i, e := range entries {
		leaves[i] = leafHash(e.account, e.asset, e.balance)
		if e.account == a && e.asset == asset {
			idx = i
		}
	}
	if idx < 0 {
		return nil, nil, ErrNoLeaf
	}
	return merkleProof(leaves, idx), new(uint256.Int).Set(entries[idx].balance), nil
}

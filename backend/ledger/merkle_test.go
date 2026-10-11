package ledger

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSingleLeafRootEqualsLeaf(t *testing.T) {
	l := New()
	a := acct(1)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})

	root := l.Root()
	leaf := leafHash(a, 1, amt(100))
	if root != leaf {
		t.Fatalf("root %x != leaf %x", root, leaf)
	}
}

func TestRootIsDeterministic(t *testing.T) {
	build := func() *Ledger {
		l := New()
		_ = l.Apply(DepositOp{Account: acct(2), Asset: 3, Amount: amt(5), Ref: "d1"})
		_ = l.Apply(DepositOp{Account: acct(1), Asset: 1, Amount: amt(7), Ref: "d2"})
		return l
	}
	if build().Root() != build().Root() {
		t.Fatal("root is not deterministic")
	}
}

func TestProofRoundTrip(t *testing.T) {
	l := New()
	a, b, c := acct(1), acct(2), acct(3)
	_ = l.Apply(DepositOp{Account: a, Asset: 1, Amount: amt(100), Ref: "d1"})
	_ = l.Apply(DepositOp{Account: b, Asset: 1, Amount: amt(200), Ref: "d2"})
	_ = l.Apply(DepositOp{Account: c, Asset: 1, Amount: amt(300), Ref: "d3"})

	root := l.Root()
	for _, x := range []AccountID{a, b, c} {
		proof, balance, err := l.Proof(x, 1)
		if err != nil {
			t.Fatalf("proof: %v", err)
		}
		leaf := leafHash(x, 1, balance)
		if !verifyProof(proof, root, leaf) {
			t.Fatalf("proof does not verify for %x", x)
		}
	}
}

func TestGoldenVectors(t *testing.T) {
	path := filepath.Join("testdata", "merkle_vectors.json")
	l := New()
	accounts := []AccountID{acct(0x11), acct(0x22), acct(0x33)}
	assets := []AssetID{1, 1, 2}
	amounts := []uint64{100, 200, 300}
	for i := range accounts {
		_ = l.Apply(DepositOp{Account: accounts[i], Asset: assets[i], Amount: amt(amounts[i]), Ref: "d" + string(rune('1'+i))})
	}

	type leafJSON struct {
		Account string `json:"account"`
		AssetID uint32 `json:"assetId"`
		Balance string `json:"balance"`
		Leaf    string `json:"leaf"`
	}
	type proofJSON struct {
		Index int      `json:"index"`
		Proof []string `json:"proof"`
	}
	type fixture struct {
		Root   string      `json:"root"`
		Leaves []leafJSON  `json:"leaves"`
		Proofs []proofJSON `json:"proofs"`
	}

	entries := l.leafEntries()
	fx := fixture{Root: "0x" + hex.EncodeToString(hexBytes(l.Root()))}
	for i, e := range entries {
		fx.Leaves = append(fx.Leaves, leafJSON{
			Account: e.account.Hex(),
			AssetID: uint32(e.asset),
			Balance: e.balance.Dec(),
			Leaf:    "0x" + hex.EncodeToString(hexBytes(leafHash(e.account, e.asset, e.balance))),
		})
		proof, _, err := l.Proof(e.account, e.asset)
		if err != nil {
			t.Fatalf("proof %d: %v", i, err)
		}
		pj := proofJSON{Index: i}
		for _, p := range proof {
			pj.Proof = append(pj.Proof, "0x"+hex.EncodeToString(hexBytes(p)))
		}
		fx.Proofs = append(fx.Proofs, pj)
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		data, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("golden vectors written to %s", path)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (run with UPDATE_GOLDEN=1 first): %v", err)
	}
	var want fixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fx.Root != want.Root {
		t.Fatalf("root drifted: got %s want %s", fx.Root, want.Root)
	}
	if len(fx.Leaves) != len(want.Leaves) {
		t.Fatalf("leaf count drifted: got %d want %d", len(fx.Leaves), len(want.Leaves))
	}
	for i := range want.Leaves {
		if fx.Leaves[i].Leaf != want.Leaves[i].Leaf {
			t.Fatalf("leaf %d drifted: got %s want %s", i, fx.Leaves[i].Leaf, want.Leaves[i].Leaf)
		}
	}
}

func hexBytes(b [32]byte) []byte { return b[:] }

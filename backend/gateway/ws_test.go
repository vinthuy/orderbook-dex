package gateway

import (
	"testing"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/matching"
)

func mkMarket(id matching.MarketID) *matching.MarketID { return &id }
func mkAcct(n byte) *ledger.AccountID                  { a := acct(n); return &a }

func TestSubscriptionMatches(t *testing.T) {
	m := mkMarket(1)
	a := mkAcct(1)

	cases := []struct {
		name    string
		sub     subscription
		market  *matching.MarketID
		account *ledger.AccountID
		want    bool
	}{
		{"all", subscription{}, mkMarket(1), mkAcct(1), true},
		{"market match", subscription{market: m}, mkMarket(1), mkAcct(9), true},
		{"market mismatch", subscription{market: m}, mkMarket(2), mkAcct(1), false},
		{"account match", subscription{account: a}, mkMarket(9), mkAcct(1), true},
		{"account mismatch", subscription{account: a}, mkMarket(1), mkAcct(2), false},
		{"market+account match", subscription{market: m, account: a}, mkMarket(1), mkAcct(1), true},
		{"market+account account mismatch", subscription{market: m, account: a}, mkMarket(1), mkAcct(2), false},
	}
	for _, c := range cases {
		if got := c.sub.matches(c.market, c.account); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}

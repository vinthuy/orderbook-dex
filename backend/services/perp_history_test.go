package services

import (
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/perp"
)

// TestHistoryRecordsBothSidesOfATrade pins that one fill produces one row per
// participant, each with its own side and role.
func TestHistoryRecordsBothSidesOfATrade(t *testing.T) {
	svc, _ := feeService(t, 0, 0)
	maker := perpAcct(1)
	taker := perpAcct(2)

	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: maker, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: taker, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	h := svc.History()

	mk := h.Trades(maker, nil, 10)
	if len(mk) != 1 {
		t.Fatalf("maker rows = %d, want 1", len(mk))
	}
	if mk[0].Role != perp.FeeMaker || mk[0].Side != perp.SideShort {
		t.Fatalf("maker row = role %s side %s", mk[0].Role, mk[0].Side)
	}

	tk := h.Trades(taker, nil, 10)
	if len(tk) != 1 {
		t.Fatalf("taker rows = %d, want 1", len(tk))
	}
	if tk[0].Role != perp.FeeTaker || tk[0].Side != perp.SideLong {
		t.Fatalf("taker row = role %s side %s", tk[0].Role, tk[0].Side)
	}
	if tk[0].Qty.Uint64() != 1000 || tk[0].Price != 100 {
		t.Fatalf("taker qty/price = %s/%d", tk[0].Qty, tk[0].Price)
	}
}

// TestHistoryRecordsTheActualFeeCharged pins that the row reports what the ledger
// took, not what was quoted.
func TestHistoryRecordsTheActualFeeCharged(t *testing.T) {
	svc, _ := feeService(t, 200000000000000, 500000000000000)
	maker := perpAcct(1)
	taker := perpAcct(2)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: maker, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: taker, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	h := svc.History()
	if f := h.Trades(maker, nil, 1)[0].Fee; f == nil || f.Uint64() != 20 {
		t.Fatalf("maker fee row = %v, want 20", f)
	}
	if f := h.Trades(taker, nil, 1)[0].Fee; f == nil || f.Uint64() != 50 {
		t.Fatalf("taker fee row = %v, want 50", f)
	}
}

// TestHistoryRecordsRealizedPnLSign pins that a losing close is negative.
func TestHistoryRecordsRealizedPnLSign(t *testing.T) {
	l := ledger.New()
	holds := ledger.NewHolds(l)
	markets := []perp.Market{{
		ID: 1, Symbol: "ETH-PERP", Base: 2, Quote: 1, BaseDecimals: 0, MaxLeverage: 10,
		RiskConfig: perp.RiskConfig{SlippageE18: uint256.NewInt(0)},
	}}
	for i, ref := range []string{"d1", "d2", "d3"} {
		if err := l.Apply(ledger.DepositOp{Account: perpAcct(byte(i + 1)), Asset: 1,
			Amount: uint256.NewInt(10000000), Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewPerpOrderBookService(markets, l, holds, mustPerpJournal(t, ""), NewEventBus())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	loser := perpAcct(2)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: loser, Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(3), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 90, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: loser, Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 90, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}

	rows := svc.History().Trades(loser, nil, 5)
	found := false
	for _, r := range rows {
		if r.RealizedPnL != nil {
			found = true
			if r.RealizedPnL.Sign() >= 0 {
				t.Fatalf("a losing close must be negative, got %s", r.RealizedPnL)
			}
			if r.RealizedPnL.Cmp(big.NewInt(-10000)) != 0 {
				t.Fatalf("realized PnL = %s, want -10000", r.RealizedPnL)
			}
		}
	}
	if !found {
		t.Fatal("no row carries a realized PnL")
	}
}

func TestHistoryFiltersByMarket(t *testing.T) {
	svc, _ := feeService(t, 0, 0)
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(1), Side: perp.SideShort,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Place(perp.PlaceRequest{Market: 1, Account: perpAcct(2), Side: perp.SideLong,
		Type: perp.TypeLimit, TIF: perp.TIFGTC, Price: 100, Qty: uint256.NewInt(1000), Leverage: 10}); err != nil {
		t.Fatal(err)
	}
	other := perp.MarketID(99)
	if rows := svc.History().Trades(perpAcct(1), &other, 10); len(rows) != 0 {
		t.Fatalf("want no rows for an unrelated market, got %d", len(rows))
	}
	one := perp.MarketID(1)
	if rows := svc.History().Trades(perpAcct(1), &one, 10); len(rows) != 1 {
		t.Fatalf("want 1 row for market 1, got %d", len(rows))
	}
}

func TestHistoryReturnsNewestFirst(t *testing.T) {
	h := NewPerpHistory()
	a := perpAcct(1)
	base := time.Now()
	for i := 0; i < 3; i++ {
		h.RecordTrade(perp.Trade{
			ID: perp.TradeID(i + 1), Market: 1, Price: perp.Price(100 + i),
			Qty: uint256.NewInt(10), Notional: uint256.NewInt(1000), Maker: a, Taker: perpAcct(2),
		}, base.Add(time.Duration(i)*time.Second))
	}
	rows := h.Trades(a, nil, 10)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	if rows[0].TradeID != 3 || rows[2].TradeID != 1 {
		t.Fatalf("want newest first, got %d..%d", rows[0].TradeID, rows[2].TradeID)
	}
}

func TestHistoryCapsRetention(t *testing.T) {
	h := NewPerpHistory()
	a := perpAcct(1)
	base := time.Now()
	for i := 0; i < maxHistoryPerAccount+50; i++ {
		h.RecordTrade(perp.Trade{
			ID: perp.TradeID(i + 1), Market: 1, Price: 100,
			Qty: uint256.NewInt(1), Notional: uint256.NewInt(100), Maker: a, Taker: perpAcct(2),
		}, base.Add(time.Duration(i)*time.Millisecond))
	}
	if rows := h.Trades(a, nil, 0); len(rows) != maxHistoryPerAccount {
		t.Fatalf("want %d retained, got %d", maxHistoryPerAccount, len(rows))
	}
}


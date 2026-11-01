package services

import (
	"testing"

	"github.com/holiman/uint256"

	"web3-amm-dex-backend/perp"
)

func TestPerpJournalAppendsMarkAndFunding(t *testing.T) {
	j := mustPerpJournal(t, t.TempDir())
	defer j.Close()

	if err := j.AppendMark(MarkCmd{Market: 1, Index: 2000, Mark: 2001, At: 100}); err != nil {
		t.Fatalf("AppendMark: %v", err)
	}
	payer := perpAcct(1)
	receiver := perpAcct(2)
	amt, _ := uint256.FromDecimal("1000000000000000")
	if err := j.AppendFunding(FundingCmd{
		Market: 1, Index: 7, Rate: "1000000000000000", Mark: 2001, At: 200,
		Legs: []FundingLeg{{Payer: payer, Receiver: receiver, Amount: amt}},
	}); err != nil {
		t.Fatalf("AppendFunding: %v", err)
	}

	ins, err := j.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(ins) != 2 {
		t.Fatalf("want 2 instructions, got %d", len(ins))
	}
	if ins[0].Kind != "mark" || ins[0].Mark == nil || ins[0].Mark.Mark != 2001 || ins[0].Mark.Index != 2000 {
		t.Fatalf("bad mark instruction: %+v", ins[0])
	}
	if ins[1].Kind != "funding" || ins[1].Funding == nil {
		t.Fatalf("bad funding instruction: %+v", ins[1])
	}
	f := ins[1].Funding
	if f.Index != 7 || f.Rate != "1000000000000000" || f.Mark != 2001 || len(f.Legs) != 1 {
		t.Fatalf("bad funding payload: %+v", f)
	}
	if f.Legs[0].Payer != payer || f.Legs[0].Receiver != receiver {
		t.Fatalf("funding leg parties mismatch")
	}
	if f.Legs[0].Amount.Cmp(amt) != 0 {
		t.Fatalf("funding leg amount mismatch: %s", f.Legs[0].Amount)
	}
}

func TestPerpJournalReplaysPlaceAfterMark(t *testing.T) {
	// A mixed log must still decode place/cancel in order: the new kinds must
	// not break the existing replay path.
	j := mustPerpJournal(t, t.TempDir())
	defer j.Close()

	if err := j.AppendMark(MarkCmd{Market: 1, Index: 2000, Mark: 2001, At: 1}); err != nil {
		t.Fatalf("AppendMark: %v", err)
	}
	if err := j.AppendPlace(perp.PlaceRequest{
		Market: 1, Account: perpAcct(1), Side: perp.SideLong, Type: perp.TypeLimit,
		TIF: perp.TIFGTC, Price: 2000, Qty: uint256.NewInt(100), Leverage: 10,
	}); err != nil {
		t.Fatalf("AppendPlace: %v", err)
	}

	ins, err := j.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(ins) != 2 || ins[0].Kind != "mark" || ins[1].Kind != "place" {
		t.Fatalf("unexpected replay order: %+v", ins)
	}
	if ins[1].Place == nil || ins[1].Place.Qty.Uint64() != 100 {
		t.Fatalf("place payload lost: %+v", ins[1].Place)
	}
}

func TestPerpJournalRejectsUnknownKind(t *testing.T) {
	j := mustPerpJournal(t, t.TempDir())
	defer j.Close()
	if err := j.append("bogus", map[string]any{"x": 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := j.Replay(); err == nil {
		t.Fatal("want error for unknown journal kind")
	}
}



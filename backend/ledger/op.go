package ledger

import (
	"fmt"

	"github.com/holiman/uint256"
)

// Op is a single ledger mutation with an idempotency key.
type Op interface {
	Key() string
	walRecord() OpRecord
	apply(t *tx) error
}

type DepositOp struct {
	Account AccountID
	Asset   AssetID
	Amount  *uint256.Int
	Ref     string
}

func (o DepositOp) Key() string { return o.Ref }
func (o DepositOp) walRecord() OpRecord {
	return OpRecord{Type: "deposit", Account: o.Account.Hex(), Asset: uint32(o.Asset), Amount: o.Amount.Dec(), Ref: o.Ref}
}
func (o DepositOp) apply(t *tx) error {
	cur := t.get(o.Account, o.Asset)
	t.record(o.Account, o.Asset)
	t.set(o.Account, o.Asset, new(uint256.Int).Add(cur, o.Amount))
	return nil
}

type WithdrawOp struct {
	Account AccountID
	Asset   AssetID
	Amount  *uint256.Int
	Ref     string
}

func (o WithdrawOp) Key() string { return o.Ref }
func (o WithdrawOp) walRecord() OpRecord {
	return OpRecord{Type: "withdraw", Account: o.Account.Hex(), Asset: uint32(o.Asset), Amount: o.Amount.Dec(), Ref: o.Ref}
}
func (o WithdrawOp) apply(t *tx) error {
	cur := t.get(o.Account, o.Asset)
	if cur.Cmp(o.Amount) < 0 {
		return ErrInsufficientBalance
	}
	t.record(o.Account, o.Asset)
	t.set(o.Account, o.Asset, new(uint256.Int).Sub(cur, o.Amount))
	return nil
}

type TransferOp struct {
	From, To AccountID
	Asset    AssetID
	Amount   *uint256.Int
	Ref      string
}

func (o TransferOp) Key() string { return o.Ref }
func (o TransferOp) walRecord() OpRecord {
	return OpRecord{Type: "transfer", From: o.From.Hex(), To: o.To.Hex(), Asset: uint32(o.Asset), Amount: o.Amount.Dec(), Ref: o.Ref}
}
func (o TransferOp) apply(t *tx) error {
	if o.From == o.To {
		// Self-transfer moves nothing; treat it as a successful no-op so the
		// key is still recorded as applied and conservation is preserved.
		return nil
	}
	from := t.get(o.From, o.Asset)
	if from.Cmp(o.Amount) < 0 {
		return ErrInsufficientBalance
	}
	to := t.get(o.To, o.Asset)
	t.record(o.From, o.Asset)
	t.record(o.To, o.Asset)
	t.set(o.From, o.Asset, new(uint256.Int).Sub(from, o.Amount))
	t.set(o.To, o.Asset, new(uint256.Int).Add(to, o.Amount))
	return nil
}

// OpRecord is the serializable form of an Op used in the WAL.
type OpRecord struct {
	Type    string `json:"type"`
	Account string `json:"account,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Asset   uint32 `json:"asset"`
	Amount  string `json:"amount"`
	Ref     string `json:"ref"`
}

func (r OpRecord) ToOp() (Op, error) {
	amount, err := uint256.FromDecimal(r.Amount)
	if err != nil {
		return nil, fmt.Errorf("ledger: bad amount %q: %w", r.Amount, err)
	}
	switch r.Type {
	case "deposit":
		a, err := AccountFromHex(r.Account)
		if err != nil {
			return nil, err
		}
		return DepositOp{Account: a, Asset: AssetID(r.Asset), Amount: amount, Ref: r.Ref}, nil
	case "withdraw":
		a, err := AccountFromHex(r.Account)
		if err != nil {
			return nil, err
		}
		return WithdrawOp{Account: a, Asset: AssetID(r.Asset), Amount: amount, Ref: r.Ref}, nil
	case "transfer":
		from, err := AccountFromHex(r.From)
		if err != nil {
			return nil, err
		}
		to, err := AccountFromHex(r.To)
		if err != nil {
			return nil, err
		}
		return TransferOp{From: from, To: to, Asset: AssetID(r.Asset), Amount: amount, Ref: r.Ref}, nil
	}
	return nil, fmt.Errorf("ledger: unknown op type %q", r.Type)
}

package gateway

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/holiman/uint256"
)

// EIP-712 domain: pure off-chain verification, no bearing contract, so the
// verifying contract is the zero address (spec §4.2).
var (
	orderTypes = apitypes.Types{
		"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		},
		"Order": {
			{Name: "market", Type: "uint32"},
			{Name: "side", Type: "uint8"},
			{Name: "type", Type: "uint8"},
			{Name: "tif", Type: "uint8"},
			{Name: "price", Type: "uint64"},
			{Name: "qty", Type: "uint256"},
			{Name: "maxNotional", Type: "uint256"},
			{Name: "nonce", Type: "uint64"},
			{Name: "expiry", Type: "uint64"},
		},
	}
	cancelTypes = apitypes.Types{
		"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		},
		"Cancel": {
			{Name: "market", Type: "uint32"},
			{Name: "orderId", Type: "uint64"},
			{Name: "nonce", Type: "uint64"},
			{Name: "expiry", Type: "uint64"},
		},
	}
)

// SignedOrder is the EIP-712 typed payload for a place instruction. Qty and
// MaxNotional are decimal strings to avoid float/scientific-notation ambiguity;
// MaxNotional is "0" when not applicable.
type SignedOrder struct {
	Market      uint32 `json:"market"`
	Side        uint8  `json:"side"`
	Type        uint8  `json:"type"`
	TIF         uint8  `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"maxNotional"`
	Nonce       uint64 `json:"nonce"`
	Expiry      uint64 `json:"expiry"`
}

// SignedOrderEnvelope is the wire form: the order plus its 65-byte signature.
type SignedOrderEnvelope struct {
	Order     SignedOrder   `json:"order"`
	Signature hexutil.Bytes `json:"signature"`
}

// SignedCancel is the EIP-712 typed payload for a cancel instruction. It shares
// the account's nonce space with orders (spec §4.2).
type SignedCancel struct {
	Market  uint32 `json:"market"`
	OrderID uint64 `json:"orderId"`
	Nonce   uint64 `json:"nonce"`
	Expiry  uint64 `json:"expiry"`
}

// SignedCancelEnvelope is the wire form of a signed cancel.
type SignedCancelEnvelope struct {
	Cancel    SignedCancel  `json:"cancel"`
	Signature hexutil.Bytes `json:"signature"`
}

// Verifier builds EIP-712 digests and recovers the signer account.
type Verifier struct {
	domain apitypes.TypedDataDomain
}

func NewVerifier(chainID int64, name, version string) *Verifier {
	if name == "" {
		name = DefaultDomainName
	}
	if version == "" {
		version = DefaultDomainVersion
	}
	return &Verifier{domain: apitypes.TypedDataDomain{
		Name:              name,
		Version:           version,
		ChainId:           math.NewHexOrDecimal256(chainID),
		VerifyingContract: "0x0000000000000000000000000000000000000000",
	}}
}

// parseUint parses a decimal string, treating "" as zero (an absent optional
// field hashes as 0).
func parseUint(s string) (*uint256.Int, error) {
	if s == "" {
		return uint256.NewInt(0), nil
	}
	v, err := uint256.FromDecimal(s)
	if err != nil {
		return nil, fmt.Errorf("gateway: bad decimal %q: %w", s, err)
	}
	return v, nil
}

func (v *Verifier) hashOrder(o SignedOrder) ([]byte, error) {
	qty, err := parseUint(o.Qty)
	if err != nil {
		return nil, fmt.Errorf("gateway: qty: %w", err)
	}
	maxNotional, err := parseUint(o.MaxNotional)
	if err != nil {
		return nil, fmt.Errorf("gateway: maxNotional: %w", err)
	}
	td := apitypes.TypedData{
		Types:       orderTypes,
		PrimaryType: "Order",
		Domain:      v.domain,
		Message: apitypes.TypedDataMessage{
			"market":      new(big.Int).SetUint64(uint64(o.Market)),
			"side":        new(big.Int).SetUint64(uint64(o.Side)),
			"type":        new(big.Int).SetUint64(uint64(o.Type)),
			"tif":         new(big.Int).SetUint64(uint64(o.TIF)),
			"price":       new(big.Int).SetUint64(o.Price),
			"qty":         qty.ToBig(),
			"maxNotional": maxNotional.ToBig(),
			"nonce":       new(big.Int).SetUint64(o.Nonce),
			"expiry":      new(big.Int).SetUint64(o.Expiry),
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	return hash, err
}

func (v *Verifier) hashCancel(c SignedCancel) ([]byte, error) {
	td := apitypes.TypedData{
		Types:       cancelTypes,
		PrimaryType: "Cancel",
		Domain:      v.domain,
		Message: apitypes.TypedDataMessage{
			"market":  new(big.Int).SetUint64(uint64(c.Market)),
			"orderId": new(big.Int).SetUint64(c.OrderID),
			"nonce":   new(big.Int).SetUint64(c.Nonce),
			"expiry":  new(big.Int).SetUint64(c.Expiry),
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	return hash, err
}

// RecoverOrder verifies the order signature and returns the signer address.
func (v *Verifier) RecoverOrder(env SignedOrderEnvelope) (common.Address, error) {
	h, err := v.hashOrder(env.Order)
	if err != nil {
		return common.Address{}, err
	}
	return recoverSigner(h, env.Signature)
}

// RecoverCancel verifies the cancel signature and returns the signer address.
func (v *Verifier) RecoverCancel(env SignedCancelEnvelope) (common.Address, error) {
	h, err := v.hashCancel(env.Cancel)
	if err != nil {
		return common.Address{}, err
	}
	return recoverSigner(h, env.Signature)
}

// recoverSigner recovers the signer of hash from a 65-byte r||s||v signature,
// normalising the v byte (27/28 -> 0/1) so both wallet and library signatures
// verify.
func recoverSigner(hash []byte, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, fmt.Errorf("gateway: signature must be 65 bytes, got %d", len(sig))
	}
	s := make([]byte, 65)
	copy(s, sig)
	if s[64] == 27 || s[64] == 28 {
		s[64] -= 27
	}
	pub, err := crypto.SigToPub(hash, s)
	if err != nil {
		return common.Address{}, fmt.Errorf("gateway: recover signer: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}

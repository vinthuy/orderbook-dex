package perpgateway

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/holiman/uint256"
)

// The perpetual domain is deliberately distinct from the spot one so a signature
// minted for a spot order can never be replayed as a perpetual order (spec
// §4.2): the domain separator is part of what gets signed.
var (
	domainTypes = apitypes.Types{
		"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		},
	}

	orderTypes = apitypes.Types{
		"EIP712Domain": domainTypes["EIP712Domain"],
		"PerpOrder": {
			{Name: "market", Type: "uint32"},
			{Name: "side", Type: "uint8"},
			{Name: "type", Type: "uint8"},
			{Name: "tif", Type: "uint8"},
			{Name: "price", Type: "uint64"},
			{Name: "qty", Type: "uint256"},
			{Name: "maxNotional", Type: "uint256"},
			{Name: "leverage", Type: "uint32"},
			{Name: "nonce", Type: "uint64"},
			{Name: "expiry", Type: "uint64"},
		},
	}

	cancelTypes = apitypes.Types{
		"EIP712Domain": domainTypes["EIP712Domain"],
		"PerpCancel": {
			{Name: "market", Type: "uint32"},
			{Name: "orderId", Type: "uint64"},
			{Name: "nonce", Type: "uint64"},
			{Name: "expiry", Type: "uint64"},
		},
	}
)

// SignedOrder is the EIP-712 payload for a perpetual place instruction.
type SignedOrder struct {
	Market      uint32 `json:"market"`
	Side        uint8  `json:"side"`
	Type        uint8  `json:"type"`
	TIF         uint8  `json:"tif"`
	Price       uint64 `json:"price"`
	Qty         string `json:"qty"`
	MaxNotional string `json:"maxNotional"`
	Leverage    uint32 `json:"leverage"`
	Nonce       uint64 `json:"nonce"`
	Expiry      uint64 `json:"expiry"`
}

// SignedOrderEnvelope is the wire form of a signed perpetual order.
type SignedOrderEnvelope struct {
	Order     SignedOrder   `json:"order"`
	Signature hexutil.Bytes `json:"signature"`
}

// SignedCancel is the EIP-712 payload for a perpetual cancel. It shares the
// account's nonce space with orders on this domain.
type SignedCancel struct {
	Market  uint32 `json:"market"`
	OrderID uint64 `json:"orderId"`
	Nonce   uint64 `json:"nonce"`
	Expiry  uint64 `json:"expiry"`
}

// SignedCancelEnvelope is the wire form of a signed perpetual cancel.
type SignedCancelEnvelope struct {
	Cancel    SignedCancel  `json:"cancel"`
	Signature hexutil.Bytes `json:"signature"`
}

// Verifier builds EIP-712 digests for the perpetual domain.
type Verifier struct {
	domain apitypes.TypedDataDomain
}

// NewVerifier builds a verifier for the given chain and domain name.
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

// ErrBadAmount is returned when an amount field is not a valid decimal string.
var ErrBadAmount = errors.New("perpgateway: bad amount")

func parseUint(s string) (*uint256.Int, error) {
	if s == "" {
		return uint256.NewInt(0), nil
	}
	v, err := uint256.FromDecimal(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrBadAmount, s)
	}
	return v, nil
}

func (v *Verifier) hashOrder(o SignedOrder) ([]byte, error) {
	qty, err := parseUint(o.Qty)
	if err != nil {
		return nil, fmt.Errorf("perpgateway: qty: %w", err)
	}
	maxNotional, err := parseUint(o.MaxNotional)
	if err != nil {
		return nil, fmt.Errorf("perpgateway: maxNotional: %w", err)
	}
	td := apitypes.TypedData{
		Types:       orderTypes,
		PrimaryType: "PerpOrder",
		Domain:      v.domain,
		Message: apitypes.TypedDataMessage{
			"market":      new(big.Int).SetUint64(uint64(o.Market)),
			"side":        new(big.Int).SetUint64(uint64(o.Side)),
			"type":        new(big.Int).SetUint64(uint64(o.Type)),
			"tif":         new(big.Int).SetUint64(uint64(o.TIF)),
			"price":       new(big.Int).SetUint64(o.Price),
			"qty":         qty.ToBig(),
			"maxNotional": maxNotional.ToBig(),
			"leverage":    new(big.Int).SetUint64(uint64(o.Leverage)),
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
		PrimaryType: "PerpCancel",
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

// OrderDigest returns the EIP-712 digest a signature must cover for this order.
// It exists so callers (and tests) can prove domain separation: the same payload
// hashes differently on the spot and perpetual domains.
func (v *Verifier) OrderDigest(env SignedOrderEnvelope) ([]byte, error) {
	return v.hashOrder(env.Order)
}

// recoverSigner recovers the signer of hash from a 65-byte r||s||v signature.
//
// IMPORTANT — what this function does and does NOT guarantee. ECDSA public-key
// recovery is not a validity check: for ANY well-formed (r, s, v) there exists a
// public key under which that signature verifies on the given hash. A garbage or
// cross-domain signature therefore always "recovers" SOME address. That address
// is not the attacker's target, but some essentially random key.
//
// Security consequently rests on three things, none of which is this function:
//   - the recovered address OWNS the resources being spent (an attacker's random
//     address owns nothing, so the order fails on funding),
//   - the EIP-712 domain differs from the spot domain, so a signature harvested
//     from the spot book recovers a different address here,
//   - the per-account nonce is persisted, so a captured perp signature cannot be
//     replayed.
//
// What this function adds is defence in depth: it rejects structurally malformed
// input (bad length, v outside {0,1}, zero or out-of-range r/s, high-s
// malleability) so such blobs fail early and loudly instead of resolving to a
// meaningless address.
func recoverSigner(hash []byte, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, fmt.Errorf("perpgateway: signature must be 65 bytes, got %d", len(sig))
	}
	s := make([]byte, 65)
	copy(s, sig)
	if s[64] == 27 || s[64] == 28 {
		s[64] -= 27
	}
	if s[64] > 1 {
		return common.Address{}, fmt.Errorf("perpgateway: invalid signature v byte %d", sig[64])
	}

	rInt := new(big.Int).SetBytes(s[:32])
	sInt := new(big.Int).SetBytes(s[32:64])
	if rInt.Sign() == 0 || sInt.Sign() == 0 {
		return common.Address{}, errors.New("perpgateway: signature r or s is zero")
	}
	if rInt.Cmp(secp256k1N) >= 0 || sInt.Cmp(secp256k1N) >= 0 {
		return common.Address{}, errors.New("perpgateway: signature r or s out of range")
	}
	if sInt.Cmp(halfSecp256k1N) > 0 {
		return common.Address{}, errors.New("perpgateway: signature s is not in the lower half")
	}

	pub, err := crypto.SigToPub(hash, s)
	if err != nil {
		return common.Address{}, fmt.Errorf("perpgateway: recover signer: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// secp256k1N is the order of the secp256k1 curve; halfN is the EIP-2 upper bound
// on s.
var (
	secp256k1N, _    = new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)
	halfSecp256k1N   = new(big.Int).Rsh(new(big.Int).Set(secp256k1N), 1)
)

var _ = ecdsa.Verify

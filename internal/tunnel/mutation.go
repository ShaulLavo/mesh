// Package tunnel implements named, signed reverse tunnel reservations.
package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/shaul/mesh/internal/serve"
)

const (
	Domain                = "mesh/tunnel-claim/v1"
	MaximumClaimsPerKey   = 32
	MaximumClaimants      = 4096
	MaximumRateEntries    = 4096
	MaximumFrameBytes     = 4096
	MaximumForwardsPerKey = 8
)

type Action string

const (
	Create  Action = "create"
	Release Action = "release"
)

var (
	ErrNotFound         = errors.New("tunnel: claim or sequence not found")
	ErrCollision        = errors.New("tunnel: hostname is already claimed")
	ErrStaleSequence    = errors.New("tunnel: stale mutation sequence")
	ErrSequenceConflict = errors.New("tunnel: sequence has a different digest")
	ErrCapacity         = errors.New("tunnel: reservation capacity reached")
	ErrActive           = errors.New("tunnel: disconnect the active forward before releasing its claim")
	ErrUnauthorized     = errors.New("tunnel: claiming key is not currently authorized")
	ErrRateLimited      = errors.New("tunnel: claim frame rate limit exceeded")
)

type Mutation struct {
	Action     Action `json:"action"`
	TargetID   string `json:"targetId"`
	ClaimantID string `json:"claimantId"`
	Sequence   uint64 `json:"sequence"`
	PublicName string `json:"publicName"`
	Signature  []byte `json:"signature"`
}

// Ack also identifies a definitive refusal, so a failed create cannot strand
// an owner's release behind it. Ambiguous persistence errors have no receipt.
type Ack struct {
	Sequence uint64 `json:"sequence"`
	Digest   string `json:"digest"`
	Error    string `json:"error,omitempty"`
}

type Claim struct {
	PublicName string
	ClaimantID string
}

type StateStore interface {
	TunnelVersion(context.Context, string) (Ack, error)
	TunnelClaim(context.Context, string) (Claim, error)
	ApplyTunnelMutation(context.Context, Mutation, string) error
	DeleteTunnelClaim(context.Context, string) error
}

func KeyID(key ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(key) }

func PublicKey(id string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(key) != ed25519.PublicKeySize || KeyID(key) != id {
		return nil, errors.New("tunnel: identity is not a canonical Ed25519 key ID")
	}
	return ed25519.PublicKey(key), nil
}

func ValidateHostname(name string) error {
	if name == "" {
		return errors.New("tunnel: full public hostname is required")
	}
	if err := serve.ValidatePublicName(name); err != nil {
		return fmt.Errorf("tunnel: full public hostname: %w", err)
	}
	return nil
}

// Canonical encodes only the domain facts, never their transport representation.
func Canonical(m Mutation) ([]byte, error) {
	if m.Action != Create && m.Action != Release {
		return nil, errors.New("tunnel: mutation action must be create or release")
	}
	if _, err := PublicKey(m.TargetID); err != nil {
		return nil, fmt.Errorf("tunnel: target identity: %w", err)
	}
	if _, err := PublicKey(m.ClaimantID); err != nil {
		return nil, fmt.Errorf("tunnel: claimant identity: %w", err)
	}
	if m.Sequence == 0 || m.Sequence > math.MaxInt64 {
		return nil, errors.New("tunnel: sequence is outside 1..MaxInt64")
	}
	if err := ValidateHostname(m.PublicName); err != nil {
		return nil, err
	}
	var encoded []byte
	for _, value := range []string{Domain, string(m.Action), m.TargetID, m.ClaimantID} {
		encoded = appendString(encoded, value)
	}
	encoded = binary.BigEndian.AppendUint64(encoded, m.Sequence)
	encoded = appendString(encoded, m.PublicName)
	if len(encoded)+ed25519.SignatureSize > MaximumFrameBytes {
		return nil, errors.New("tunnel: canonical mutation exceeds 4 KiB")
	}
	return encoded, nil
}

func appendString(dst []byte, value string) []byte {
	dst = binary.BigEndian.AppendUint64(dst, uint64(len(value)))
	return append(dst, value...)
}

func Sign(key ed25519.PrivateKey, targetID string, action Action, publicName string, sequence uint64) (Mutation, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return Mutation{}, errors.New("tunnel: invalid signing key")
	}
	m := Mutation{Action: action, TargetID: targetID, ClaimantID: KeyID(key.Public().(ed25519.PublicKey)), Sequence: sequence, PublicName: publicName}
	encoded, err := Canonical(m)
	if err != nil {
		return Mutation{}, err
	}
	digest := sha256.Sum256(encoded)
	m.Signature = ed25519.Sign(key, digest[:])
	return m, nil
}

func Verify(m Mutation, targetID string) (string, error) {
	encoded, err := Canonical(m)
	if err != nil {
		return "", err
	}
	if m.TargetID != targetID {
		return "", errors.New("tunnel: mutation names another edge identity")
	}
	key, err := PublicKey(m.ClaimantID)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	if !ed25519.Verify(key, digest[:], m.Signature) {
		return "", errors.New("tunnel: invalid owner signature")
	}
	return hex.EncodeToString(digest[:]), nil
}

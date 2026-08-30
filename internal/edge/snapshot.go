// Package edge owns authenticated public-route registration and proxying.
package edge

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/serve"
)

const (
	snapshotSignatureDomain = "mesh/edge-route-snapshot/v1"
	MaximumRoutes           = 256
	maximumSnapshotBytes    = 1 << 20
	maximumServiceNameBytes = 512
	minimumSnapshotTTL      = 30 * time.Second
	maximumSnapshotTTL      = 15 * time.Minute
	maximumFutureSkew       = 2 * time.Minute
)

// Route is one complete public claim. The origin endpoint is deliberately not
// representable here; the edge derives it from its pinned allowlist.
type Route struct {
	PublicName    string
	ServiceName   string
	WakeOnRequest bool
}

// Snapshot is one origin's complete signed desired state for one exact edge.
type Snapshot struct {
	TargetID  string
	OriginID  string
	Sequence  uint64
	IssuedAt  time.Time
	ExpiresAt time.Time
	Routes    []Route
	Signature []byte
}

// NewSnapshot builds a canonical unsigned snapshot and sorts a private copy of
// routes. SignSnapshot still validates every field before signing.
func NewSnapshot(targetID, originID string, sequence uint64, issuedAt, expiresAt time.Time, routes []Route) Snapshot {
	routes = append([]Route(nil), routes...)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].PublicName != routes[j].PublicName {
			return routes[i].PublicName < routes[j].PublicName
		}
		return routes[i].ServiceName < routes[j].ServiceName
	})
	return Snapshot{
		TargetID: targetID, OriginID: originID, Sequence: sequence,
		IssuedAt: issuedAt.UTC(), ExpiresAt: expiresAt.UTC(), Routes: routes,
	}
}

// SignSnapshot signs a canonical complete snapshot with the origin identity.
func SignSnapshot(snapshot Snapshot, signer ed25519.PrivateKey, now time.Time) (Snapshot, error) {
	if len(signer) != ed25519.PrivateKeySize {
		return Snapshot{}, errors.New("edge: snapshot signer is not an Ed25519 private key")
	}
	wantOrigin := base64.RawURLEncoding.EncodeToString(signer.Public().(ed25519.PublicKey))
	if snapshot.OriginID != wantOrigin {
		return Snapshot{}, errors.New("edge: snapshot origin does not match signer identity")
	}
	digest, err := snapshotDigest(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if err := ValidateFresh(snapshot, now); err != nil {
		return Snapshot{}, err
	}
	snapshot.Routes = append([]Route(nil), snapshot.Routes...)
	snapshot.Signature = ed25519.Sign(signer, digest[:])
	return snapshot, nil
}

// VerifySnapshot validates the complete boundary object, its exact pins, and
// its signature. It returns the canonical digest used for idempotent acks.
func VerifySnapshot(snapshot Snapshot, targetID, originID string) (string, error) {
	if snapshot.TargetID != targetID {
		return "", errors.New("edge: snapshot targets a different edge")
	}
	if snapshot.OriginID != originID {
		return "", errors.New("edge: snapshot names a different origin")
	}
	if len(snapshot.Signature) != ed25519.SignatureSize {
		return "", fmt.Errorf("edge: snapshot signature size %d, want %d", len(snapshot.Signature), ed25519.SignatureSize)
	}
	publicKey, err := parseIdentity("origin", originID)
	if err != nil {
		return "", err
	}
	digest, err := snapshotDigest(snapshot)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(publicKey, digest[:], snapshot.Signature) {
		return "", errors.New("edge: snapshot signature is invalid")
	}
	return hex.EncodeToString(digest[:]), nil
}

func snapshotDigest(snapshot Snapshot) ([sha256.Size]byte, error) {
	if _, err := parseIdentity("edge target", snapshot.TargetID); err != nil {
		return [sha256.Size]byte{}, err
	}
	if _, err := parseIdentity("origin", snapshot.OriginID); err != nil {
		return [sha256.Size]byte{}, err
	}
	if snapshot.Sequence == 0 || snapshot.Sequence > math.MaxInt64 {
		return [sha256.Size]byte{}, errors.New("edge: snapshot sequence is outside 1..MaxInt64")
	}
	issuedAt := snapshot.IssuedAt.UTC()
	expiresAt := snapshot.ExpiresAt.UTC()
	if issuedAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(issuedAt) {
		return [sha256.Size]byte{}, errors.New("edge: snapshot expiry must follow its issue time")
	}
	ttl := expiresAt.Sub(issuedAt)
	if ttl < minimumSnapshotTTL || ttl > maximumSnapshotTTL {
		return [sha256.Size]byte{}, fmt.Errorf("edge: snapshot TTL %s is outside [%s,%s]", ttl, minimumSnapshotTTL, maximumSnapshotTTL)
	}
	if len(snapshot.Routes) > MaximumRoutes {
		return [sha256.Size]byte{}, fmt.Errorf("edge: snapshot route count %d exceeds %d", len(snapshot.Routes), MaximumRoutes)
	}

	hasher := sha256.New()
	writeField(hasher, []byte(snapshotSignatureDomain))
	writeField(hasher, []byte(snapshot.TargetID))
	writeField(hasher, []byte(snapshot.OriginID))
	writeUint64(hasher, snapshot.Sequence)
	writeInt64(hasher, issuedAt.UnixNano())
	writeInt64(hasher, expiresAt.UnixNano())
	writeUint64(hasher, uint64(len(snapshot.Routes)))
	encodedSize := 8 + len(snapshotSignatureDomain) + 8 + len(snapshot.TargetID) + 8 + len(snapshot.OriginID) + 32
	var prior Route
	for index, route := range snapshot.Routes {
		if err := validateRoute(route); err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("edge: snapshot route %d: %w", index, err)
		}
		if index > 0 && !routeLess(prior, route) {
			return [sha256.Size]byte{}, errors.New("edge: snapshot routes are not strictly sorted and unique")
		}
		encodedSize += len(route.PublicName) + len(route.ServiceName) + 25
		if encodedSize > maximumSnapshotBytes {
			return [sha256.Size]byte{}, fmt.Errorf("edge: snapshot canonical payload exceeds %d bytes", maximumSnapshotBytes)
		}
		writeField(hasher, []byte(route.PublicName))
		writeField(hasher, []byte(route.ServiceName))
		if route.WakeOnRequest {
			writeField(hasher, []byte{1})
		} else {
			writeField(hasher, []byte{0})
		}
		prior = route
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

// ValidateFresh applies ingress-only clock checks to a new sequence. Equal
// authenticated retries deliberately skip this check so lost acknowledgements
// remain recoverable without refreshing liveness.
func ValidateFresh(snapshot Snapshot, now time.Time) error {
	now = now.UTC()
	if snapshot.IssuedAt.UTC().After(now.Add(maximumFutureSkew)) {
		return errors.New("edge: snapshot issue time is too far in the future")
	}
	if !snapshot.ExpiresAt.UTC().After(now) {
		return errors.New("edge: snapshot has expired")
	}
	return nil
}

func validateRoute(route Route) error {
	if route.PublicName == "" {
		return errors.New("public name is empty")
	}
	if err := serve.ValidatePublicName(route.PublicName); err != nil {
		return err
	}
	if len(route.ServiceName) > maximumServiceNameBytes {
		return fmt.Errorf("service name exceeds %d bytes", maximumServiceNameBytes)
	}
	if err := serve.ValidateName(route.ServiceName); err != nil {
		return err
	}
	if route.ServiceName == "mesh" || strings.HasPrefix(route.ServiceName, "mesh/") {
		return errors.New("service name overlaps the reserved terminal path")
	}
	return nil
}

func routeLess(left, right Route) bool {
	return left.PublicName < right.PublicName || left.PublicName == right.PublicName && left.ServiceName < right.ServiceName
}

func parseIdentity(label, value string) (ed25519.PublicKey, error) {
	if len(value) != 43 || strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("edge: %s identity is empty or not canonical", label)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("edge: %s identity is not a canonical Ed25519 public key", label)
	}
	return ed25519.PublicKey(decoded), nil
}

func writeField(writer hash.Hash, value []byte) {
	writeUint64(writer, uint64(len(value)))
	_, _ = writer.Write(value)
}

func writeUint64(writer hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeInt64(writer hash.Hash, value int64) {
	writeUint64(writer, uint64(value)) //nolint:gosec // canonical hashing preserves the signed value's two's-complement bits
}

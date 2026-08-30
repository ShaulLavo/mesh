package edge

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

const (
	listProofSignatureDomain = "mesh/edge-list-request/v1"
	maximumListProofAge      = 2 * time.Minute
)

// ListProof authenticates one exact edge.list page without exposing the
// origin identity private key or trusting a caller-supplied display name.
type ListProof struct {
	TargetID  string
	OriginID  string
	IssuedAt  time.Time
	Signature []byte
}

func signListProof(targetID, originID, requestID, cursor string, limit int, issuedAt time.Time, signer ed25519.PrivateKey) (ListProof, error) {
	if len(signer) != ed25519.PrivateKeySize {
		return ListProof{}, errors.New("edge: list signer is not an Ed25519 private key")
	}
	proof := ListProof{TargetID: targetID, OriginID: originID, IssuedAt: issuedAt.UTC()}
	digest, err := listProofDigest(proof, requestID, cursor, limit)
	if err != nil {
		return ListProof{}, err
	}
	proof.Signature = ed25519.Sign(signer, digest[:])
	return proof, nil
}

func verifyListProof(proof ListProof, requestID, cursor string, limit int, targetID string, now time.Time) (string, error) {
	if proof.TargetID != targetID {
		return "", errors.New("edge: list proof targets a different edge")
	}
	if len(proof.Signature) != ed25519.SignatureSize {
		return "", errors.New("edge: list proof signature size is invalid")
	}
	digest, err := listProofDigest(proof, requestID, cursor, limit)
	if err != nil {
		return "", err
	}
	publicKey, err := parseIdentity("list origin", proof.OriginID)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(publicKey, digest[:], proof.Signature) {
		return "", errors.New("edge: list proof signature is invalid")
	}
	now = now.UTC()
	if proof.IssuedAt.After(now.Add(maximumFutureSkew)) || proof.IssuedAt.Before(now.Add(-maximumListProofAge)) {
		return "", errors.New("edge: list proof time is outside the accepted window")
	}
	return hex.EncodeToString(digest[:]), nil
}

func listProofDigest(proof ListProof, requestID, cursor string, limit int) ([sha256.Size]byte, error) {
	if _, err := parseIdentity("edge target", proof.TargetID); err != nil {
		return [sha256.Size]byte{}, err
	}
	if _, err := parseIdentity("list origin", proof.OriginID); err != nil {
		return [sha256.Size]byte{}, err
	}
	if err := validateEdgeRequestID(requestID); err != nil {
		return [sha256.Size]byte{}, err
	}
	if len(cursor) > maximumListCursorLength {
		return [sha256.Size]byte{}, errors.New("edge: list cursor is too long")
	}
	if limit < 1 || limit > maximumListLimit {
		return [sha256.Size]byte{}, fmt.Errorf("edge: list limit %d is outside 1..%d", limit, maximumListLimit)
	}
	if proof.IssuedAt.IsZero() {
		return [sha256.Size]byte{}, errors.New("edge: list proof has no issue time")
	}
	hasher := sha256.New()
	writeField(hasher, []byte(listProofSignatureDomain))
	writeField(hasher, []byte(proof.TargetID))
	writeField(hasher, []byte(proof.OriginID))
	writeField(hasher, []byte(requestID))
	writeField(hasher, []byte(cursor))
	writeUint64(hasher, uint64(limit))
	writeInt64(hasher, proof.IssuedAt.UTC().UnixNano())
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func listProofToProtocol(proof ListProof) protocol.EdgeListProof {
	return protocol.EdgeListProof{
		TargetID: proof.TargetID, OriginID: proof.OriginID, IssuedAt: proof.IssuedAt,
		Signature: append([]byte(nil), proof.Signature...),
	}
}

func listProofFromProtocol(proof protocol.EdgeListProof) ListProof {
	return ListProof{
		TargetID: proof.TargetID, OriginID: proof.OriginID, IssuedAt: proof.IssuedAt,
		Signature: append([]byte(nil), proof.Signature...),
	}
}

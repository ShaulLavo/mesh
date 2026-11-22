// Package appcaller identifies application callers: separately configured
// agent frontends that share one device key or local account.
//
// A frontend owns a 256-bit random credential in a private file on its own
// device. It sends the encoded credential in protocol.Control.Caller over an
// already authenticated Mesh connection or the daemon's local socket. The
// owning host never stores the credential. It derives the application ID with
// Derive from the credential and the authenticated namespace of the
// connection, so the same credential presented by another device or account
// names a different caller.
//
// Frontend side:
//
//	id, err := appcaller.Load(configDir, "codex")
//	request.Caller = id.Credential.Encode()
//
// Host side (package daemon): applicationCaller(ctx, request) returns the
// verified Caller, and Caller.Owns compares it with a persisted owner in
// constant time.
package appcaller

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CredentialBytes is the size of the random caller secret.
const CredentialBytes = 32

// credentialPrefix versions the encoding so a pasted value is recognizable.
const credentialPrefix = "mac1_"

// ErrInvalidCredential reports an encoded credential that cannot be parsed.
var ErrInvalidCredential = errors.New("application credential is malformed")

// Credential is one caller secret. The bytes sit behind a pointer, so a
// credential formatted into a log or an error by accident prints an address,
// never the secret.
type Credential struct{ raw *[CredentialBytes]byte }

var zeroCredential [CredentialBytes]byte

// Generate creates a fresh random credential.
func Generate() (Credential, error) {
	c := Credential{raw: new([CredentialBytes]byte)}
	if _, err := rand.Read(c.raw[:]); err != nil {
		return Credential{}, fmt.Errorf("generate application credential: %w", err)
	}
	return c, nil
}

// ParseCredential decodes the wire form produced by Encode.
func ParseCredential(encoded string) (Credential, error) {
	body, ok := strings.CutPrefix(encoded, credentialPrefix)
	if !ok {
		return Credential{}, ErrInvalidCredential
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil || len(raw) != CredentialBytes {
		return Credential{}, ErrInvalidCredential
	}
	c := Credential{raw: new([CredentialBytes]byte)}
	copy(c.raw[:], raw)
	if c.IsZero() {
		return Credential{}, ErrInvalidCredential
	}
	return c, nil
}

// Encode returns the form carried in protocol.Control.Caller and stored in
// the identity file. The unset credential encodes as empty.
func (c Credential) Encode() string {
	if c.IsZero() {
		return ""
	}
	return credentialPrefix + base64.RawURLEncoding.EncodeToString(c.raw[:])
}

// IsZero reports the unset credential.
func (c Credential) IsZero() bool { return c.raw == nil || *c.raw == zeroCredential }

// Fingerprint is a short local label for listing identities. It is a
// separate digest from the application ID, so it reveals neither the secret
// nor the ID a host derives.
func (c Credential) Fingerprint() string {
	if c.IsZero() {
		return ""
	}
	sum := sha256.Sum256(append([]byte("mesh application credential fingerprint\x00"), c.raw[:]...))
	return hex.EncodeToString(sum[:6])
}

// Scope is the authenticated namespace of the connection that carried a
// credential: the daemon's own account over the local socket, or one pinned
// device identity over mutual TLS.
type Scope struct {
	Class     string
	Principal string
}

// Scope classes match execution.Key classes for the same connections.
const (
	ScopeLocalAccount = "local-account"
	ScopeDevice       = "device"
)

func (s Scope) validate() error {
	if s.Principal == "" || (s.Class != ScopeLocalAccount && s.Class != ScopeDevice) {
		return fmt.Errorf("application caller: unauthenticated scope %q", s.Class)
	}
	return nil
}

// ID is the hex SHA-256 application ID a host derives for one credential in
// one scope. It is safe to persist and to show; it is not a secret and grants
// nothing without the credential.
type ID string

// Derive computes the application ID. Length-prefixed fields keep the scope
// and the secret from running together.
func Derive(scope Scope, c Credential) (ID, error) {
	if err := scope.validate(); err != nil {
		return "", err
	}
	if c.IsZero() {
		return "", ErrInvalidCredential
	}
	h := sha256.New()
	for _, field := range [][]byte{[]byte("mesh application caller v1"), []byte(scope.Class), []byte(scope.Principal), c.raw[:]} {
		_, _ = fmt.Fprintf(h, "%d:", len(field))
		_, _ = h.Write(field)
	}
	return ID(hex.EncodeToString(h.Sum(nil))), nil
}

// Valid reports a well-formed ID.
func (id ID) Valid() bool {
	raw, err := hex.DecodeString(string(id))
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == string(id)
}

// Equal compares two IDs in constant time.
func (id ID) Equal(other ID) bool {
	return subtle.ConstantTimeCompare([]byte(id), []byte(other)) == 1 && id.Valid()
}

// Caller is a verified application caller on the owning host.
type Caller struct {
	ID    ID
	Scope Scope
}

// Owns reports whether a persisted owner ID names this caller. The
// comparison is constant time.
func (c Caller) Owns(owner string) bool { return c.ID.Equal(ID(owner)) }

// Verify parses an encoded credential and derives its caller in scope.
func Verify(scope Scope, encoded string) (Caller, error) {
	credential, err := ParseCredential(encoded)
	if err != nil {
		return Caller{}, err
	}
	id, err := Derive(scope, credential)
	if err != nil {
		return Caller{}, err
	}
	return Caller{ID: id, Scope: scope}, nil
}

// Package apps implements disposable websites with owner authorization and traffic expiry.
package apps

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/webauth"
)

const managementLabel = "apps"

func Domain() string           { return domainpolicy.Primary() }
func ManagementHost() string   { return managementLabel + "." + Domain() }
func ManagementOrigin() string { return "https://" + ManagementHost() }

const IdleTTL = 24 * time.Hour

// LeaseTTL is a variable only so integration builds can let a lease lapse
// within a script's time budget.
var LeaseTTL = 3 * time.Minute

const MaxArchive = 64 << 20
const ChunkSize = 256 << 10

var idPattern = regexp.MustCompile(`^[0123456789abcdefghjkmnpqrstvwxyz]{4}$`)

func ValidID(id string) bool {
	return storedID(id) && id != managementLabel && !serve.ReservedLabel(id)
}

// Stored IDs predate allocation reservations; syntax still confines their filesystem paths.
func storedID(id string) bool { return idPattern.MatchString(id) }

func URL(id string) string { return "https://" + id + "." + Domain() }

type Request struct {
	Action    string   `json:"action"`
	ID        string   `json:"id,omitempty"`
	Path      string   `json:"path,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Command   string   `json:"command,omitempty"`
	Setup     string   `json:"setup,omitempty"`
	Port      int      `json:"port,omitempty"`
	Env       []string `json:"env,omitempty"`
	UploadID  string   `json:"uploadId,omitempty"`
	Data      []byte   `json:"data,omitempty"`
	Digest    string   `json:"digest,omitempty"`
	Code      string   `json:"code,omitempty"`
	BrowserID string   `json:"browserId,omitempty"`
	Offset    int64    `json:"offset,omitempty"`
}

type Record struct {
	Revision   string    `json:"revision,omitempty"`
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Cleanup    string    `json:"cleanup"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Generation uint64    `json:"generation"`
	Ready      bool      `json:"ready"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
}

type RuntimeInfo struct {
	Phase     string        `json:"phase"`
	SessionID string        `json:"sessionId,omitempty"`
	Command   string        `json:"command,omitempty"`
	Port      int           `json:"port,omitempty"`
	Root      string        `json:"root,omitempty"`
	Failure   *SetupFailure `json:"failure,omitempty"`
	// Problem is why a server app stopped being served, such as a listener
	// beyond loopback or a port another process holds.
	Problem string `json:"problem,omitempty"`
}

// SetupFailure is the owner's account of an app's last failed setup. It outlives
// the workspace and worker it describes, so an owner can still learn why a
// create failed after the app is gone.
type SetupFailure struct {
	UploadID  string    `json:"uploadId"`
	Error     string    `json:"error"`
	Output    string    `json:"output,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type Result struct {
	Pairing  *webauth.PairingInfo `json:"pairing,omitempty"`
	Runtime  *RuntimeInfo         `json:"runtime,omitempty"`
	App      *Record              `json:"app,omitempty"`
	Apps     []Record             `json:"apps,omitempty"`
	UploadID string               `json:"uploadId,omitempty"`
	Data     []byte               `json:"data,omitempty"`
	Done     bool                 `json:"done,omitempty"`
	Browsers json.RawMessage      `json:"browsers,omitempty"`
}

type StateStore interface {
	LoadAppState(context.Context, string) ([]byte, error)
	SaveAppState(context.Context, string, []byte) error
}
type NameStore interface {
	StateStore
	ReserveAppNames(context.Context, []string, string) error
	AppNameExists(context.Context, string) (bool, error)
	SetAppNameInactive(context.Context, string, string) error
}

type Signed struct {
	Domain    string          `json:"domain"`
	Target    string          `json:"target"`
	Owner     string          `json:"owner"`
	ID        string          `json:"id"`
	Sequence  uint64          `json:"sequence"`
	IssuedAt  time.Time       `json:"issuedAt"`
	Body      json.RawMessage `json:"body"`
	Signature string          `json:"signature,omitempty"`
}

func Sign(domain, target string, sequence uint64, body any, key ed25519.PrivateKey, now time.Time) (Signed, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Signed{}, errors.New("app: invalid signing key")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Signed{}, err
	}
	id, err := RandomToken()
	if err != nil {
		return Signed{}, err
	}
	s := Signed{Domain: domain, Target: target, Owner: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), ID: id, Sequence: sequence, IssuedAt: now.UTC(), Body: payload}
	data, err := json.Marshal(s)
	if err != nil {
		return Signed{}, err
	}
	s.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, data))
	return s, nil
}
func (s Signed) Verify(domain, target, owner string, now time.Time) error {
	if s.Domain != domain || s.Target != target || s.Owner != owner || len(s.ID) != 43 || len(s.Body) > 1<<20 {
		return errors.New("app: invalid signed envelope")
	}
	if s.IssuedAt.Before(now.Add(-2*time.Minute)) || s.IssuedAt.After(now.Add(30*time.Second)) {
		return errors.New("app: expired signed envelope")
	}
	pub, err := base64.RawURLEncoding.DecodeString(owner)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("app: invalid signer")
	}
	sig, err := base64.RawURLEncoding.DecodeString(s.Signature)
	if err != nil {
		return err
	}
	s.Signature = ""
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, data, sig) {
		return errors.New("app: bad signature")
	}
	return nil
}
func RandomToken() (string, error) {
	var b [32]byte
	_, err := rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:]), err
}
func digestBytes(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func save(ctx context.Context, s StateStore, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.SaveAppState(ctx, key, b)
}
func load(ctx context.Context, s StateStore, key string, value any) error {
	b, err := s.LoadAppState(ctx, key)
	if err != nil || len(b) == 0 {
		return err
	}
	return json.Unmarshal(b, value)
}
func decode(raw []byte, value any) error {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(raw), 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("app: decode: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("app: trailing JSON data")
	}
	return nil
}

func refresh(s Signed, key ed25519.PrivateKey, now time.Time) (Signed, error) {
	s.IssuedAt = now.UTC()
	s.Signature = ""
	b, err := json.Marshal(s)
	if err != nil {
		return Signed{}, err
	}
	s.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
	return s, nil
}

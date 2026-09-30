// Package webauth pairs browsers with exact Mesh owner identities.
package webauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	PairCookie         = "__Host-mesh-pair"
	OwnerCookie        = "__Host-mesh-owner"
	ViewCookie         = "__Host-mesh-view"
	stateKey           = "browser-auth-v1"
	pairTTL            = 10 * time.Minute
	sessionTTL         = 30 * 24 * time.Hour
	ticketTTL          = time.Minute
	viewTTL            = 12 * time.Hour
	maxPairs           = 256
	maxPendingPairs    = 256
	maxAggregatePairs  = 16
	maxAggregateStarts = 32
	maxSourcePairs     = 4
	maxSourceStarts    = 8
	maxPairSources     = 1024
	maxBrowsers        = 1024
	maxTickets         = 1024
	maxViews           = 4096
	maxOwners          = 64
	maxStateBytes      = 4 << 20
)

var (
	ErrUnauthorized    = errors.New("webauth: owner authentication required")
	ErrPairing         = errors.New("webauth: pairing code is invalid, expired, or already used")
	ErrApprovalPending = fmt.Errorf("%w: awaiting owner approval", ErrPairing)
	ErrCapacity        = errors.New("webauth: browser authentication capacity reached")
	ErrRateLimited     = errors.New("webauth: too many pairing approval attempts")
	ErrMutation        = errors.New("webauth: invalid management request origin or CSRF token")
)

type StateStore interface {
	LoadAppState(context.Context, string) ([]byte, error)
	SaveAppState(context.Context, string, []byte) error
}

type Pairing struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type Session struct {
	ID        string    `json:"id"`
	Owners    []string  `json:"owners"`
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s Session) Owns(owner string) bool { return slices.Contains(s.Owners, owner) }

type BrowserInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type PairingInfo struct {
	UserAgent  string    `json:"userAgent"`
	SourceIP   string    `json:"sourceIP"`
	CreatedAt  time.Time `json:"createdAt"`
	AgeSeconds int64     `json:"ageSeconds"`
}

type pairRecord struct {
	PairingInfo
	Code       string    `json:"-"`
	Source     string    `json:"source,omitempty"`
	Aggregate  string    `json:"aggregate,omitempty"`
	CodeHash   string    `json:"codeHash"`
	ExpiresAt  time.Time `json:"expiresAt"`
	ExistingID string    `json:"existingId,omitempty"`
	Owner      string    `json:"owner,omitempty"`
}
type browserRecord struct {
	Session
	CreatedAt time.Time `json:"createdAt"`
}
type viewRecord struct {
	BrowserID, Owner, AppID string
	ExpiresAt               time.Time
}
type state struct {
	Version          int                      `json:"version"`
	Pairs            map[string]pairRecord    `json:"pairs"`
	Browsers         map[string]browserRecord `json:"browsers"`
	Tickets          map[string]viewRecord    `json:"tickets"`
	Views            map[string]viewRecord    `json:"views"`
	ApprovalWindow   time.Time                `json:"approvalWindow"`
	ApprovalAttempts int                      `json:"approvalAttempts"`
}
type sourceWindow struct {
	LastUsedAt time.Time
	StartedAt  time.Time
	Issued     int
}

type Service struct {
	sources map[string]sourceWindow
	mu      sync.Mutex
	store   StateStore
	now     func() time.Time
	state   state
}

func New(store StateStore, now func() time.Time) (*Service, error) {
	if store == nil || now == nil {
		return nil, errors.New("webauth: state store and clock are required")
	}
	raw, err := store.LoadAppState(context.Background(), stateKey)
	if err != nil {
		return nil, fmt.Errorf("webauth: load browser state: %w", err)
	}
	initial := state{Version: 1, Pairs: map[string]pairRecord{}, Browsers: map[string]browserRecord{}, Tickets: map[string]viewRecord{}, Views: map[string]viewRecord{}}
	if len(raw) > maxStateBytes {
		return nil, ErrCapacity
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &initial); err != nil {
			return nil, fmt.Errorf("webauth: decode browser state: %w", err)
		}
	}
	if initial.Version != 1 || initial.Pairs == nil || initial.Browsers == nil || initial.Tickets == nil || initial.Views == nil {
		return nil, errors.New("webauth: unsupported or incomplete browser state")
	}
	if len(initial.Pairs) > maxPairs || len(initial.Browsers) > maxBrowsers || len(initial.Tickets) > maxTickets || len(initial.Views) > maxViews {
		return nil, ErrCapacity
	}
	// Older releases persisted unapproved pairs. A restart must drop that anonymous occupancy.
	for key, pair := range initial.Pairs {
		if pair.Owner == "" {
			delete(initial.Pairs, key)
		}
	}
	initial.prune(now().UTC())
	return &Service{store: store, now: now, state: initial, sources: map[string]sourceWindow{}}, nil
}

func (s *Service) change(ctx context.Context, apply func(*state, time.Time) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	var next state
	if err = json.Unmarshal(raw, &next); err != nil {
		return err
	}
	// Keep the display code in memory without ever serializing it to the store.
	next.Pairs = maps.Clone(s.state.Pairs)
	now := s.now().UTC()
	next.prune(now)
	operationErr := apply(&next, now)
	// Rejected approval attempts still consume their durable rate limit.
	durable := next
	durable.Pairs = maps.Clone(next.Pairs)
	for key, pair := range durable.Pairs {
		if pair.Owner == "" {
			delete(durable.Pairs, key)
		}
	}
	raw, err = json.Marshal(durable)
	if err != nil {
		return err
	}
	if len(raw) > maxStateBytes {
		return ErrCapacity
	}
	if err = s.store.SaveAppState(ctx, stateKey, raw); err != nil {
		return fmt.Errorf("webauth: persist browser state: %w", err)
	}
	s.state = next
	return operationErr
}
func (d *state) prune(now time.Time) {
	for key, p := range d.Pairs {
		if !now.Before(p.ExpiresAt) {
			delete(d.Pairs, key)
		}
	}
	for key, b := range d.Browsers {
		if !now.Before(b.ExpiresAt) {
			delete(d.Browsers, key)
		}
	}
	for key, t := range d.Tickets {
		if !now.Before(t.ExpiresAt) {
			delete(d.Tickets, key)
		}
	}
	for key, v := range d.Views {
		if !now.Before(v.ExpiresAt) {
			delete(d.Views, key)
		}
	}
}
func bearer() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func cookieKey(r *http.Request, name string) (string, error) {
	var token string
	for _, c := range r.Cookies() {
		if c.Name != name {
			continue
		}
		if token != "" || len(c.Value) != 43 {
			return "", ErrUnauthorized
		}
		token = c.Value
	}
	if token == "" {
		return "", ErrUnauthorized
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(decoded) != 32 {
		return "", ErrUnauthorized
	}
	return hash(token), nil
}
func writeCookie(w http.ResponseWriter, name, token string, expires time.Time, now time.Time) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: max(1, int(expires.Sub(now).Seconds()))})
}
func validOwner(owner string) bool {
	return owner != "" && len(owner) <= 256 && strings.TrimSpace(owner) == owner && !strings.ContainsAny(owner, "\r\n\x00")
}
func browserIn(d *state, r *http.Request, now time.Time) (browserRecord, error) {
	key, err := cookieKey(r, OwnerCookie)
	if err != nil {
		return browserRecord{}, err
	}
	b, ok := d.Browsers[key]
	if !ok || !now.Before(b.ExpiresAt) || len(b.Owners) == 0 {
		return browserRecord{}, ErrUnauthorized
	}
	b.Owners = slices.Clone(b.Owners)
	return b, nil
}
func browserByID(d *state, id string, now time.Time) (browserRecord, error) {
	for _, b := range d.Browsers {
		if b.ID == id && now.Before(b.ExpiresAt) && len(b.Owners) > 0 {
			return b, nil
		}
	}
	return browserRecord{}, ErrUnauthorized
}

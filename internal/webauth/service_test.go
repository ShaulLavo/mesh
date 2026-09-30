package webauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	raw  []byte
	fail bool
}

func (m *memoryStore) LoadAppState(context.Context, string) ([]byte, error) {
	return append([]byte(nil), m.raw...), nil
}
func (m *memoryStore) SaveAppState(_ context.Context, _ string, raw []byte) error {
	if m.fail {
		return errors.New("disk unavailable")
	}
	m.raw = append([]byte(nil), raw...)
	return nil
}
func fixture(t *testing.T) (*Service, *memoryStore, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	store := &memoryStore{}
	s, err := New(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return s, store, &now
}
func request(cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://apps.example.test/", nil)
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	return r
}
func namedCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("missing %s cookie", name)
	return nil
}
func pair(t *testing.T, s *Service, owner string, old ...*http.Cookie) (*http.Cookie, Session) {
	t.Helper()
	ctx := context.Background()
	begin := httptest.NewRecorder()
	p, err := s.Begin(ctx, begin, request(old...), netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, p.Code, owner); err != nil {
		t.Fatal(err)
	}
	cookies := append(old, namedCookie(t, begin, PairCookie))
	promote := httptest.NewRecorder()
	session, err := s.Promote(ctx, promote, request(cookies...))
	if err != nil {
		t.Fatal(err)
	}
	return namedCookie(t, promote, OwnerCookie), session
}

func TestPairingRequiresBoundBrowserAndConsumesCode(t *testing.T) {
	s, store, _ := fixture(t)
	ctx := context.Background()
	w := httptest.NewRecorder()
	pending, err := s.Begin(ctx, w, request(), netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if _, err = s.Promote(ctx, httptest.NewRecorder(), request(namedCookie(t, w, PairCookie))); !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("unapproved promotion: %v", err)
	}
	store.fail = false
	if err = s.Approve(ctx, "unknown", "owner-a"); !errors.Is(err, ErrPairing) {
		t.Fatalf("unknown code: %v", err)
	}
	if err = s.Approve(ctx, pending.Code, "owner-a"); err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, pending.Code, "owner-b"); !errors.Is(err, ErrPairing) {
		t.Fatalf("approval replay: %v", err)
	}
	if _, err = s.Promote(ctx, httptest.NewRecorder(), request()); !errors.Is(err, ErrPairing) {
		t.Fatalf("unbound browser: %v", err)
	}
	out := httptest.NewRecorder()
	session, err := s.Promote(ctx, out, request(namedCookie(t, w, PairCookie)))
	if err != nil {
		t.Fatal(err)
	}
	if !session.Owns("owner-a") || session.Owns("owner-b") {
		t.Fatalf("wrong grants: %#v", session)
	}
	if _, err = s.Promote(ctx, httptest.NewRecorder(), request(namedCookie(t, w, PairCookie))); !errors.Is(err, ErrPairing) {
		t.Fatalf("promotion replay: %v", err)
	}
	owner := namedCookie(t, out, OwnerCookie)
	if !owner.Secure || !owner.HttpOnly || owner.Domain != "" || owner.Path != "/" {
		t.Fatalf("cookie attributes: %#v", owner)
	}
	if strings.Contains(string(store.raw), owner.Value) || strings.Contains(string(store.raw), pending.Code) {
		t.Fatal("bearer or code persisted in plaintext")
	}
}
func TestPairingExpiryRateLimitAndPersistence(t *testing.T) {
	s, store, now := fixture(t)
	ctx := context.Background()
	w := httptest.NewRecorder()
	p, err := s.Begin(ctx, w, request(), netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(pairTTL)
	if err = s.Approve(ctx, p.Code, "owner-a"); !errors.Is(err, ErrPairing) {
		t.Fatalf("expired: %v", err)
	}
	for range 29 {
		if err = s.Approve(ctx, "bad", "owner-a"); !errors.Is(err, ErrPairing) {
			t.Fatal(err)
		}
	}
	restarted, err := New(store, s.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Approve(ctx, "bad", "owner-a"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limit after restart: %v", err)
	}
	*now = now.Add(time.Minute)
	if err = restarted.Approve(ctx, "bad", "owner-a"); !errors.Is(err, ErrPairing) {
		t.Fatalf("new rate window: %v", err)
	}
}
func TestMultiOwnerPairingBoundToExistingSessionAndRevocation(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	owner, first := pair(t, s, "owner-a")
	begin := httptest.NewRecorder()
	p, err := s.Begin(ctx, begin, request(owner), netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, p.Code, "owner-b"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Promote(ctx, httptest.NewRecorder(), request(namedCookie(t, begin, PairCookie))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("lost existing binding: %v", err)
	}
	out := httptest.NewRecorder()
	second, err := s.Promote(ctx, out, request(owner, namedCookie(t, begin, PairCookie)))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.Owns("owner-a") || !second.Owns("owner-b") {
		t.Fatalf("grants: %#v", second)
	}
	updated := namedCookie(t, out, OwnerCookie)
	if _, err = s.Browser(ctx, request(owner)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old token not rotated: %v", err)
	}
	if err = s.Revoke(ctx, "owner-c", first.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong owner revoke: %v", err)
	}
	if err = s.Revoke(ctx, "owner-a", first.ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.Browser(ctx, request(updated))
	if err != nil {
		t.Fatal(err)
	}
	if remaining.Owns("owner-a") || !remaining.Owns("owner-b") {
		t.Fatalf("revocation removed wrong owner: %#v", remaining)
	}
	list, err := s.List(ctx, "owner-a")
	if err != nil || len(list) != 0 {
		t.Fatalf("revoked browser listed: %#v %v", list, err)
	}
}
func TestMutationChecksOriginCSRFAndMethod(t *testing.T) {
	s, _, _ := fixture(t)
	cookie, session := pair(t, s, "owner-a")
	tests := []struct {
		name, method, origin, csrf string
		want                       bool
	}{
		{"valid", http.MethodPost, "https://apps.example.test", session.CSRF, true},
		{"sibling", http.MethodPost, "https://abcd.example.test", session.CSRF, false},
		{"missing origin", http.MethodPost, "", session.CSRF, false},
		{"missing CSRF", http.MethodPost, "https://apps.example.test", "", false},
		{"forged CSRF", http.MethodPost, "https://apps.example.test", "forged", false},
		{"GET", http.MethodGet, "https://apps.example.test", session.CSRF, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "https://apps.example.test/mutate", nil)
			r.AddCookie(cookie)
			r.Header.Set("Origin", test.origin)
			r.Header.Set("X-Mesh-CSRF", test.csrf)
			_, err := s.ValidateMutation(context.Background(), r, "https://apps.example.test")
			if (err == nil) != test.want {
				t.Fatalf("ValidateMutation: %v", err)
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "https://apps.example.test/mutate", strings.NewReader("csrf="+session.CSRF))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://apps.example.test")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := s.ValidateMutation(context.Background(), r, "https://apps.example.test"); err != nil {
		t.Fatalf("form CSRF: %v", err)
	}
}
func TestViewTicketScopeReplayRevocationAndRestart(t *testing.T) {
	s, store, _ := fixture(t)
	ctx := context.Background()
	cookie, session := pair(t, s, "owner-a")
	if _, err := s.IssueView(ctx, request(cookie), "owner-b", "abcd"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong owner issue: %v", err)
	}
	ticket, err := s.IssueView(ctx, request(cookie), "owner-a", "abcd")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConsumeView(ctx, httptest.NewRecorder(), request(), ticket, "efgh"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong app consume: %v", err)
	}
	w := httptest.NewRecorder()
	if err = s.ConsumeView(ctx, w, request(), ticket, "abcd"); err != nil {
		t.Fatal(err)
	}
	if err = s.ConsumeView(ctx, httptest.NewRecorder(), request(), ticket, "abcd"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("view ticket replay: %v", err)
	}
	view := namedCookie(t, w, ViewCookie)
	if strings.Contains(string(store.raw), ticket) || strings.Contains(string(store.raw), view.Value) {
		t.Fatal("view bearers stored in plaintext")
	}
	restarted, err := New(store, s.now)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := restarted.ViewOwner(ctx, request(view), "abcd")
	if err != nil || owner != "owner-a" {
		t.Fatalf("durable private view: %q %v", owner, err)
	}
	if _, err = restarted.ViewOwner(ctx, request(view), "efgh"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-app view: %v", err)
	}
	if err = restarted.Revoke(ctx, "owner-a", session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.ViewOwner(ctx, request(view), "abcd"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked private view: %v", err)
	}
}
func TestFailedPersistenceDoesNotApproveOrConsume(t *testing.T) {
	s, store, _ := fixture(t)
	ctx := context.Background()
	w := httptest.NewRecorder()
	p, err := s.Begin(ctx, w, request(), netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if err = s.Approve(ctx, p.Code, "owner-a"); err == nil {
		t.Fatal("approval succeeded without durable save")
	}
	store.fail = false
	if _, err = s.Promote(ctx, httptest.NewRecorder(), request(namedCookie(t, w, PairCookie))); !errors.Is(err, ErrPairing) {
		t.Fatalf("failed save changed grants: %v", err)
	}
	if err = s.Approve(ctx, p.Code, "owner-a"); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	out := httptest.NewRecorder()
	if _, err = s.Promote(ctx, out, request(namedCookie(t, w, PairCookie))); err == nil {
		t.Fatal("promotion succeeded without durable save")
	}
	if len(out.Result().Cookies()) != 0 {
		t.Fatal("failed save issued cookie")
	}
	store.fail = false
	if _, err = s.Promote(ctx, httptest.NewRecorder(), request(namedCookie(t, w, PairCookie))); err != nil {
		t.Fatalf("failed promotion consumed pairing: %v", err)
	}
}
func TestSessionExpiryAndDuplicateCookies(t *testing.T) {
	s, _, now := fixture(t)
	cookie, _ := pair(t, s, "owner-a")
	ctx := context.Background()
	if _, err := s.Browser(ctx, request(cookie, cookie)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ambiguous cookie: %v", err)
	}
	*now = now.Add(sessionTTL)
	if _, err := s.Browser(ctx, request(cookie)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired session: %v", err)
	}
}
func TestStoredStateVersionFailsClosed(t *testing.T) {
	s, store, _ := fixture(t)
	pair(t, s, "owner-a")
	var stored map[string]any
	if err := json.Unmarshal(store.raw, &stored); err != nil {
		t.Fatal(err)
	}
	stored["version"] = float64(2)
	store.raw, _ = json.Marshal(stored)
	if _, err := New(store, s.now); err == nil {
		t.Fatal("accepted unknown security state version")
	}
}

package webauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func beginFrom(t *testing.T, s *Service, address string, cookies ...*http.Cookie) (Pairing, *httptest.ResponseRecorder, error) {
	t.Helper()
	r := request(cookies...)
	r.RemoteAddr = address
	r.Header.Set("User-Agent", "Mozilla/5.0 TestBrowser/1.0")
	w := httptest.NewRecorder()
	p, err := s.Begin(context.Background(), w, r)
	return p, w, err
}

func TestPendingPairReusesBrowserAndDoesNotWrite(t *testing.T) {
	s, store, _ := fixture(t)
	p, w, err := beginFrom(t, s, "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	cookie := namedCookie(t, w, PairCookie)
	for range 20 {
		next, _, err := beginFrom(t, s, "192.0.2.1:1234", cookie)
		if err != nil || next.Code != p.Code || !next.ExpiresAt.Equal(p.ExpiresAt) {
			t.Fatalf("same browser replaced pending code: %q -> %q, %v", p.Code, next.Code, err)
		}
	}
	if len(s.state.Pairs) != 1 || len(store.raw) != 0 {
		t.Fatalf("pending pairs = %d, durable bytes = %d; want 1 and 0", len(s.state.Pairs), len(store.raw))
	}
}

func TestPairingSourceCapEvictsOnlyUnapprovedAndIsolatesSources(t *testing.T) {
	s, _, now := fixture(t)
	first, _, err := beginFrom(t, s, "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(context.Background(), first.Code, "owner-a"); err != nil {
		t.Fatal(err)
	}
	oldest, _, err := beginFrom(t, s, "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		*now = now.Add(time.Millisecond)
		if _, _, err := beginFrom(t, s, "192.0.2.1:5678"); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.state.Pairs) != 4 {
		t.Fatalf("one source occupies %d pairs, want 4", len(s.state.Pairs))
	}
	if err := s.Approve(context.Background(), oldest.Code, "owner-a"); !errors.Is(err, ErrPairing) {
		t.Fatalf("oldest unapproved pair not evicted: %v", err)
	}
	if _, p, ok := findPair(&s.state, codeHash(first.Code)); !ok || p.Owner != "owner-a" {
		t.Fatal("approved pair was evicted")
	}
	if _, _, err := beginFrom(t, s, "192.0.2.2:1234"); err != nil {
		t.Fatalf("second source blocked: %v", err)
	}
}

func TestPairingSourceIssuanceLimitAndIPv6Prefix(t *testing.T) {
	s, _, now := fixture(t)
	for i := range 8 {
		address := "[2001:db8:1::1]:1234"
		if i%2 == 1 {
			address = "[2001:db8:1::2]:5678"
		}
		if _, _, err := beginFrom(t, s, address); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := beginFrom(t, s, "[2001:db8:1::3]:9999"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("source issuance not limited: %v", err)
	}
	if len(s.state.Pairs) != 4 {
		t.Fatalf("one IPv6 /64 occupies %d pairs", len(s.state.Pairs))
	}
	if _, _, err := beginFrom(t, s, "[2001:db8:2::1]:1234"); err != nil {
		t.Fatalf("second prefix blocked: %v", err)
	}
	*now = now.Add(time.Minute)
	if _, _, err := beginFrom(t, s, "[2001:db8:1::4]:1234"); err != nil {
		t.Fatalf("new window blocked: %v", err)
	}
}

func TestUnapprovedPairDoesNotPersistWithOtherChanges(t *testing.T) {
	s, store, _ := fixture(t)
	pending, _, err := beginFrom(t, s, "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	approved, w, err := beginFrom(t, s, "192.0.2.2:1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(context.Background(), approved.Code, "owner-a"); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(store, s.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Approve(context.Background(), pending.Code, "owner-a"); !errors.Is(err, ErrPairing) {
		t.Fatalf("unapproved pair survived restart: %v", err)
	}
	if _, err := restarted.Promote(context.Background(), httptest.NewRecorder(), request(namedCookie(t, w, PairCookie))); err != nil {
		t.Fatalf("durable approval lost: %v", err)
	}
}

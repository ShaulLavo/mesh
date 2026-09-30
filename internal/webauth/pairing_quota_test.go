package webauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func beginFrom(t *testing.T, s *Service, address string, cookies ...*http.Cookie) (Pairing, *httptest.ResponseRecorder, error) {
	t.Helper()
	r := request(cookies...)
	r.RemoteAddr = address
	r.Header.Set("User-Agent", "Mozilla/5.0 TestBrowser/1.0")
	w := httptest.NewRecorder()
	p, err := s.Begin(context.Background(), w, r, netip.MustParseAddrPort(address).Addr())
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

func TestApprovedSourceSlotsCannotBeEvicted(t *testing.T) {
	s, _, _ := fixture(t)
	for range 4 {
		p, _, err := beginFrom(t, s, "192.0.2.1:1234")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Approve(context.Background(), p.Code, "owner-a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := beginFrom(t, s, "192.0.2.1:9999"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("approved occupancy was evicted: %v", err)
	}
	if _, _, err := beginFrom(t, s, "192.0.2.2:1234"); err != nil {
		t.Fatalf("approved first source blocked second source: %v", err)
	}
}

func TestPairingConcurrentStartsRespectSourceLimits(t *testing.T) {
	s, store, _ := fixture(t)
	results := make(chan error, 32)
	for range 32 {
		go func() {
			_, _, err := beginFrom(t, s, "192.0.2.1:1234")
			results <- err
		}()
	}
	issued := 0
	for range 32 {
		err := <-results
		if err == nil {
			issued++
		} else if !errors.Is(err, ErrRateLimited) {
			t.Fatal(err)
		}
	}
	if issued != 8 || len(s.state.Pairs) != 4 || len(store.raw) != 0 {
		t.Fatalf("concurrent issuance=%d occupancy=%d durable bytes=%d", issued, len(s.state.Pairs), len(store.raw))
	}
}

func TestPendingPairSurvivesFailedApprovalWithSafeMetadata(t *testing.T) {
	s, store, _ := fixture(t)
	r := request()
	r.Header.Set("User-Agent", "TestBrowser\x1b[2J\r\n\u202e"+strings.Repeat("x", 400))
	w := httptest.NewRecorder()
	store.fail = true
	p, err := s.Begin(context.Background(), w, r, netip.MustParseAddr("::ffff:192.0.2.1"))
	if err != nil {
		t.Fatalf("pending issuance depended on disk: %v", err)
	}
	if err := s.Approve(context.Background(), p.Code, "owner-a"); err == nil {
		t.Fatal("approval ignored failed persistence")
	}
	info, err := s.Inspect(context.Background(), p.Code)
	if err != nil {
		t.Fatal(err)
	}
	if info.SourceIP != "192.0.2.1" || len([]rune(info.UserAgent)) != 256 || strings.ContainsAny(info.UserAgent, "\x1b\r\n\u202e") {
		t.Fatalf("unsafe metadata: %#v", info)
	}
	reused, err := s.Begin(context.Background(), httptest.NewRecorder(), request(namedCookie(t, w, PairCookie)), netip.MustParseAddr("192.0.2.1"))
	if err != nil || reused.Code != p.Code {
		t.Fatalf("failed approval lost pending browser: %v", err)
	}
}

func TestGlobalPairingBackstopStillReusesPendingBrowser(t *testing.T) {
	s, _, _ := fixture(t)
	first, w, err := beginFrom(t, s, "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxPairs; i++ {
		ip := netip.AddrFrom4([4]byte{198, 51, byte(i / 256), byte(i % 256)})
		if _, err := s.Begin(context.Background(), httptest.NewRecorder(), request(), ip); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := beginFrom(t, s, "203.0.113.1:1234"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("global backstop not enforced: %v", err)
	}
	reused, _, err := beginFrom(t, s, "192.0.2.1:1234", namedCookie(t, w, PairCookie))
	if err != nil || reused.Code != first.Code {
		t.Fatalf("global backstop blocked existing browser: %v", err)
	}
}

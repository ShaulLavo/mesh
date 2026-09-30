package webauth

import (
	"context"
	"errors"
	"fmt"
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

func TestFreshBrowserPairsThroughDistributedFlood(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		t.Run(fmt.Sprintf("IPv6=%t", ipv6), func(t *testing.T) {
			s, _, now := fixture(t)
			for source := range 64 {
				address := fmt.Sprintf("198.51.100.%d:1234", source+1)
				if ipv6 {
					address = fmt.Sprintf("[2001:db8:ffff:%x::1]:1234", source)
				}
				for range 4 {
					_, _, err := beginFrom(t, s, address)
					if err != nil && !errors.Is(err, ErrRateLimited) {
						t.Fatal(err)
					}
					*now = now.Add(time.Millisecond)
				}
			}
			if _, _, err := beginFrom(t, s, "203.0.113.1:1234"); err != nil {
				t.Fatalf("fresh owner browser blocked by distributed flood: %v", err)
			}
		})
	}
}

func TestGlobalPairingEvictsOldestPairOfFullestSource(t *testing.T) {
	s, _, now := fixture(t)
	var first Pairing
	for source := range 64 {
		for slot := range 4 {
			p, _, err := beginFrom(t, s, fmt.Sprintf("198.51.100.%d:1234", source+1))
			if err != nil {
				t.Fatal(err)
			}
			if source == 0 && slot == 0 {
				first = p
			}
			*now = now.Add(time.Millisecond)
		}
	}
	if _, _, err := beginFrom(t, s, "203.0.113.1:1234"); err != nil {
		t.Fatal(err)
	}
	if _, _, exists := findPair(&s.state, codeHash(first.Code)); exists {
		t.Fatal("oldest pending pair of the fullest source was not evicted")
	}
	if len(s.state.Pairs) != maxPairs {
		t.Fatal("pending table is not bounded")
	}
}

func TestIPv6DelegationHasAggregateQuotas(t *testing.T) {
	s, _, _ := fixture(t)
	for source := range 32 {
		if _, _, err := beginFrom(t, s, fmt.Sprintf("[2001:db8:ffff:%x::1]:1234", source)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.state.Pairs) != 16 {
		t.Fatalf("one /48 occupies %d pairs, want 16", len(s.state.Pairs))
	}
	if _, _, err := beginFrom(t, s, "[2001:db8:ffff:100::1]:1234"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("one /48 has no aggregate issuance limit: %v", err)
	}
	if _, _, err := beginFrom(t, s, "[2001:db8:fffe::1]:1234"); err != nil {
		t.Fatal(err)
	}
}

func TestFullSourceTrackerEvictsLeastRecentWindow(t *testing.T) {
	s, _, now := fixture(t)
	for source := range maxPairSources {
		s.sources[fmt.Sprintf("tracker-%04d", source)] = sourceWindow{StartedAt: now.Add(time.Duration(source) * time.Millisecond)}
	}
	*now = now.Add(2 * time.Second)
	if _, _, err := beginFrom(t, s, "203.0.113.1:1234"); err != nil {
		t.Fatalf("full tracker blocks owner: %v", err)
	}
	if len(s.sources) != maxPairSources {
		t.Fatal("source tracker grew beyond its bound")
	}
	if _, exists := s.sources["tracker-0000"]; exists {
		t.Fatal("least recent source was not evicted")
	}
}

func TestApprovedPairsDoNotConsumePendingCapacity(t *testing.T) {
	s, _, now := fixture(t)
	for i := range maxPairs {
		s.state.Pairs[fmt.Sprint(i)] = pairRecord{Owner: "owner-a", ExpiresAt: now.Add(pairTTL)}
	}
	if _, _, err := beginFrom(t, s, "203.0.113.1:1234"); err != nil {
		t.Fatalf("approved records blocked fresh pending browser: %v", err)
	}
	approved := 0
	for _, p := range s.state.Pairs {
		if p.Owner != "" {
			approved++
		}
	}
	if approved != maxPairs {
		t.Fatal("approved pairing was evicted")
	}
}

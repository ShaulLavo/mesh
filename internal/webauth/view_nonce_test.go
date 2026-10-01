package webauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestViewNonceBindingAndAdditiveRestart(t *testing.T) {
	s, store, _ := fixture(t)
	owner, _ := pair(t, s, "owner-a")
	start := httptest.NewRecorder()
	nonceHash, err := s.BeginView(start)
	if err != nil {
		t.Fatal(err)
	}
	nonce := namedCookie(t, start, ViewNonceCookie)
	if nonce.Domain != "" || !nonce.Secure || !nonce.HttpOnly || nonce.SameSite != http.SameSiteLaxMode || nonce.MaxAge > 60 {
		t.Fatalf("unsafe nonce cookie: %#v", nonce)
	}
	ticket, err := s.IssueView(context.Background(), request(owner), "owner-a", "abcd", nonceHash)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(store, s.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookies := range [][]*http.Cookie{nil, {{Name: ViewNonceCookie, Value: strings.Repeat("x", 43), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}}, {nonce, nonce}} {
		if err := restarted.ConsumeView(context.Background(), httptest.NewRecorder(), request(cookies...), ticket, "abcd"); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("missing, foreign or duplicate nonce accepted: %v", err)
		}
	}
	consume := httptest.NewRecorder()
	if err := restarted.ConsumeView(context.Background(), consume, request(nonce), ticket, "abcd"); err != nil {
		t.Fatal(err)
	}
	if namedCookie(t, consume, ViewNonceCookie).MaxAge != -1 {
		t.Fatal("nonce was not cleared")
	}
	if restarted.state.Version != 1 {
		t.Fatal("state version changed")
	}
}

func TestUnboundLegacyViewTicketFailsClosed(t *testing.T) {
	s, _, _ := fixture(t)
	owner, session := pair(t, s, "owner-a")
	start := httptest.NewRecorder()
	nonceHash, err := s.BeginView(start)
	if err != nil {
		t.Fatal(err)
	}
	nonce := namedCookie(t, start, ViewNonceCookie)
	ticket, err := s.IssueView(context.Background(), request(owner), "owner-a", "abcd", nonceHash)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	legacy := s.state.Tickets[hash(ticket)]
	legacy.NonceHash = ""
	s.state.Tickets[hash(ticket)] = legacy
	s.state.Views[hash(strings.Repeat("l", 43))] = viewRecord{BrowserID: session.ID, Owner: "owner-a", AppID: "abcd", ExpiresAt: session.ExpiresAt}
	s.mu.Unlock()
	if err := s.ConsumeView(context.Background(), httptest.NewRecorder(), request(nonce), ticket, "abcd"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("legacy unbound ticket accepted: %v", err)
	}
	if _, err := s.ViewSession(context.Background(), request(&http.Cookie{Name: ViewCookie, Value: strings.Repeat("l", 43), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}), "abcd"); err != nil {
		t.Fatalf("existing view session lost: %v", err)
	}
	for _, digest := range []string{"", "xyz", strings.Repeat("A", 64), strings.Repeat("a", 66)} {
		if _, err := s.IssueView(context.Background(), request(owner), "owner-a", "abcd", digest); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("invalid nonce digest accepted: %q %v", digest, err)
		}
	}
}

func TestRejectedViewConsumptionDoesNotTouchDurableState(t *testing.T) {
	for _, mode := range []string{"absent", "wrong nonce", "expired", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			s, store, now := fixture(t)
			owner, session := pair(t, s, "owner-a")
			start := httptest.NewRecorder()
			nonceHash, err := s.BeginView(start)
			if err != nil {
				t.Fatal(err)
			}
			nonce := namedCookie(t, start, ViewNonceCookie)
			ticket, err := s.IssueView(context.Background(), request(owner), "owner-a", "abcd", nonceHash)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "absent":
				ticket = strings.Repeat("A", 43)
			case "wrong nonce":
				nonce.Value = strings.Repeat("A", 43)
				nonce.Secure = true
				nonce.HttpOnly = true
				nonce.SameSite = http.SameSiteLaxMode
			case "expired":
				*now = now.Add(ticketTTL)
			case "revoked":
				s.mu.Lock()
				for key, browser := range s.state.Browsers {
					if browser.ID == session.ID {
						delete(s.state.Browsers, key)
					}
				}
				s.mu.Unlock()
			}
			store.fail = true
			if err := s.ConsumeView(context.Background(), httptest.NewRecorder(), request(nonce), ticket, "abcd"); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("invalid ticket reached durable storage: %v", err)
			}
		})
	}
}

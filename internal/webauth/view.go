package webauth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Service) ValidateMutation(ctx context.Context, r *http.Request, managementOrigin string) (Session, error) {
	session, err := s.Browser(ctx, r)
	if err != nil {
		return Session{}, err
	}
	if err = ValidateSessionMutation(r, managementOrigin, session); err != nil {
		return Session{}, err
	}
	return session, nil
}

// ValidateSessionMutation checks origin and CSRF for an already verified session.
func ValidateSessionMutation(r *http.Request, managementOrigin string, session Session) error {
	if r.Method != http.MethodPost || r.Header.Get("Origin") != managementOrigin || !validOrigin(managementOrigin) || session.CSRF == "" {
		return ErrMutation
	}
	csrf := r.Header.Get("X-Mesh-CSRF")
	if csrf == "" {
		csrf = r.Header.Get("X-CSRF-Token")
	}
	if csrf == "" {
		r.Body = http.MaxBytesReader(nil, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			return ErrMutation
		}
		csrf = r.PostForm.Get("csrf")
	}
	if subtle.ConstantTimeCompare([]byte(csrf), []byte(session.CSRF)) != 1 {
		return ErrMutation
	}
	return nil
}
func validOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}
func validApp(appID string) bool {
	return appID != "" && len(appID) <= 64 && !strings.ContainsAny(appID, "/\\\x00\r\n ")
}

func (s *Service) BeginView(w http.ResponseWriter) (string, error) {
	nonce, err := bearer()
	if err != nil {
		return "", err
	}
	writeCookie(w, ViewNonceCookie, nonce, s.now().Add(ticketTTL), s.now())
	return hash(nonce), nil
}

func (s *Service) IssueView(ctx context.Context, r *http.Request, owner, appID, nonceHash string) (string, error) {
	nonce, err := hex.DecodeString(nonceHash)
	if err != nil || len(nonce) != 32 || hex.EncodeToString(nonce) != nonceHash {
		return "", ErrUnauthorized
	}
	if !validOwner(owner) || !validApp(appID) {
		return "", ErrUnauthorized
	}
	token, err := bearer()
	if err != nil {
		return "", err
	}
	err = s.change(ctx, func(d *state, now time.Time) error {
		b, err := browserIn(d, r, now)
		if err != nil || !b.Owns(owner) {
			return ErrUnauthorized
		}
		if len(d.Tickets) >= maxTickets {
			return ErrCapacity
		}
		d.Tickets[hash(token)] = viewRecord{BrowserID: b.ID, Owner: owner, AppID: appID, NonceHash: nonceHash, ExpiresAt: minTime(now.Add(ticketTTL), b.ExpiresAt)}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
func viewTicketKeys(r *http.Request, ticket, appID string) (string, string, error) {
	nonceHash, err := cookieKey(r, ViewNonceCookie)
	if err != nil || len(ticket) != 43 || !validApp(appID) {
		return "", "", ErrUnauthorized
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(ticket)
	if err != nil || len(decoded) != 32 {
		return "", "", ErrUnauthorized
	}
	return hash(ticket), nonceHash, nil
}

func checkViewTicket(d *state, ticketHash, nonceHash, appID string, now time.Time) error {
	v, ok := d.Tickets[ticketHash]
	if !ok || v.AppID != appID || !now.Before(v.ExpiresAt) || v.NonceHash == "" || subtle.ConstantTimeCompare([]byte(v.NonceHash), []byte(nonceHash)) != 1 {
		return ErrUnauthorized
	}
	b, err := browserByID(d, v.BrowserID, now)
	if err != nil || !b.Owns(v.Owner) {
		return ErrUnauthorized
	}
	return nil
}

// CheckView rejects anonymous probes before app lookup or durable auth work.
func (s *Service) CheckView(ctx context.Context, r *http.Request, ticket, appID string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("webauth: check view ticket: %w", err)
	}
	ticketHash, nonceHash, err := viewTicketKeys(r, ticket, appID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return checkViewTicket(&s.state, ticketHash, nonceHash, appID, s.now().UTC())
}

func (s *Service) ConsumeView(ctx context.Context, w http.ResponseWriter, r *http.Request, ticket, appID string) error {
	ticketHash, nonceHash, err := viewTicketKeys(r, ticket, appID)
	if err != nil {
		return err
	}
	var token string
	var expires time.Time
	err = s.changeIf(ctx, func(d *state, now time.Time) error {
		return checkViewTicket(d, ticketHash, nonceHash, appID, now)
	}, func(d *state, now time.Time) error {
		if len(d.Views) >= maxViews {
			return ErrCapacity
		}
		var err error
		token, err = bearer()
		if err != nil {
			return err
		}
		v := d.Tickets[ticketHash]
		b, err := browserByID(d, v.BrowserID, now)
		if err != nil {
			return err
		}
		delete(d.Tickets, ticketHash)
		if existing, err := cookieKey(r, ViewCookie); err == nil {
			delete(d.Views, existing)
		}
		v.NonceHash = ""
		v.ExpiresAt = minTime(now.Add(viewTTL), b.ExpiresAt)
		d.Views[hash(token)] = v
		expires = v.ExpiresAt
		return nil
	})
	if err != nil {
		return err
	}
	writeCookie(w, ViewCookie, token, expires, s.now())
	http.SetCookie(w, &http.Cookie{Name: ViewNonceCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	return nil
}

type ViewSession struct {
	Owner, BrowserID string
	ExpiresAt        time.Time
}

func (s *Service) ViewOwner(ctx context.Context, r *http.Request, appID string) (string, error) {
	view, err := s.ViewSession(ctx, r, appID)
	return view.Owner, err
}
func (s *Service) ViewSession(ctx context.Context, r *http.Request, appID string) (ViewSession, error) {
	if err := ctx.Err(); err != nil {
		return ViewSession{}, fmt.Errorf("webauth: read view session: %w", err)
	}
	key, err := cookieKey(r, ViewCookie)
	if err != nil {
		return ViewSession{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state.Views[key]
	if !ok || v.AppID != appID || !s.now().Before(v.ExpiresAt) {
		return ViewSession{}, ErrUnauthorized
	}
	b, err := browserByID(&s.state, v.BrowserID, s.now())
	if err != nil || !b.Owns(v.Owner) {
		return ViewSession{}, ErrUnauthorized
	}
	return ViewSession{Owner: v.Owner, BrowserID: v.BrowserID, ExpiresAt: v.ExpiresAt}, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

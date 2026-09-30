package webauth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Service) ValidateMutation(ctx context.Context, r *http.Request, managementOrigin string) (Session, error) {
	if r.Method != http.MethodPost || r.Header.Get("Origin") != managementOrigin || !validOrigin(managementOrigin) {
		return Session{}, ErrMutation
	}
	session, err := s.Browser(ctx, r)
	if err != nil {
		return Session{}, err
	}
	csrf := r.Header.Get("X-Mesh-CSRF")
	if csrf == "" {
		csrf = r.Header.Get("X-CSRF-Token")
	}
	if csrf == "" {
		r.Body = http.MaxBytesReader(nil, r.Body, 16<<10)
		if err = r.ParseForm(); err != nil {
			return Session{}, ErrMutation
		}
		csrf = r.PostForm.Get("csrf")
	}
	if subtle.ConstantTimeCompare([]byte(csrf), []byte(session.CSRF)) != 1 {
		return Session{}, ErrMutation
	}
	return session, nil
}
func validOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}
func validApp(appID string) bool {
	return appID != "" && len(appID) <= 64 && !strings.ContainsAny(appID, "/\\\x00\r\n ")
}

func (s *Service) IssueView(ctx context.Context, r *http.Request, owner, appID string) (string, error) {
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
		d.Tickets[hash(token)] = viewRecord{BrowserID: b.ID, Owner: owner, AppID: appID, ExpiresAt: minTime(now.Add(ticketTTL), b.ExpiresAt)}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
func (s *Service) ConsumeView(ctx context.Context, w http.ResponseWriter, r *http.Request, ticket, appID string) error {
	if len(ticket) != 43 || !validApp(appID) {
		return ErrUnauthorized
	}
	token, err := bearer()
	if err != nil {
		return err
	}
	var expires time.Time
	err = s.change(ctx, func(d *state, now time.Time) error {
		v, ok := d.Tickets[hash(ticket)]
		if !ok || v.AppID != appID {
			return ErrUnauthorized
		}
		b, err := browserByID(d, v.BrowserID, now)
		if err != nil || !b.Owns(v.Owner) {
			return ErrUnauthorized
		}
		if len(d.Views) >= maxViews {
			return ErrCapacity
		}
		delete(d.Tickets, hash(ticket))
		if existing, err := cookieKey(r, ViewCookie); err == nil {
			delete(d.Views, existing)
		}
		v.ExpiresAt = minTime(now.Add(viewTTL), b.ExpiresAt)
		d.Views[hash(token)] = v
		expires = v.ExpiresAt
		return nil
	})
	if err != nil {
		return err
	}
	writeCookie(w, ViewCookie, token, expires, s.now())
	return nil
}
func (s *Service) ViewOwner(ctx context.Context, r *http.Request, appID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := cookieKey(r, ViewCookie)
	if err != nil {
		return "", err
	}
	v, ok := s.state.Views[key]
	if !ok || v.AppID != appID || !s.now().Before(v.ExpiresAt) {
		return "", ErrUnauthorized
	}
	b, err := browserByID(&s.state, v.BrowserID, s.now())
	if err != nil || !b.Owns(v.Owner) {
		return "", ErrUnauthorized
	}
	return v.Owner, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

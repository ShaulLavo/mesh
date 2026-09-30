package apps

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

type networkSessionKey struct{}

func (e *Edge) authenticateNetwork(r *http.Request) *http.Request {
	if e.config.NetworkOwners == nil || e.config.ClientIP == nil {
		return r
	}
	ip := e.config.ClientIP(r)
	if !ip.IsValid() {
		return r
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	owners, err := e.config.NetworkOwners(ctx, ip)
	cancel()
	if err != nil || len(owners) == 0 {
		return r
	}
	allowed := []string{}
	for _, owner := range owners {
		if e.config.Allowed[owner] {
			allowed = append(allowed, owner)
		}
	}
	if len(allowed) == 0 {
		return r
	}
	mac := hmac.New(sha256.New, e.config.Key.Seed())
	_, _ = mac.Write([]byte("mesh-app/tailnet-csrf/v1\x00" + ip.String()))
	session := webauth.Session{Owners: allowed, CSRF: base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
	return r.WithContext(context.WithValue(r.Context(), networkSessionKey{}, session))
}

func (e *Edge) browser(r *http.Request) (webauth.Session, error) {
	if session, ok := r.Context().Value(networkSessionKey{}).(webauth.Session); ok {
		return session, nil
	}
	return e.auth.Browser(r.Context(), r)
}

func networkOwns(r *http.Request, owner string) bool {
	session, _ := r.Context().Value(networkSessionKey{}).(webauth.Session)
	return session.Owns(owner)
}

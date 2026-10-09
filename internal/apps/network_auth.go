package apps

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

type networkSessionKey struct{}

func (e *Registry) authenticateNetwork(r *http.Request) *http.Request {
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
	key, err := hkdf.Key(sha256.New, e.config.Key.Seed(), nil, "mesh-app/tailnet-csrf/key/v1", sha256.Size)
	if err != nil {
		return r
	}
	mac := hmac.New(sha256.New, key)
	bucket := e.config.Now().Unix() / int64(time.Hour/time.Second)
	_, _ = mac.Write([]byte("mesh-app/tailnet-csrf/v2\x00" + ip.String() + "\x00" + strconv.FormatInt(bucket, 10)))
	session := webauth.Session{Owners: allowed, CSRF: base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
	return r.WithContext(context.WithValue(r.Context(), networkSessionKey{}, session))
}

func (e *Registry) browser(r *http.Request) (webauth.Session, error) {
	if session, ok := r.Context().Value(networkSessionKey{}).(webauth.Session); ok {
		paired, err := e.auth.Browser(r.Context(), r)
		if err == nil {
			session.Owners = append(append([]string(nil), session.Owners...), paired.Owners...)
		}
		return session, nil
	}
	return e.auth.Browser(r.Context(), r)
}

func networkOwns(r *http.Request, owner string) bool {
	session, _ := r.Context().Value(networkSessionKey{}).(webauth.Session)
	return session.Owns(owner)
}

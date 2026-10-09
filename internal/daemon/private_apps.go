package daemon

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

type privateAppRouter interface {
	HasHost(string) bool
	ServeHost(http.ResponseWriter, *http.Request, string) bool
}

type privateAppsHandler struct {
	apps   privateAppRouter
	owners func(context.Context, netip.Addr) ([]string, error)
}

func privateAppsHTTPHandler(apps privateAppRouter, owners func(context.Context, netip.Addr) ([]string, error)) http.Handler {
	return &privateAppsHandler{apps: apps, owners: owners}
}

func appClientAddress(request *http.Request) netip.Addr {
	peer, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return peer.Addr().Unmap()
}

func (h *privateAppsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, valid := meshserve.CanonicalHost(r.Host)
	if !valid || !h.apps.HasHost(host) {
		http.NotFound(w, r)
		return
	}
	if r.TLS == nil {
		http.NotFound(w, r)
		return
	}
	sni, valid := meshserve.CanonicalHost(r.TLS.ServerName)
	if !valid || sni != host {
		w.WriteHeader(http.StatusMisdirectedRequest)
		return
	}
	if !h.admitted(r) {
		http.NotFound(w, r)
		return
	}
	if !h.apps.ServeHost(w, r, host) {
		http.NotFound(w, r)
	}
}

// The TLS listener authenticates the PROXY source before this handler runs.
// Forwarded headers and browser cookies never establish private ingress.
func (h *privateAppsHandler) admitted(r *http.Request) bool {
	if h.owners == nil {
		return false
	}
	address := appClientAddress(r)
	if !netip.MustParsePrefix("100.64.0.0/10").Contains(address) && !netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(address) {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	owners, err := h.owners(ctx, address)
	return err == nil && len(owners) != 0
}

func (h *privateAppsHandler) Close() {
	if closer, ok := h.apps.(interface{ Close() }); ok {
		closer.Close()
	}
}

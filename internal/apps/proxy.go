package apps

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

type proxyTransports struct {
	mu      sync.Mutex
	entries map[netip.AddrPort]*http.Transport
}

func appTransport(dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: dial, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 1 << 20, DisableCompression: true, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second}
}
func (p *proxyTransports) forEndpoint(endpoint netip.AddrPort) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = map[netip.AddrPort]*http.Transport{}
	}
	if transport := p.entries[endpoint]; transport != nil {
		return transport
	}
	if len(p.entries) >= 128 {
		for key, transport := range p.entries {
			transport.CloseIdleConnections()
			delete(p.entries, key)
			break
		}
	}
	transport := appTransport(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return netDialer.DialContext(ctx, network, endpoint.String())
	})
	p.entries[endpoint] = transport
	return transport
}
func (p *proxyTransports) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, transport := range p.entries {
		transport.CloseIdleConnections()
		delete(p.entries, key)
	}
}
func (e *Edge) Close() { e.transports.close() }

type appHandler struct {
	http.Handler
	transport *http.Transport
}

// originHandler proxies to the app's verified upstream only: another family's
// loopback on the same port may belong to anyone.
func originHandler(app appRoute) *appHandler {
	target := &url.URL{Scheme: "http", Host: app.Upstream.String()}
	transport := appTransport(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return netDialer.DialContext(ctx, network, target.Host)
	})
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "app server unavailable", http.StatusServiceUnavailable)
	}
	return &appHandler{Handler: proxy, transport: transport}
}

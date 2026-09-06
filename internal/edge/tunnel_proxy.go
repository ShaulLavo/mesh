package edge

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/shaul/mesh/internal/tunnel"
)

type tunnelRoute struct {
	publicName string
	claimantID string
	endpoint   tunnel.Endpoint
	transport  *http.Transport
	proxy      *httputil.ReverseProxy
	active     atomic.Bool
	closeOnce  sync.Once
	release    func()
}

func (r *Registry) newTunnelRoute(name, claimant string, endpoint tunnel.Endpoint) *tunnelRoute {
	route := &tunnelRoute{publicName: name, claimantID: claimant, endpoint: endpoint}
	route.active.Store(true)
	route.transport = &http.Transport{
		Proxy: nil, DialContext: route.dial, DisableKeepAlives: true,
		ForceAttemptHTTP2: false, ResponseHeaderTimeout: proxyResponseHeaderTimeout,
		MaxResponseHeaderBytes: maximumResponseHeaderBytes,
		ReadBufferSize:         32 << 10, WriteBufferSize: 32 << 10,
	}
	route.proxy = &httputil.ReverseProxy{
		Transport: route.transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			removeForwarded(p.Out.Header)
			p.Out.URL.Scheme = "http"
			p.Out.URL.Host = name
			p.Out.Host = p.In.Host
			p.Out.Header.Set("X-Forwarded-Host", p.In.Host)
			p.Out.Header.Set("X-Forwarded-Proto", r.forwardedScheme(p.In))
			if address, ok := p.In.Context().Value(proxyClientIPKey{}).(netip.Addr); ok {
				p.Out.Header.Set("X-Forwarded-For", address.String())
			}
		},
		ErrorLog:     log.New(io.Discard, "", 0),
		ErrorHandler: route.proxyError,
	}
	return route
}

func (route *tunnelRoute) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	if !route.active.Load() {
		return nil, net.ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, proxyDialTimeout)
	defer cancel()
	connection, err := route.endpoint.Dial(ctx)
	if err != nil {
		if !errors.Is(err, tunnel.ErrCapacity) {
			route.release()
		}
		return nil, err
	}
	if !route.active.Load() {
		_ = connection.Close()
		return nil, net.ErrClosed
	}
	return connection, nil
}

func (route *tunnelRoute) proxyError(response http.ResponseWriter, request *http.Request, err error) {
	if !route.active.Load() {
		http.NotFound(response, request)
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, _ := request.Body.(*inboundRequestBody)
	if body != nil && body.timedOut.Load() || errors.Is(err, errInboundRequestBodyTimeout) {
		http.Error(response, "request body timed out", http.StatusRequestTimeout)
		return
	}
	http.Error(response, "service temporarily unavailable", http.StatusServiceUnavailable)
}

func (route *tunnelRoute) serve(response http.ResponseWriter, request *http.Request, registry *Registry) {
	if !route.active.Load() {
		http.NotFound(response, request)
		return
	}
	if request.ContentLength > registry.requestBodyLimit {
		http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	clientIP, ok := request.Context().Value(proxyClientIPKey{}).(netip.Addr)
	if !ok || !registry.clients.Acquire(clientIP) {
		http.Error(response, "service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer registry.clients.Release(clientIP)
	select {
	case registry.global <- struct{}{}:
		defer func() { <-registry.global }()
	default:
		http.Error(response, "service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	request.Body = &inboundRequestBody{ReadCloser: http.MaxBytesReader(response, request.Body, registry.requestBodyLimit)}
	route.proxy.ServeHTTP(response, request)
}

func (route *tunnelRoute) close() {
	route.closeOnce.Do(func() {
		route.active.Store(false)
		route.transport.CloseIdleConnections()
		_ = route.endpoint.Close()
	})
}

func (r *Registry) setTunnel(route *tunnelRoute) {
	r.tunnelsMu.Lock()
	r.tunnels[route.publicName] = route
	r.tunnelsMu.Unlock()
}

func (r *Registry) removeTunnel(route *tunnelRoute) {
	r.tunnelsMu.Lock()
	if r.tunnels[route.publicName] == route {
		route.active.Store(false)
		delete(r.tunnels, route.publicName)
	}
	r.tunnelsMu.Unlock()
}

func (r *Registry) findTunnel(name string) *tunnelRoute {
	r.tunnelsMu.RLock()
	route := r.tunnels[name]
	r.tunnelsMu.RUnlock()
	return route
}

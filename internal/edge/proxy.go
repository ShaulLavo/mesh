package edge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/serve"
)

const (
	DefaultPublicListenAddress = "127.0.0.1:8080"
	DefaultDirectListenAddress = ":443"
	maximumRequestBody         = 64 << 20
	maximumResponseHeaderBytes = 1 << 20
	maximumConcurrentPerOrigin = 32
	maximumConcurrentUpstreams = 128
	maximumConcurrentPerClient = 8
	maximumRateClients         = 4096
	maximumRequestsPerMinute   = 120
	MaximumTotalRoutes         = 8192
	defaultListLimit           = 50
	maximumListLimit           = 100
	maximumListCursorLength    = 786
	proxyDialTimeout           = 2 * time.Second
	proxyResponseHeaderTimeout = 3 * time.Second
	proxyIdleConnectionTimeout = 30 * time.Second
	wakeTimeout                = 5 * time.Second
)

var errInboundRequestBodyTimeout = errors.New("edge: inbound request body timed out")

// Mode selects the public listener trust boundary.
type Mode string

const (
	ModeProxy     Mode = "proxy"
	ModeDirectTLS Mode = "direct-tls"
)

// Waker is the bounded seam to the always-on Pi. Wake should return only when
// an origin is ready for one connection or when ctx ends.
type Waker interface {
	Wake(context.Context, string) error
}

type noWaker struct{}

func (noWaker) Wake(context.Context, string) error { return errors.New("wake is not configured") }

// DialContext is injectable so deterministic tests can retain the numeric
// target invariant without requiring a real tailnet.
type DialContext func(context.Context, string, string) (net.Conn, error)

// HandlerConfig bounds the public proxy and names its trust mode.
type HandlerConfig struct {
	Mode             Mode
	ReservedPath     string
	Waker            Waker
	DialContext      DialContext
	Now              func() time.Time
	RequestBodyLimit int64
	WakeTimeout      time.Duration
	Logger           *log.Logger
}

// ResolvedOrigin combines durable safe display state with an edge-resolved
// numeric endpoint. Endpoint is never derived from a signed route.
type ResolvedOrigin struct {
	Identity     string
	DisplayAlias string
	Endpoint     netip.AddrPort
	LastSeenAt   time.Time
	OnlineUntil  time.Time
	Online       bool
}

// PublishedRoute is one proxy claim joined to its pinned runtime origin.
type PublishedRoute struct {
	Route  Route
	Origin ResolvedOrigin
}

// RouteStatus is safe to expose through edge.list.
type RouteStatus struct {
	PublicName    string
	ServiceName   string
	WakeOnRequest bool
	DisplayAlias  string
	LastSeenAt    time.Time
	Online        bool
}

// Registry atomically publishes a complete public route table.
type Registry struct {
	mode             Mode
	waker            Waker
	wakerConfigured  bool
	now              func() time.Time
	transport        *http.Transport
	rate             *clientRateLimiter
	clients          *clientConcurrencyLimiter
	snapshot         atomic.Pointer[proxySnapshot]
	budgetsMu        sync.Mutex
	budgets          map[string]chan struct{}
	global           chan struct{}
	changeMu         sync.Mutex
	change           chan struct{}
	generation       uint64
	requestBodyLimit int64
	wakeTimeout      time.Duration
	logger           *eventLogger
	reservedPath     string
}

type proxySnapshot struct {
	generation uint64
	routes     []proxyRoute
	list       []proxyRoute
}

type proxyRoute struct {
	publicName string
	prefix     string
	wake       bool
	origin     *originRuntime
	proxy      *httputil.ReverseProxy
}

type originRuntime struct {
	identity     string
	displayAlias string
	endpoint     netip.AddrPort
	lastSeenAt   time.Time
	onlineUntil  time.Time
	online       atomic.Bool
	budget       chan struct{}
}

// NewRegistry constructs an empty bounded public proxy.
func NewRegistry(config HandlerConfig) (*Registry, error) {
	if config.Mode != ModeProxy && config.Mode != ModeDirectTLS {
		return nil, fmt.Errorf("edge: unsupported listener mode %q", config.Mode)
	}
	if config.ReservedPath == "" {
		config.ReservedPath = "/mesh"
	}
	if err := validateControlPath(config.ReservedPath); err != nil {
		return nil, fmt.Errorf("edge: reserved terminal path: %w", err)
	}
	wakerConfigured := config.Waker != nil
	if config.Waker == nil {
		config.Waker = noWaker{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.RequestBodyLimit == 0 {
		config.RequestBodyLimit = maximumRequestBody
	}
	if config.RequestBodyLimit < 1 || config.RequestBodyLimit > maximumRequestBody {
		return nil, fmt.Errorf("edge: request body limit %d is outside 1..%d", config.RequestBodyLimit, maximumRequestBody)
	}
	if config.WakeTimeout == 0 {
		config.WakeTimeout = wakeTimeout
	}
	if config.WakeTimeout <= 0 || config.WakeTimeout > wakeTimeout {
		return nil, fmt.Errorf("edge: wake timeout %s is outside (0,%s]", config.WakeTimeout, wakeTimeout)
	}
	if config.DialContext == nil {
		dialer := &net.Dialer{Timeout: proxyDialTimeout, KeepAlive: 30 * time.Second}
		config.DialContext = dialer.DialContext
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: config.DialContext, ForceAttemptHTTP2: false,
		MaxIdleConns: 128, MaxIdleConnsPerHost: maximumConcurrentPerOrigin,
		IdleConnTimeout: proxyIdleConnectionTimeout, ResponseHeaderTimeout: proxyResponseHeaderTimeout,
		MaxResponseHeaderBytes: maximumResponseHeaderBytes,
	}
	registry := &Registry{
		mode: config.Mode, waker: config.Waker, wakerConfigured: wakerConfigured, now: config.Now, transport: transport,
		rate:    newClientRateLimiter(maximumRateClients, maximumRequestsPerMinute, time.Minute),
		clients: newClientConcurrencyLimiter(maximumConcurrentPerClient),
		budgets: make(map[string]chan struct{}), global: make(chan struct{}, maximumConcurrentUpstreams), change: make(chan struct{}),
		requestBodyLimit: config.RequestBodyLimit,
		wakeTimeout:      config.WakeTimeout,
		logger:           newEventLogger(config.Logger, config.Now),
		reservedPath:     config.ReservedPath,
	}
	registry.snapshot.Store(&proxySnapshot{})
	return registry, nil
}

// WakeAvailable reports whether offline wake requests have an operational
// configured boundary rather than the fail-closed placeholder.
func (r *Registry) WakeAvailable() bool { return r != nil && r.wakerConfigured }

// Close releases idle origin connections.
func (r *Registry) Close() {
	r.transport.CloseIdleConnections()
	r.logger.Close()
}

// Replace validates and publishes a complete joined route table.
func (r *Registry) Replace(routes []PublishedRoute) error {
	if len(routes) > MaximumTotalRoutes {
		return fmt.Errorf("edge: total route count %d exceeds %d", len(routes), MaximumTotalRoutes)
	}
	built := &proxySnapshot{routes: make([]proxyRoute, 0, len(routes))}
	seen := make(map[string]struct{}, len(routes))
	runtimes := make(map[string]*originRuntime)
	for _, published := range routes {
		if err := validateRoute(published.Route); err != nil {
			return err
		}
		if _, err := parseIdentity("origin", published.Origin.Identity); err != nil {
			return err
		}
		if err := validateDisplayAlias(published.Origin.DisplayAlias); err != nil {
			return err
		}
		if published.Origin.Online && (!published.Origin.Endpoint.IsValid() || published.Origin.Endpoint.Port() == 0) {
			return errors.New("edge: resolved origin endpoint is invalid")
		}
		key := published.Route.PublicName + "\x00" + published.Route.ServiceName
		if _, exists := seen[key]; exists {
			return ErrRouteCollision
		}
		seen[key] = struct{}{}
		runtime := runtimes[published.Origin.Identity]
		if runtime == nil {
			r.budgetsMu.Lock()
			budget := r.budgets[published.Origin.Identity]
			if budget == nil {
				budget = make(chan struct{}, maximumConcurrentPerOrigin)
				r.budgets[published.Origin.Identity] = budget
			}
			r.budgetsMu.Unlock()
			runtime = &originRuntime{
				identity: published.Origin.Identity, displayAlias: published.Origin.DisplayAlias,
				endpoint: published.Origin.Endpoint, lastSeenAt: published.Origin.LastSeenAt.UTC(),
				onlineUntil: published.Origin.OnlineUntil.UTC(), budget: budget,
			}
			runtime.online.Store(published.Origin.Online)
			runtimes[published.Origin.Identity] = runtime
		}
		route := proxyRoute{
			publicName: published.Route.PublicName, prefix: "/" + published.Route.ServiceName,
			wake: published.Route.WakeOnRequest, origin: runtime,
		}
		route.proxy = r.reverseProxy(&route)
		built.routes = append(built.routes, route)
	}
	sort.Slice(built.routes, func(i, j int) bool {
		if built.routes[i].publicName != built.routes[j].publicName {
			return built.routes[i].publicName < built.routes[j].publicName
		}
		if len(built.routes[i].prefix) != len(built.routes[j].prefix) {
			return len(built.routes[i].prefix) > len(built.routes[j].prefix)
		}
		return built.routes[i].prefix < built.routes[j].prefix
	})
	built.list = append([]proxyRoute(nil), built.routes...)
	sort.Slice(built.list, func(i, j int) bool {
		if built.list[i].publicName != built.list[j].publicName {
			return built.list[i].publicName < built.list[j].publicName
		}
		return built.list[i].prefix < built.list[j].prefix
	})
	r.changeMu.Lock()
	r.generation++
	built.generation = r.generation
	r.snapshot.Store(built)
	close(r.change)
	r.change = make(chan struct{})
	r.changeMu.Unlock()
	return nil
}

// Status returns a safe route snapshot without internal names or endpoints.
func (r *Registry) Status() []RouteStatus {
	now := r.now().UTC()
	snapshot := r.snapshot.Load()
	statuses := make([]RouteStatus, 0, len(snapshot.routes))
	for _, route := range snapshot.routes {
		statuses = append(statuses, RouteStatus{
			PublicName: route.publicName, ServiceName: strings.TrimPrefix(route.prefix, "/"), WakeOnRequest: route.wake,
			DisplayAlias: route.origin.displayAlias, LastSeenAt: route.origin.lastSeenAt,
			Online: route.origin.online.Load() && now.Before(route.origin.onlineUntil),
		})
	}
	return statuses
}

// Page returns one stable bounded edge.list page keyed after the last route.
func (r *Registry) Page(cursor string, limit int) ([]RouteStatus, string, error) {
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maximumListLimit {
		return nil, "", fmt.Errorf("edge: list limit %d is outside 1..%d", limit, maximumListLimit)
	}
	snapshot := r.snapshot.Load()
	index := 0
	if cursor != "" {
		publicName, prefix, err := decodeListCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		index = sort.Search(len(snapshot.list), func(index int) bool {
			route := snapshot.list[index]
			return route.publicName > publicName || route.publicName == publicName && route.prefix > prefix
		})
	}
	end := min(index+limit, len(snapshot.list))
	now := r.now().UTC()
	statuses := make([]RouteStatus, 0, end-index)
	for _, route := range snapshot.list[index:end] {
		statuses = append(statuses, routeStatus(route, now))
	}
	next := ""
	if end < len(snapshot.list) {
		last := snapshot.list[end-1]
		next = encodeListCursor(last.publicName, last.prefix)
	}
	return statuses, next, nil
}

func routeStatus(route proxyRoute, now time.Time) RouteStatus {
	return RouteStatus{
		PublicName: route.publicName, ServiceName: strings.TrimPrefix(route.prefix, "/"), WakeOnRequest: route.wake,
		DisplayAlias: route.origin.displayAlias, LastSeenAt: route.origin.lastSeenAt,
		Online: route.origin.online.Load() && now.Before(route.origin.onlineUntil),
	}
}

func encodeListCursor(publicName, prefix string) string {
	contents := publicName + "\x00" + strings.TrimPrefix(prefix, "/")
	return base64.RawURLEncoding.EncodeToString([]byte(contents))
}

func decodeListCursor(cursor string) (string, string, error) {
	if cursor == "" || len(cursor) > maximumListCursorLength {
		return "", "", errors.New("edge: list cursor length is invalid")
	}
	contents, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(contents) > 1024 || base64.RawURLEncoding.EncodeToString(contents) != cursor {
		return "", "", errors.New("edge: list cursor is not canonical")
	}
	parts := strings.Split(string(contents), "\x00")
	if len(parts) != 2 || serve.ValidatePublicName(parts[0]) != nil || validateRoute(Route{PublicName: parts[0], ServiceName: parts[1]}) != nil {
		return "", "", errors.New("edge: list cursor is invalid")
	}
	return parts[0], "/" + parts[1], nil
}

// ServeHTTP rejects malformed public requests before route lookup.
func (r *Registry) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect || request.URL.IsAbs() || request.URL.Opaque != "" {
		r.logger.Print("edge event=invalid-request")
		http.NotFound(response, request)
		return
	}
	if reservedTerminalPath(request.URL, r.reservedPath) {
		r.logger.Print("edge event=reserved-terminal-path")
		http.NotFound(response, request)
		return
	}
	publicName, forwardedHost, err := canonicalPublicHost(request.Host)
	if err != nil {
		r.logger.Print("edge event=invalid-public-host")
		http.NotFound(response, request)
		return
	}
	clientIP, err := immediateClientIP(request.RemoteAddr)
	if err != nil {
		r.logger.Print("edge event=invalid-client-address")
		http.NotFound(response, request)
		return
	}
	if r.mode == ModeProxy && !clientIP.IsLoopback() {
		r.logger.Printf("edge event=untrusted-front-door client=%s", clientIP)
		http.NotFound(response, request)
		return
	}
	if r.mode == ModeDirectTLS && request.TLS == nil {
		r.logger.Printf("edge event=plaintext-direct-request client=%s host=%s", clientIP, publicName)
		http.NotFound(response, request)
		return
	}
	if r.mode == ModeDirectTLS {
		serverName := request.TLS.ServerName
		if serverName != publicName || serve.ValidatePublicName(serverName) != nil {
			r.logger.Printf("edge event=invalid-server-name client=%s host=%s", clientIP, publicName)
			http.NotFound(response, request)
			return
		}
	}
	if r.mode == ModeProxy {
		clientIP, err = trustedForwardedMetadata(request.Header)
		if err != nil {
			r.logger.Print("edge event=invalid-forwarded-metadata")
			http.NotFound(response, request)
			return
		}
	}
	if !r.rate.Allow(clientIP, r.now().UTC()) {
		r.logger.Printf("edge event=rate-limit client=%s host=%s", clientIP, publicName)
		http.Error(response, "request limit exceeded", http.StatusTooManyRequests)
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), proxyClientIPKey{}, clientIP))
	request.Host = forwardedHost

	requestPath := request.URL.EscapedPath()
	snapshot := r.snapshot.Load()
	for index := range snapshot.routes {
		route := &snapshot.routes[index]
		if route.publicName != publicName || requestPath != route.prefix && !strings.HasPrefix(requestPath, route.prefix+"/") {
			continue
		}
		route.serve(response, request, r, snapshot.generation)
		return
	}
	http.NotFound(response, request)
	r.logger.Printf("edge event=unknown-route client=%s host=%s", clientIP, publicName)
}

func (route *proxyRoute) serve(response http.ResponseWriter, request *http.Request, registry *Registry, generation uint64) {
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
	select {
	case route.origin.budget <- struct{}{}:
		defer func() { <-route.origin.budget }()
	default:
		http.Error(response, "service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	now := registry.now().UTC()
	online := route.origin.online.Load() && now.Before(route.origin.onlineUntil)
	if !online && route.wake {
		wokenOriginID := route.origin.identity
		originalRoute := route
		wakeContext, cancel := context.WithTimeout(request.Context(), registry.wakeTimeout)
		err := registry.waker.Wake(wakeContext, wokenOriginID)
		if err == nil {
			freshRoute, waitErr := registry.waitForFreshRoute(wakeContext, route.publicName, route.prefix, wokenOriginID, generation)
			if waitErr == nil {
				route = freshRoute
			}
			err = waitErr
		}
		cancel()
		online = err == nil
		if !online {
			route = originalRoute
		}
	}
	if !online {
		writeOffline(response, route.origin)
		return
	}
	request.Body = &inboundRequestBody{ReadCloser: http.MaxBytesReader(response, request.Body, registry.requestBodyLimit)}
	route.proxy.ServeHTTP(response, request)
}

type inboundRequestBody struct {
	io.ReadCloser
	timedOut atomic.Bool
}

func (b *inboundRequestBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	var timeout interface {
		error
		Timeout() bool
	}
	if err != nil && errors.As(err, &timeout) && timeout.Timeout() {
		b.timedOut.Store(true)
		return read, errInboundRequestBodyTimeout
	}
	return read, err
}

func (r *Registry) reverseProxy(route *proxyRoute) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: r.transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			removeForwarded(request.Out.Header)
			request.Out.URL.Scheme = "http"
			request.Out.URL.Host = route.origin.endpoint.String()
			request.Out.Host = request.In.Host
			request.Out.Header.Set("X-Forwarded-Host", request.In.Host)
			request.Out.Header.Set("X-Forwarded-Proto", r.forwardedScheme(request.In))
			if clientIP, ok := request.In.Context().Value(proxyClientIPKey{}).(netip.Addr); ok {
				request.Out.Header.Set("X-Forwarded-For", clientIP.String())
			}
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(response http.ResponseWriter, request *http.Request, proxyErr error) {
			body, _ := request.Body.(*inboundRequestBody)
			bodyTimedOut := body != nil && body.timedOut.Load()
			if bodyTimedOut || errors.Is(proxyErr, errInboundRequestBodyTimeout) {
				http.Error(response, "request body timed out", http.StatusRequestTimeout)
				return
			}
			var tooLarge *http.MaxBytesError
			if errors.As(proxyErr, &tooLarge) {
				http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.logger.Printf("edge event=origin-unavailable origin=%q", route.origin.displayAlias)
			writeOffline(response, route.origin)
		},
	}
}

func (r *Registry) forwardedScheme(request *http.Request) string {
	if r.mode == ModeProxy {
		value := request.Header.Get("X-Forwarded-Proto")
		if value == "http" || value == "https" {
			return value
		}
	}
	if request.TLS != nil {
		return "https"
	}
	return "http"
}

func writeOffline(response http.ResponseWriter, origin *originRuntime) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(http.StatusBadGateway)
	lastSeen := "never"
	if !origin.lastSeenAt.IsZero() {
		lastSeen = origin.lastSeenAt.UTC().Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(response, "<!doctype html><title>Service unavailable</title><h1>%s is offline</h1><p>Last seen %s UTC.</p>", html.EscapeString(origin.displayAlias), html.EscapeString(lastSeen))
}

func removeForwarded(header http.Header) {
	header.Del("Forwarded")
	header.Del("X-Forwarded-For")
	header.Del("X-Forwarded-Host")
	header.Del("X-Forwarded-Proto")
	header.Del("X-Forwarded-Port")
	header.Del("X-Real-IP")
}

func reservedTerminalPath(parsed *url.URL, configured string) bool {
	value := parsed.Path
	for range 5 {
		if pathWithin(value, "/mesh") || pathWithin(value, configured) {
			return true
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return true
		}
		if decoded == value {
			return false
		}
		value = decoded
	}
	return true
}

func pathWithin(value, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(value, "/")
	}
	return value == prefix || strings.HasPrefix(value, prefix+"/")
}

func canonicalPublicHost(value string) (string, string, error) {
	if value == "" || strings.ContainsAny(value, "@/\\\r\n") {
		return "", "", errors.New("invalid public host")
	}
	host := value
	port := ""
	if strings.Contains(value, ":") {
		var err error
		host, port, err = net.SplitHostPort(value)
		if err != nil || host == "" || port == "" || net.JoinHostPort(host, port) != value {
			return "", "", errors.New("invalid public host port")
		}
		parsedPort, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || parsedPort == 0 || strconv.FormatUint(parsedPort, 10) != port {
			return "", "", errors.New("public host port is not canonical")
		}
	}
	if host == "" {
		return "", "", errors.New("invalid public host")
	}
	lowerHost := strings.ToLower(host)
	if host != lowerHost {
		return "", "", errors.New("public host is not canonical lowercase")
	}
	host = lowerHost
	if err := serve.ValidatePublicName(host); err != nil {
		return "", "", err
	}
	forwarded := host
	if port != "" {
		forwarded = net.JoinHostPort(host, port)
	}
	return host, forwarded, nil
}

func canonicalForwardedIP(value string) (netip.Addr, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, ",") {
		return netip.Addr{}, errors.New("edge: forwarded client IP is not one canonical address")
	}
	address, err := netip.ParseAddr(value)
	if err != nil || address.String() != value || address.Zone() != "" {
		return netip.Addr{}, errors.New("edge: forwarded client IP is not one canonical address")
	}
	return address.Unmap(), nil
}

func trustedForwardedMetadata(header http.Header) (netip.Addr, error) {
	addresses := header.Values("X-Forwarded-For")
	schemes := header.Values("X-Forwarded-Proto")
	if len(addresses) != 1 || len(schemes) != 1 || schemes[0] != "http" && schemes[0] != "https" {
		return netip.Addr{}, errors.New("edge: trusted proxy must set one canonical client IP and scheme")
	}
	address, err := canonicalForwardedIP(addresses[0])
	if err != nil {
		return netip.Addr{}, err
	}
	header.Set("X-Forwarded-For", address.String())
	header.Set("X-Forwarded-Proto", schemes[0])
	return address, nil
}

func immediateClientIP(remoteAddr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	address, err := netip.ParseAddr(host)
	return address.Unmap(), err
}

func validateDisplayAlias(alias string) error {
	if alias == "" || strings.TrimSpace(alias) != alias || len(alias) > 128 || strings.ContainsAny(alias, "\r\n") {
		return errors.New("edge: origin display alias is empty or invalid")
	}
	return nil
}

// clientRateLimiter sheds repeated requests from stable IPv4 addresses and
// IPv6 /64s. The global and per-origin concurrency channels are the hard work
// bounds; this bounded table is deliberately not an admission-control boundary.
type clientRateLimiter struct {
	mu      sync.Mutex
	entries map[netip.Addr]rateEntry
	maximum int
	limit   int
	window  time.Duration
	ring    []netip.Addr
	next    int
}

type rateEntry struct {
	start time.Time
	count int
}

func newClientRateLimiter(maximum, limit int, window time.Duration) *clientRateLimiter {
	return &clientRateLimiter{
		entries: make(map[netip.Addr]rateEntry), maximum: maximum, limit: limit, window: window,
		ring: make([]netip.Addr, maximum),
	}
}

func (l *clientRateLimiter) Allow(address netip.Addr, now time.Time) bool {
	address = clientQuotaKey(address)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, exists := l.entries[address]
	if exists && now.Sub(entry.start) >= l.window {
		l.entries[address] = rateEntry{start: now, count: 1}
		return true
	}
	if !exists {
		if len(l.entries) >= l.maximum {
			candidate := l.ring[l.next]
			if now.Sub(l.entries[candidate].start) < l.window {
				// Admit without tracking. Rotating addresses must not evict a live
				// entry and erase the limit already applied to a stable client.
				l.next = (l.next + 1) % l.maximum
				return true
			}
			delete(l.entries, candidate)
		}
		l.ring[l.next] = address
		l.next = (l.next + 1) % l.maximum
		l.entries[address] = rateEntry{start: now, count: 1}
		return true
	}
	if entry.count >= l.limit {
		return false
	}
	entry.count++
	l.entries[address] = entry
	return true
}

type proxyClientIPKey struct{}

type clientConcurrencyLimiter struct {
	mu      sync.Mutex
	active  map[netip.Addr]int
	maximum int
}

func newClientConcurrencyLimiter(maximum int) *clientConcurrencyLimiter {
	return &clientConcurrencyLimiter{active: make(map[netip.Addr]int), maximum: maximum}
}

func (l *clientConcurrencyLimiter) Acquire(address netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !address.IsValid() {
		return false
	}
	address = clientQuotaKey(address)
	if l.active[address] >= l.maximum {
		return false
	}
	l.active[address]++
	return true
}

func (l *clientConcurrencyLimiter) Release(address netip.Addr) {
	address = clientQuotaKey(address)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[address] <= 1 {
		delete(l.active, address)
		return
	}
	l.active[address]--
}

func clientQuotaKey(address netip.Addr) netip.Addr {
	address = address.Unmap()
	bits := 32
	if address.Is6() {
		bits = 64
	}
	return netip.PrefixFrom(address, bits).Masked().Addr()
}

func (r *Registry) currentGeneration() (uint64, <-chan struct{}) {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	return r.generation, r.change
}

func (r *Registry) waitForFreshRoute(ctx context.Context, publicName, prefix, originID string, generation uint64) (*proxyRoute, error) {
	for {
		nextGeneration, changed := r.currentGeneration()
		if nextGeneration > generation {
			now := r.now().UTC()
			snapshot := r.snapshot.Load()
			for index := range snapshot.routes {
				route := &snapshot.routes[index]
				if route.publicName == publicName && route.prefix == prefix && route.origin.identity == originID && route.origin.online.Load() && now.Before(route.origin.onlineUntil) {
					return route, nil
				}
			}
			generation = nextGeneration
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

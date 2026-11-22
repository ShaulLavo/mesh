package mcpnative

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	// DefaultHTTPPath is the exact endpoint path for the loopback HTTP frontend.
	DefaultHTTPPath = "/mcp"

	httpJSONMedia        = "application/json"
	httpSSEMedia         = "text/event-stream"
	httpHeaderAllow      = "Allow"
	httpHeaderAuth       = "Authorization"
	httpHeaderCache      = "Cache-Control"
	httpHeaderContent    = "Content-Type"
	httpHeaderConnection = "Connection"
	httpHeaderWWWAuth    = "WWW-Authenticate"
	httpBearerPrefix     = "Bearer "
	sseFrameOverhead     = len("event: message\ndata: \n\n")
	defaultReadTimeout   = 15 * time.Second
	defaultWriteTimeout  = 15 * time.Second
)

var (
	errIngressBudget = errors.New("MCP HTTP ingress byte budget exceeded")
	errStreamRetired = errors.New("MCP HTTP stream retired by token rotation")
)

// HTTPServerOptions configures the native loopback Streamable HTTP frontend (H6).
type HTTPServerOptions struct {
	ServerOptions
	Authority      string
	Path           string
	AllowedOrigins []string
	Token          string
	PreferSSE      bool
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// HTTPServer serves the native MCP tool set over loopback Streamable HTTP.
// It shares the native dispatch core with StdioServer.
type HTTPServer struct {
	core           *dispatcher
	toolHeaders    map[string]mcpwire.ParameterHeaders
	authority      string
	path           string
	allowedOrigins map[string]struct{}
	preferSSE      bool
	readTimeout    time.Duration
	writeTimeout   time.Duration
	maxReqs        int
	maxBytes       int
	connBytes      int
	drainWait      time.Duration
	onShutdown     func()

	// rotateMu keeps rotations one at a time, so at most one retired
	// generation is draining outside the admission accounting.
	rotateMu sync.Mutex

	mu            sync.Mutex
	token         string
	generation    uint64
	active        map[*httpActiveRequest]struct{}
	conns         map[net.Conn]*httpConn
	inflightBytes int
	closed        bool

	workers   sync.WaitGroup
	exchanges sync.WaitGroup
}

// httpConn tracks the handlers on one client connection of an http.Server
// this frontend is attached to. Its fields are guarded by HTTPServer.mu.
type httpConn struct {
	// busy is net/http's StateActive: from reading a request until its
	// response, including anything net/http writes after the handler, is
	// finished.
	busy     bool
	handlers int
	closing  bool
	drained  chan struct{}
}

type httpConnKey struct{}

type httpActiveRequest struct {
	generation    uint64
	cancel        context.CancelFunc
	retired       atomic.Bool
	ingressBytes  int
	reservedBytes int
	progressBytes int
	terminalBytes int
	// detached marks a retired exchange whose bytes left the admission
	// accounting at rotation; guarded by HTTPServer.mu.
	detached bool

	deadlineMu sync.Mutex
	rc         *http.ResponseController
}

func (e *httpActiveRequest) totalBytesLocked() int {
	return e.ingressBytes + e.reservedBytes + e.progressBytes + e.terminalBytes
}

func (e *httpActiveRequest) abortIO() {
	e.deadlineMu.Lock()
	if e.rc != nil {
		past := time.Now().Add(-time.Second)
		_ = e.rc.SetWriteDeadline(past)
		_ = e.rc.SetReadDeadline(past)
	}
	e.deadlineMu.Unlock()
}

func (e *httpActiveRequest) armWriteDeadline(timeout time.Duration) error {
	e.deadlineMu.Lock()
	defer e.deadlineMu.Unlock()
	if e.retired.Load() {
		return errStreamRetired
	}
	if e.rc != nil {
		_ = e.rc.SetWriteDeadline(time.Now().Add(timeout))
	}
	return nil
}

func (e *httpActiveRequest) clearWriteDeadline() {
	e.deadlineMu.Lock()
	if e.rc != nil && !e.retired.Load() {
		_ = e.rc.SetWriteDeadline(time.Time{})
	}
	e.deadlineMu.Unlock()
}

type httpReplyResult struct {
	reply []byte
	fault *mcpwire.Fault
}

type httpProgressSlot struct {
	mu      sync.Mutex
	pending []byte
	signal  chan struct{}
}

func newHTTPProgressSlot() *httpProgressSlot {
	return &httpProgressSlot{signal: make(chan struct{}, 1)}
}

func (p *httpProgressSlot) set(frame []byte) {
	p.mu.Lock()
	p.pending = frame
	p.mu.Unlock()
	select {
	case p.signal <- struct{}{}:
	default:
	}
}

func (p *httpProgressSlot) take() []byte {
	p.mu.Lock()
	frame := p.pending
	p.pending = nil
	p.mu.Unlock()
	return frame
}

type httpBudgetedBody struct {
	inner       io.ReadCloser
	server      *HTTPServer
	entry       *httpActiveRequest
	precharged  int
	readSoFar   int
	budgetError bool
}

func (b *httpBudgetedBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.readSoFar += n
		extra := b.readSoFar - b.precharged
		if extra > 0 {
			if !b.server.chargeIngress(b.entry, extra) {
				b.budgetError = true
				return 0, errIngressBudget
			}
			b.precharged = b.readSoFar
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read MCP HTTP body: %w", err)
	}
	return n, err //nolint:wrapcheck // Preserve io.EOF for io.ReadAll.
}

func (b *httpBudgetedBody) Close() error {
	if err := b.inner.Close(); err != nil {
		return fmt.Errorf("close MCP HTTP body: %w", err)
	}
	return nil
}

// ValidateLoopbackOrigin checks that raw is an exact http/https loopback origin
// with no path, query, fragment, or user info.
func ValidateLoopbackOrigin(raw string) error {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") {
		return fmt.Errorf("invalid origin %q: expected an exact loopback origin such as http://127.0.0.1:3000", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("invalid origin %q: expected scheme://loopback-host[:port] with no path or query", raw)
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("origin %q is not a loopback origin", raw)
	}
	return nil
}

// NewHTTPServer constructs a stateless loopback Streamable HTTP frontend.
func NewHTTPServer(opts HTTPServerOptions) (*HTTPServer, error) {
	if strings.TrimSpace(opts.Authority) == "" {
		return nil, errors.New("HTTP MCP server requires an exact loopback authority")
	}
	if opts.Token == "" {
		return nil, errors.New("HTTP MCP server requires a non-empty bearer token")
	}
	path := opts.Path
	if path == "" {
		path = DefaultHTTPPath
	}
	allowed, err := parseAllowedOrigins(opts.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	core, err := newDispatcher(opts.ServerOptions)
	if err != nil {
		return nil, err
	}
	toolHeaders, err := compileToolHeaders(core.registry)
	if err != nil {
		return nil, err
	}
	maxReqs, maxBytes, connBytes, drainWait := serverLimits(opts.ServerOptions)
	readTimeout := opts.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	writeTimeout := opts.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWriteTimeout
	}
	return &HTTPServer{
		core:           core,
		toolHeaders:    toolHeaders,
		authority:      opts.Authority,
		path:           path,
		allowedOrigins: allowed,
		preferSSE:      opts.PreferSSE,
		readTimeout:    readTimeout,
		writeTimeout:   writeTimeout,
		maxReqs:        maxReqs,
		maxBytes:       maxBytes,
		connBytes:      connBytes,
		drainWait:      drainWait,
		onShutdown:     opts.OnShutdown,
		token:          opts.Token,
		generation:     1,
		active:         make(map[*httpActiveRequest]struct{}),
		conns:          make(map[net.Conn]*httpConn),
	}, nil
}

func parseAllowedOrigins(origins []string) (map[string]struct{}, error) {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if err := ValidateLoopbackOrigin(origin); err != nil {
			return nil, err
		}
		allowed[origin] = struct{}{}
	}
	return allowed, nil
}

func compileToolHeaders(registry *Registry) (map[string]mcpwire.ParameterHeaders, error) {
	defs := registry.Definitions()
	compiled := make(map[string]mcpwire.ParameterHeaders, len(defs))
	for _, def := range defs {
		headers, err := mcpwire.CompileParameterHeaders(def.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("compile MCP parameter headers for %s: %w", def.Name, err)
		}
		compiled[def.Name] = headers
	}
	return compiled, nil
}

func serverLimits(opts ServerOptions) (maxReqs, maxBytes, connBytes int, drainWait time.Duration) {
	maxReqs = opts.MaxRequests
	if maxReqs <= 0 {
		maxReqs = DefaultMaxRequests
	}
	maxBytes = opts.MaxFrontendBytes
	if maxBytes <= 0 {
		maxBytes = DefaultFrontendBytes
	}
	connBytes = opts.MaxConnBytes
	if connBytes <= 0 {
		connBytes = DefaultConnBytes
	}
	drainWait = opts.EOFDrainTimeout
	if drainWait <= 0 {
		drainWait = defaultEOFDrainWait
	}
	return maxReqs, maxBytes, connBytes, drainWait
}

func (s *HTTPServer) originAllowed(origin string) bool {
	_, ok := s.allowedOrigins[origin]
	return ok
}

func (s *HTTPServer) httpPolicy() mcpwire.HTTPPolicy {
	return mcpwire.HTTPPolicy{
		Methods:     nativeMethods,
		ToolHeaders: func(name string) mcpwire.ParameterHeaders { return s.toolHeaders[name] },
	}
}

// Attach registers the frontend's connection hooks on hs, the server that
// will serve it. Rotation can only guarantee that old-token output stops at
// the socket for connections it has seen this way.
func (s *HTTPServer) Attach(hs *http.Server) {
	hs.ConnContext = s.connContext
	hs.ConnState = s.connState
}

func (s *HTTPServer) connContext(ctx context.Context, c net.Conn) context.Context {
	hc := &httpConn{}
	s.mu.Lock()
	s.conns[c] = hc
	s.mu.Unlock()
	return context.WithValue(ctx, httpConnKey{}, hc)
}

func (s *HTTPServer) connState(c net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == http.StateClosed || state == http.StateHijacked {
		delete(s.conns, c)
		return
	}
	if hc := s.conns[c]; hc != nil {
		hc.busy = state == http.StateActive
	}
}

// enterConn counts a handler on its connection. A connection that rotation
// is closing takes no new handlers: whatever they wrote could only be
// old-token output.
func (s *HTTPServer) enterConn(r *http.Request) (*httpConn, bool) {
	hc, _ := r.Context().Value(httpConnKey{}).(*httpConn)
	if hc == nil {
		return nil, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if hc.closing {
		return nil, false
	}
	hc.handlers++
	return hc, true
}

func (s *HTTPServer) leaveConn(hc *httpConn) {
	if hc == nil {
		return
	}
	s.mu.Lock()
	hc.handlers--
	if hc.handlers == 0 && hc.drained != nil {
		close(hc.drained)
		hc.drained = nil
	}
	s.mu.Unlock()
}

// RotateToken replaces the active bearer token and retires everything served
// under the old one. It closes every connection in the middle of a request or
// response: a closed socket cannot carry a late frame, an SSE terminator, an
// implicit status or a body drain, and an old request's deadlines cannot reach
// a reused connection. Idle keep-alive connections stay open for pooled
// clients, since their next request is authenticated against the new token.
// It returns once every handler on the closed connections has returned, so no
// output for an old-token request happens afterwards. Waits are cancelled; the
// Mesh jobs behind them are not. retired counts the admitted exchanges.
func (s *HTTPServer) RotateToken(newToken string) (generation uint64, retired int, err error) {
	if newToken == "" {
		return 0, 0, errors.New("HTTP MCP bearer token must not be empty")
	}
	s.rotateMu.Lock()
	defer s.rotateMu.Unlock()

	s.mu.Lock()
	s.token = newToken
	s.generation++
	generation = s.generation
	targets := s.detachOlderLocked(generation)
	conns, drained := s.closingConnsLocked()
	s.mu.Unlock()

	for _, entry := range targets {
		entry.cancel()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	for _, ch := range drained {
		<-ch
	}
	return generation, len(targets), nil
}

// detachOlderLocked retires exchanges from earlier generations and releases
// their admission slots and bytes at once, so new-token requests are not
// refused while retired work drains. That memory stays bounded by one
// generation's budget because rotations run one at a time and each waits for
// its retired handlers.
func (s *HTTPServer) detachOlderLocked(generation uint64) []*httpActiveRequest {
	var targets []*httpActiveRequest
	for entry := range s.active {
		if entry.generation >= generation {
			continue
		}
		entry.retired.Store(true)
		entry.detached = true
		s.inflightBytes -= entry.totalBytesLocked()
		delete(s.active, entry)
		targets = append(targets, entry)
	}
	return targets
}

func (s *HTTPServer) closingConnsLocked() ([]net.Conn, []chan struct{}) {
	conns := make([]net.Conn, 0, len(s.conns))
	var drained []chan struct{}
	for c, hc := range s.conns {
		if !hc.busy {
			continue
		}
		hc.closing = true
		conns = append(conns, c)
		if hc.handlers > 0 {
			hc.drained = make(chan struct{})
			drained = append(drained, hc.drained)
		}
	}
	return conns, drained
}

// Shutdown stops admitting new requests, cancels in-flight HTTP waits (never
// cancelling accepted Mesh jobs), and runs OnShutdown within the drain cap.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	targets := make([]*httpActiveRequest, 0, len(s.active))
	for entry := range s.active {
		targets = append(targets, entry)
	}
	s.mu.Unlock()
	for _, entry := range targets {
		entry.cancel()
		entry.abortIO()
	}

	deadline := time.Now().Add(s.drainWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	shutdownDone := startHook(s.onShutdown)
	waitBounded(deadline, func() {
		s.workers.Wait()
		s.exchanges.Wait()
	})
	waitChannelBounded(deadline, shutdownDone)
	return nil
}

func startHook(fn func()) <-chan struct{} {
	done := make(chan struct{})
	if fn == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

// ServeHTTP enforces H6 authority, Origin, method, bearer authentication,
// ingress admission before reading, section 2.2 validation, output budgets,
// and JSON/SSE delivery.
func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hc, ok := s.enterConn(r)
	if !ok {
		// Rotation is closing this connection; drop it without a status
		// rather than let net/http answer with an implicit 200.
		panic(http.ErrAbortHandler)
	}
	defer s.leaveConn(hc)
	if status := mcpwire.EndpointStatus(r, s.authority, s.path, s.originAllowed); status != http.StatusOK {
		if status == http.StatusMethodNotAllowed {
			w.Header().Set(httpHeaderAllow, http.MethodPost)
		}
		w.WriteHeader(status)
		return
	}
	authGen, ok := s.authenticate(r.Header)
	if !ok {
		writeUnauthorized(w)
		return
	}
	if r.ContentLength > mcpwire.MaxMessage {
		s.writeBoundedFaultJSON(w, &mcpwire.Fault{Code: mcpwire.InvalidRequest, Message: mcpwire.ErrTooLarge.Error()})
		return
	}
	rc := http.NewResponseController(w)
	reqCtx, entry, precharged, admitStatus := s.admitExchange(r.Context(), authGen, r.ContentLength, rc)
	if admitStatus == http.StatusUnauthorized {
		writeUnauthorized(w)
		return
	}
	if entry == nil {
		s.writeBoundedFaultJSON(w, meshFault(mcpwire.ID{}, agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish."))
		return
	}
	defer s.finishExchange(entry)

	input, ok := s.readBudgetedHTTP(w, r, rc, entry, precharged)
	if !ok {
		return
	}
	s.executeHTTPRequest(reqCtx, w, r, entry, input.Message)
}

func (s *HTTPServer) authenticate(headers http.Header) (uint64, bool) {
	token, ok := bearerToken(headers)
	if !ok {
		return 0, false
	}
	s.mu.Lock()
	expected := s.token
	gen := s.generation
	closed := s.closed
	s.mu.Unlock()
	if closed || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return 0, false
	}
	return gen, true
}

func bearerToken(headers http.Header) (string, bool) {
	var values []string
	for key, list := range headers {
		if strings.EqualFold(key, httpHeaderAuth) {
			values = append(values, list...)
		}
	}
	if len(values) != 1 {
		return "", false
	}
	token, ok := strings.CutPrefix(values[0], httpBearerPrefix)
	if !ok || token == "" || strings.TrimSpace(token) != token {
		return "", false
	}
	return token, true
}

func (s *HTTPServer) fitsExchangeLocked(entry *httpActiveRequest, delta int) bool {
	if delta <= 0 {
		return true
	}
	return entry.totalBytesLocked()+delta <= s.connBytes && s.inflightBytes+delta <= s.maxBytes
}

func (s *HTTPServer) admitExchange(parent context.Context, authGen uint64, contentLength int64, rc *http.ResponseController) (context.Context, *httpActiveRequest, int, int) {
	precharged := 0
	if contentLength > 0 {
		precharged = int(contentLength)
	}
	reserve := min(reservedErrorBytes, s.connBytes)
	needed := precharged + reserve

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.generation != authGen {
		return nil, nil, 0, http.StatusUnauthorized
	}
	if len(s.active) >= s.maxReqs || needed > s.connBytes || s.inflightBytes+needed > s.maxBytes {
		return nil, nil, 0, http.StatusOK
	}
	reqCtx, cancel := context.WithCancel(parent)
	entry := &httpActiveRequest{
		generation:    authGen,
		cancel:        cancel,
		ingressBytes:  precharged,
		reservedBytes: reserve,
		rc:            rc,
	}
	s.active[entry] = struct{}{}
	s.inflightBytes += needed
	s.exchanges.Add(1)
	return reqCtx, entry, precharged, http.StatusOK
}

func (s *HTTPServer) chargeIngress(entry *httpActiveRequest, delta int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.retired.Load() || !s.fitsExchangeLocked(entry, delta) {
		return false
	}
	entry.ingressBytes += delta
	s.inflightBytes += delta
	return true
}

func (s *HTTPServer) setIngressBytes(entry *httpActiveRequest, exact int) {
	s.mu.Lock()
	delta := exact - entry.ingressBytes
	entry.ingressBytes = exact
	s.addInflightLocked(entry, delta)
	s.mu.Unlock()
}

// addInflightLocked moves the frontend byte total with entry, unless rotation
// already took the entry's bytes out of it.
func (s *HTTPServer) addInflightLocked(entry *httpActiveRequest, delta int) {
	if !entry.detached {
		s.inflightBytes += delta
	}
}

func (s *HTTPServer) chargeProgress(entry *httpActiveRequest, size int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.retired.Load() {
		return false
	}
	delta := size - entry.progressBytes
	if delta > 0 && !s.fitsExchangeLocked(entry, delta) {
		return false
	}
	entry.progressBytes = size
	s.inflightBytes += delta
	return true
}

func (s *HTTPServer) releaseProgress(entry *httpActiveRequest) {
	s.mu.Lock()
	if entry.progressBytes > 0 {
		s.addInflightLocked(entry, -entry.progressBytes)
		entry.progressBytes = 0
	}
	s.mu.Unlock()
}

func (s *HTTPServer) finishExchange(entry *httpActiveRequest) {
	entry.cancel()
	s.mu.Lock()
	if _, ok := s.active[entry]; ok {
		delete(s.active, entry)
		s.inflightBytes -= entry.totalBytesLocked()
		entry.ingressBytes = 0
		entry.reservedBytes = 0
		entry.progressBytes = 0
		entry.terminalBytes = 0
	}
	s.mu.Unlock()
	s.exchanges.Done()
}

func (s *HTTPServer) readBudgetedHTTP(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, entry *httpActiveRequest, precharged int) (mcpwire.HTTPInput, bool) {
	budgetBody := &httpBudgetedBody{
		inner:      r.Body,
		server:     s,
		entry:      entry,
		precharged: precharged,
	}
	r.Body = budgetBody

	_ = rc.SetReadDeadline(time.Now().Add(s.readTimeout))
	input, err := mcpwire.ReadHTTP(r, s.httpPolicy())
	_ = rc.SetReadDeadline(time.Time{})

	if err != nil {
		s.handleReadHTTPError(w, r, entry, budgetBody, err)
		return mcpwire.HTTPInput{}, false
	}
	if entry.retired.Load() {
		writeUnauthorized(w)
		return mcpwire.HTTPInput{}, false
	}
	if input.Status != http.StatusOK {
		s.setIngressBytes(entry, 0)
		s.writeHTTPInputResponse(w, entry, input)
		return mcpwire.HTTPInput{}, false
	}
	s.setIngressBytes(entry, len(input.Message.Raw()))
	return input, true
}

func (s *HTTPServer) handleReadHTTPError(w http.ResponseWriter, r *http.Request, entry *httpActiveRequest, body *httpBudgetedBody, err error) {
	s.setIngressBytes(entry, 0)
	if entry.retired.Load() {
		writeUnauthorized(w)
		return
	}
	if body.budgetError || errors.Is(err, errIngressBudget) {
		s.writeBoundedFaultJSON(w, meshFault(mcpwire.ID{}, agentapi.ResourceLimit, true, "Frontend request byte budget exceeded; retry after active requests finish."))
		return
	}
	if r.Context().Err() == nil {
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (s *HTTPServer) executeHTTPRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, entry *httpActiveRequest, msg mcpwire.Message) {
	token := extractProgressToken(msg)
	progress := newHTTPProgressSlot()
	done := make(chan httpReplyResult, 1)
	notify := s.newProgressNotifier(ctx, entry, token, progress)

	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		reply, fault := s.core.handleMessage(ctx, msg, notify)
		done <- httpReplyResult{reply: reply, fault: fault}
	}()

	s.streamHTTPResponse(ctx, w, entry, msg.ID(), wantsSSE(r.Header, s.preferSSE), progress, done)
}

func (s *HTTPServer) newProgressNotifier(ctx context.Context, entry *httpActiveRequest, token json.RawMessage, progress *httpProgressSlot) func(ProgressUpdate) {
	return func(update ProgressUpdate) {
		if len(token) == 0 || ctx.Err() != nil || entry.retired.Load() {
			return
		}
		raw, ok := marshalProgressNotification(token, update)
		if !ok {
			return
		}
		if !s.chargeProgress(entry, len(raw)+sseFrameOverhead) {
			return
		}
		progress.set(raw)
	}
}

func (s *HTTPServer) streamHTTPResponse(
	ctx context.Context,
	w http.ResponseWriter,
	entry *httpActiveRequest,
	id mcpwire.ID,
	preferSSE bool,
	progress *httpProgressSlot,
	done <-chan httpReplyResult,
) {
	sseStarted := false
	for {
		select {
		case <-progress.signal:
			if !s.flushProgressFrame(ctx, w, entry, &sseStarted, progress) {
				return
			}
		case res := <-done:
			s.finishHTTPStream(ctx, w, entry, sseStarted, preferSSE, id, res)
			return
		case <-ctx.Done():
			s.handleStreamCancel(w, entry, sseStarted)
			return
		}
	}
}

func (s *HTTPServer) handleStreamCancel(w http.ResponseWriter, entry *httpActiveRequest, sseStarted bool) {
	s.releaseProgress(entry)
	if entry.retired.Load() && !sseStarted {
		writeUnauthorized(w)
	}
}

func (s *HTTPServer) flushProgressFrame(ctx context.Context, w http.ResponseWriter, entry *httpActiveRequest, sseStarted *bool, progress *httpProgressSlot) bool {
	frame := progress.take()
	if len(frame) == 0 {
		s.releaseProgress(entry)
		return true
	}
	if ctx.Err() != nil || entry.retired.Load() {
		s.releaseProgress(entry)
		return false
	}
	err := s.writeSSEEvent(w, entry, sseStarted, frame)
	s.releaseProgress(entry)
	if err != nil {
		entry.cancel()
		return false
	}
	return !entry.retired.Load()
}

func (s *HTTPServer) finishHTTPStream(ctx context.Context, w http.ResponseWriter, entry *httpActiveRequest, sseStarted, preferSSE bool, id mcpwire.ID, res httpReplyResult) {
	s.releaseProgress(entry)
	if entry.retired.Load() {
		if !sseStarted {
			writeUnauthorized(w)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	s.writeTerminalHTTP(w, entry, sseStarted, preferSSE, id, res)
}

func (s *HTTPServer) writeSSEEvent(w http.ResponseWriter, entry *httpActiveRequest, started *bool, frame []byte) error {
	if err := entry.armWriteDeadline(s.writeTimeout); err != nil {
		return err
	}
	defer entry.clearWriteDeadline()

	if !*started {
		w.Header().Set(httpHeaderContent, httpSSEMedia)
		w.Header().Set(httpHeaderCache, "no-store")
		w.WriteHeader(http.StatusOK)
		*started = true
	}
	if err := mcpwire.WriteSSE(w, frame); err != nil {
		return fmt.Errorf("write MCP SSE event: %w", err)
	}
	if entry.rc != nil {
		if err := entry.rc.Flush(); err != nil {
			return fmt.Errorf("flush MCP SSE event: %w", err)
		}
	}
	return nil
}

func (s *HTTPServer) writeTerminalHTTP(w http.ResponseWriter, entry *httpActiveRequest, sseStarted, preferSSE bool, id mcpwire.ID, res httpReplyResult) {
	frame, fallback, status := prepareTerminalFrames(id, res.reply, res.fault)
	if len(frame) == 0 {
		frame = fallback
		status = http.StatusOK
	}
	useSSE := sseStarted || (preferSSE && status == http.StatusOK)
	frame, status = s.reserveTerminalBytes(entry, frame, fallback, status, useSSE)
	if entry.retired.Load() {
		if !sseStarted {
			writeUnauthorized(w)
		}
		return
	}
	if sseStarted {
		if status == http.StatusOK {
			_ = s.writeSSEEvent(w, entry, &sseStarted, frame)
		}
		return
	}
	if preferSSE && status == http.StatusOK {
		_ = s.writeSSEEvent(w, entry, &sseStarted, frame)
		return
	}
	if err := entry.armWriteDeadline(s.writeTimeout); err != nil {
		return
	}
	defer entry.clearWriteDeadline()
	w.Header().Set(httpHeaderContent, httpJSONMedia)
	w.Header().Set(httpHeaderCache, "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(frame)
	if entry.rc != nil {
		_ = entry.rc.Flush()
	}
}

func (s *HTTPServer) reserveTerminalBytes(entry *httpActiveRequest, frame, fallback []byte, status int, useSSE bool) ([]byte, int) {
	overhead := 0
	if useSSE {
		overhead = sseFrameOverhead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Worker has finished with the decoded request bytes; switch the exchange's
	// reservation from ingress + fallback reserve to the exact outgoing frame.
	s.addInflightLocked(entry, -(entry.ingressBytes + entry.reservedBytes))
	entry.ingressBytes = 0
	entry.reservedBytes = 0

	wireSize := len(frame) + overhead
	if !s.fitsExchangeLocked(entry, wireSize) {
		frame = fallback
		status = http.StatusOK
		wireSize = len(frame) + overhead
	}
	entry.terminalBytes = wireSize
	s.addInflightLocked(entry, wireSize)
	return frame, status
}

// skipRequestBody prepares a reply sent without reading the request body.
// net/http would otherwise drain that body around the reply, waiting without
// a deadline on a client that declared a body and never sent it. Closing the
// connection skips the drain before the reply, and an expired read deadline
// ends the one after it at once.
func skipRequestBody(w http.ResponseWriter) {
	w.Header().Set(httpHeaderConnection, "close")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
}

func writeUnauthorized(w http.ResponseWriter) {
	skipRequestBody(w)
	w.Header().Set(httpHeaderWWWAuth, "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
}

func (s *HTTPServer) writeHTTPInputResponse(w http.ResponseWriter, entry *httpActiveRequest, input mcpwire.HTTPInput) {
	if input.Fault == nil {
		w.WriteHeader(input.Status)
		return
	}
	if err := entry.armWriteDeadline(s.writeTimeout); err != nil {
		return
	}
	defer entry.clearWriteDeadline()
	raw := encodeBoundedFault(input.Fault, min(mcpwire.MaxMessage, s.connBytes))
	w.Header().Set(httpHeaderContent, httpJSONMedia)
	w.Header().Set(httpHeaderCache, "no-store")
	w.WriteHeader(input.Status)
	_, _ = w.Write(raw)
}

func (s *HTTPServer) writeBoundedFaultJSON(w http.ResponseWriter, fault *mcpwire.Fault) {
	raw := encodeBoundedFault(fault, min(mcpwire.MaxMessage, s.connBytes))
	w.Header().Set(httpHeaderContent, httpJSONMedia)
	w.Header().Set(httpHeaderCache, "no-store")
	skipRequestBody(w)
	w.WriteHeader(faultHTTPStatus(fault.Code))
	_, _ = w.Write(raw)
}

// wantsSSE reports whether the server should prefer text/event-stream even when
// a request emits no progress notifications.
func wantsSSE(headers http.Header, preferSSE bool) bool {
	if preferSSE {
		return true
	}
	var jsonQ, sseQ float64
	for key, values := range headers {
		if !strings.EqualFold(key, "Accept") {
			continue
		}
		for _, value := range values {
			updateAcceptQualities(value, &jsonQ, &sseQ)
		}
	}
	return sseQ > jsonQ
}

func updateAcceptQualities(headerValue string, jsonQ, sseQ *float64) {
	for part := range strings.SplitSeq(headerValue, ",") {
		media, q := acceptMediaQuality(part)
		if media == httpJSONMedia && q > *jsonQ {
			*jsonQ = q
		}
		if media == httpSSEMedia && q > *sseQ {
			*sseQ = q
		}
	}
}

func acceptMediaQuality(part string) (string, float64) {
	media, params, err := mime.ParseMediaType(part)
	if err != nil {
		return "", 0
	}
	rawQ := params["q"]
	if rawQ == "" {
		return media, 1
	}
	q, err := strconv.ParseFloat(rawQ, 64)
	if err != nil || q <= 0 || q > 1 {
		return "", 0
	}
	return media, q
}

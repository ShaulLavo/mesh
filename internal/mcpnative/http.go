package mcpnative

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"mime"
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

	httpJSONMedia       = "application/json"
	httpSSEMedia        = "text/event-stream"
	httpHeaderAllow     = "Allow"
	httpHeaderAuth      = "Authorization"
	httpHeaderCache     = "Cache-Control"
	httpHeaderContent   = "Content-Type"
	httpHeaderWWWAuth   = "WWW-Authenticate"
	httpBearerPrefix    = "Bearer "
	defaultWriteTimeout = 15 * time.Second
)

// HTTPServerOptions configures the native loopback Streamable HTTP frontend (H6).
type HTTPServerOptions struct {
	ServerOptions
	Authority      string
	Path           string
	AllowedOrigins []string
	Token          string
	PreferSSE      bool
	WriteTimeout   time.Duration
}

// HTTPServer serves the native MCP tool set over loopback Streamable HTTP.
// It shares the native dispatch core with StdioServer.
type HTTPServer struct {
	core           *dispatcher
	authority      string
	path           string
	allowedOrigins map[string]struct{}
	preferSSE      bool
	writeTimeout   time.Duration
	maxReqs        int
	maxBytes       int
	connBytes      int
	drainWait      time.Duration
	onShutdown     func()

	mu            sync.Mutex
	token         string
	generation    uint64
	active        map[*httpActiveRequest]struct{}
	inflightBytes int
	closed        bool

	workers sync.WaitGroup
}

type httpActiveRequest struct {
	generation    uint64
	cancel        context.CancelFunc
	retired       atomic.Bool
	requestBytes  int
	reservedBytes int
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
	allowed := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, origin := range opts.AllowedOrigins {
		if err := ValidateLoopbackOrigin(origin); err != nil {
			return nil, err
		}
		allowed[origin] = struct{}{}
	}
	core, err := newDispatcher(opts.ServerOptions)
	if err != nil {
		return nil, err
	}
	maxReqs, maxBytes, connBytes, drainWait := serverLimits(opts.ServerOptions)
	writeTimeout := opts.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWriteTimeout
	}
	return &HTTPServer{
		core:           core,
		authority:      opts.Authority,
		path:           path,
		allowedOrigins: allowed,
		preferSSE:      opts.PreferSSE,
		writeTimeout:   writeTimeout,
		maxReqs:        maxReqs,
		maxBytes:       maxBytes,
		connBytes:      connBytes,
		drainWait:      drainWait,
		onShutdown:     opts.OnShutdown,
		token:          opts.Token,
		generation:     1,
		active:         make(map[*httpActiveRequest]struct{}),
	}, nil
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
		ToolHeaders: s.core.toolHeader,
	}
}

// RotateToken replaces the active bearer token and cancels every in-flight
// request stream authenticated under an earlier token generation.
func (s *HTTPServer) RotateToken(newToken string) (generation uint64, retired int, err error) {
	if newToken == "" {
		return 0, 0, errors.New("HTTP MCP bearer token must not be empty")
	}
	s.mu.Lock()
	s.token = newToken
	s.generation++
	generation = s.generation
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for entry := range s.active {
		if entry.generation < generation {
			entry.retired.Store(true)
			cancels = append(cancels, entry.cancel)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return generation, len(cancels), nil
}

// Shutdown stops admitting new requests, cancels in-flight HTTP waits (never
// cancelling accepted Mesh jobs), and runs OnShutdown within the drain cap.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for entry := range s.active {
		cancels = append(cancels, entry.cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}

	deadline := time.Now().Add(s.drainWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	shutdownDone := startHook(s.onShutdown)
	waitBounded(deadline, func() { s.workers.Wait() })
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
// section 2.2 header/body validation, bounded admission, and JSON/SSE output.
func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	input, err := mcpwire.ReadHTTP(r, s.httpPolicy())
	if err != nil {
		if r.Context().Err() == nil {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	if input.Status != http.StatusOK {
		writeHTTPInputResponse(w, input)
		return
	}
	reqCtx, entry, status := s.admitRequest(r.Context(), authGen, input.Message)
	if status == http.StatusUnauthorized {
		writeUnauthorized(w)
		return
	}
	if entry == nil {
		writeHTTPFault(w, meshFault(input.Message.ID(), agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish."))
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

func (s *HTTPServer) admitRequest(parent context.Context, authGen uint64, msg mcpwire.Message) (context.Context, *httpActiveRequest, int) {
	rawBytes := len(msg.Raw())
	reserve := faultReservation(msg.ID())
	needed := rawBytes + reserve

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.generation != authGen {
		return nil, nil, http.StatusUnauthorized
	}
	if len(s.active) >= s.maxReqs || needed > s.connBytes || s.inflightBytes+needed > s.maxBytes {
		return nil, nil, http.StatusOK
	}
	reqCtx, cancel := context.WithCancel(parent)
	entry := &httpActiveRequest{
		generation:    authGen,
		cancel:        cancel,
		requestBytes:  rawBytes,
		reservedBytes: reserve,
	}
	s.active[entry] = struct{}{}
	s.inflightBytes += needed
	s.workers.Add(1)
	return reqCtx, entry, http.StatusOK
}

func (s *HTTPServer) releaseRequest(entry *httpActiveRequest) {
	s.mu.Lock()
	if _, ok := s.active[entry]; ok {
		delete(s.active, entry)
		s.inflightBytes -= entry.requestBytes + entry.reservedBytes
	}
	s.mu.Unlock()
	s.workers.Done()
}

func (s *HTTPServer) executeHTTPRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, entry *httpActiveRequest, msg mcpwire.Message) {
	defer entry.cancel()
	token := extractProgressToken(msg)
	progress := newHTTPProgressSlot()
	done := make(chan httpReplyResult, 1)

	go func() {
		defer s.releaseRequest(entry)
		notify := func(update ProgressUpdate) {
			if len(token) == 0 || ctx.Err() != nil {
				return
			}
			if raw, ok := marshalProgressNotification(token, update); ok {
				progress.set(raw)
			}
		}
		reply, fault := s.core.handleMessage(ctx, msg, notify)
		done <- httpReplyResult{reply: reply, fault: fault}
	}()

	s.streamHTTPResponse(ctx, w, entry, msg.ID(), wantsSSE(r.Header, s.preferSSE), progress, done)
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
	rc := http.NewResponseController(w)
	sseStarted := false
	for {
		select {
		case <-progress.signal:
			if !s.flushProgressFrame(ctx, w, rc, entry, &sseStarted, progress) {
				return
			}
		case res := <-done:
			s.finishHTTPStream(ctx, w, rc, entry, sseStarted, preferSSE, id, res)
			return
		case <-ctx.Done():
			if entry.retired.Load() && !sseStarted {
				writeUnauthorized(w)
			}
			return
		}
	}
}

func (s *HTTPServer) flushProgressFrame(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, entry *httpActiveRequest, sseStarted *bool, progress *httpProgressSlot) bool {
	frame := progress.take()
	if len(frame) == 0 || ctx.Err() != nil {
		return true
	}
	if err := s.writeSSEEvent(w, rc, sseStarted, frame); err != nil {
		entry.cancel()
		return false
	}
	return true
}

func (s *HTTPServer) finishHTTPStream(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, entry *httpActiveRequest, sseStarted, preferSSE bool, id mcpwire.ID, res httpReplyResult) {
	if entry.retired.Load() {
		if !sseStarted {
			writeUnauthorized(w)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	s.writeTerminalHTTP(w, rc, sseStarted, preferSSE, id, res)
}

func (s *HTTPServer) writeSSEEvent(w http.ResponseWriter, rc *http.ResponseController, started *bool, frame []byte) error {
	if !*started {
		w.Header().Set(httpHeaderContent, httpSSEMedia)
		w.Header().Set(httpHeaderCache, "no-store")
		w.WriteHeader(http.StatusOK)
		*started = true
	}
	_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	if err := mcpwire.WriteSSE(w, frame); err != nil {
		return fmt.Errorf("write MCP SSE event: %w", err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush MCP SSE event: %w", err)
	}
	return nil
}

func (s *HTTPServer) writeTerminalHTTP(w http.ResponseWriter, rc *http.ResponseController, sseStarted, preferSSE bool, id mcpwire.ID, res httpReplyResult) {
	frame, fallback, status := prepareTerminalFrames(id, res.reply, res.fault)
	if len(frame) == 0 {
		frame = fallback
		status = http.StatusOK
	}
	if sseStarted {
		if status == http.StatusOK {
			_ = s.writeSSEEvent(w, rc, &sseStarted, frame)
		}
		return
	}
	if preferSSE && status == http.StatusOK {
		_ = s.writeSSEEvent(w, rc, &sseStarted, frame)
		return
	}
	_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	w.Header().Set(httpHeaderContent, httpJSONMedia)
	w.Header().Set(httpHeaderCache, "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(frame)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set(httpHeaderWWWAuth, "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
}

func writeHTTPInputResponse(w http.ResponseWriter, input mcpwire.HTTPInput) {
	if input.Fault == nil {
		w.WriteHeader(input.Status)
		return
	}
	raw, err := input.Fault.Reply()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set(httpHeaderContent, httpJSONMedia)
	w.Header().Set(httpHeaderCache, "no-store")
	w.WriteHeader(input.Status)
	_, _ = w.Write(raw)
}

func writeHTTPFault(w http.ResponseWriter, fault *mcpwire.Fault) {
	raw, err := fault.Reply()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set(httpHeaderContent, httpJSONMedia)
	w.Header().Set(httpHeaderCache, "no-store")
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

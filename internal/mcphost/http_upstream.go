package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/shaul/mesh/internal/mcpwire"
)

func init() { newHTTPUpstream = openHTTPUpstream }

var errHTTPUpstreamClosed = errors.New("mcp upstream: closed")

type schemaRefreshedKey struct{}

// WithSchemaRefreshed marks the one retry that follows a header mismatch and a
// schema refresh. On it the upstream delivers a second mismatch to the caller
// as the terminal error instead of returning ErrHeaderMismatch again.
func WithSchemaRefreshed(ctx context.Context) context.Context {
	return context.WithValue(ctx, schemaRefreshedKey{}, true)
}

func schemaRefreshed(ctx context.Context) bool {
	refreshed, _ := ctx.Value(schemaRefreshedKey{}).(bool)
	return refreshed
}

// httpUpstream is the owning daemon's Streamable HTTP client for one hosted
// server. It sends one POST per request and carries no session or resume state.
type httpUpstream struct {
	client      *httpClient
	toolHeaders func(string) (mcpwire.ParameterHeaders, bool)
	closed      chan struct{}
	closeOnce   sync.Once
}

func openHTTPUpstream(config HTTPUpstreamConfig) (Upstream, error) {
	endpoint, err := parseHTTPEndpoint(config.URL)
	if err != nil {
		return nil, err
	}
	credentials, err := validCredentials(config.Headers)
	if err != nil {
		return nil, err
	}
	return &httpUpstream{client: newHTTPClient(endpoint, credentials), toolHeaders: config.ToolHeaders, closed: make(chan struct{})}, nil
}

func validCredentials(headers http.Header) ([]credentialHeader, error) {
	credentials := make([]credentialHeader, 0, len(headers))
	seen := make(map[string]struct{}, len(headers))
	for name, values := range headers {
		lower := strings.ToLower(name)
		if _, duplicate := seen[lower]; duplicate {
			return nil, fmt.Errorf("MCP upstream credential header %q is listed twice", name)
		}
		seen[lower] = struct{}{}
		if !mcpwire.CredentialHeader(name) {
			return nil, fmt.Errorf("MCP upstream credential header %q is reserved or invalid", name)
		}
		if len(values) != 1 || !credentialValue(values[0]) {
			return nil, fmt.Errorf("MCP upstream credential header %q needs exactly one well-formed value", name)
		}
		credentials = append(credentials, credentialHeader{name: name, value: values[0]})
	}
	return credentials, nil
}

// credentialValue refuses what could split a header or smuggle a second one.
func credentialValue(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for i := range len(value) {
		if c := value[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

func (u *httpUpstream) Done() <-chan struct{} { return u.closed }

func (u *httpUpstream) isClosed() bool {
	select {
	case <-u.closed:
		return true
	default:
		return false
	}
}

func (u *httpUpstream) Err() error {
	if !u.isClosed() {
		return nil
	}
	return errHTTPUpstreamClosed
}

// Close cancels every call in flight. It never touches the server process.
func (u *httpUpstream) Close() error {
	u.closeOnce.Do(func() { close(u.closed) })
	u.client.transport.CloseIdleConnections()
	return nil
}

// cancelOnClose ends one call when the upstream closes or the call finishes.
func (u *httpUpstream) cancelOnClose(ctx context.Context, cancel context.CancelFunc) {
	select {
	case <-u.closed:
		cancel()
	case <-ctx.Done():
	}
}

// Call posts request and passes its events to sink in order. Cancelling ctx,
// or closing the upstream, closes the connection, which cancels the request;
// nothing reaches sink afterwards.
//
// A call that fails before the request is written returns ErrNotSent. A
// failure after that, without a terminal response, returns ErrOutcomeUnknown
// wrapping the cause (mcpwire.ErrInterrupted, ErrTooLarge, ErrPeerMessage or
// an *HTTPStatusError). A confirmed parameter header mismatch returns
// ErrHeaderMismatch and delivers nothing; see WithSchemaRefreshed.
func (u *httpUpstream) Call(ctx context.Context, request mcpwire.Message, sink func(mcpwire.Event) error) error {
	if u.isClosed() {
		return notSent(errHTTPUpstreamClosed)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go u.cancelOnClose(callCtx, cancel)
	stream, err := u.client.do(callCtx, httpCall{request: request, toolHeaders: u.headersFor(request), afterRefresh: schemaRefreshed(ctx)})
	if err != nil {
		return u.callError(ctx, err)
	}
	queue := newEventQueue()
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		pump(stream, queue)
	}()
	// Closing the stream ends the pump, so no goroutine outlives the call.
	defer func() {
		stream.close()
		<-pumped
	}()
	return u.callError(ctx, deliver(callCtx, queue, sink))
}

func deliver(ctx context.Context, queue *eventQueue, sink func(mcpwire.Event) error) error {
	for {
		event, err := queue.take(ctx)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("MCP upstream call: %w", ctx.Err())
		}
		err = sink(event)
		queue.release(event)
		if err != nil {
			return &sinkFailure{err}
		}
		if event.Terminal {
			return nil
		}
	}
}

// sinkFailure keeps the caller's own error apart from transport failures.
type sinkFailure struct{ err error }

func (f *sinkFailure) Error() string { return f.err.Error() }
func (f *sinkFailure) Unwrap() error { return f.err }

// callError classifies a failure for the router. A caller that cancelled knows
// the outcome, so its own context error passes through unchanged.
func (u *httpUpstream) callError(ctx context.Context, err error) error {
	var sink *sinkFailure
	switch {
	case err == nil:
		return nil
	case errors.As(err, &sink):
		return sink.err
	case ctx.Err() != nil:
		return fmt.Errorf("MCP upstream call: %w", ctx.Err())
	case isFinalCallError(err):
		return err
	case u.isClosed():
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, errHTTPUpstreamClosed)
	default:
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
}

// isFinalCallError reports errors that already say what happened to the
// request, or that came from the caller's own sink.
func isFinalCallError(err error) bool {
	return errors.Is(err, ErrNotSent) || errors.Is(err, ErrSlowConsumer) ||
		errors.Is(err, ErrHeaderMismatch) || errors.Is(err, ErrOutcomeUnknown)
}

func (u *httpUpstream) headersFor(request mcpwire.Message) mcpwire.ParameterHeaders {
	if request.Method() != mcpwire.MethodCallTool || u.toolHeaders == nil {
		return mcpwire.ParameterHeaders{}
	}
	var params struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(request.Params(), &params) != nil {
		return mcpwire.ParameterHeaders{}
	}
	headers, _ := u.toolHeaders(params.Name)
	return headers
}

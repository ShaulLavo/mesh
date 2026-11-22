package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/shaul/mesh/internal/mcpwire"
)

// maxUpstreamResponseHeader bounds the status line and headers of a reply.
const maxUpstreamResponseHeader = 64 << 10

var (
	// ErrNotSent marks a failure before any byte of the request reached the
	// upstream, so the caller can treat the request as never delivered.
	ErrNotSent = errors.New("MCP request was not sent")
	// ErrRedirect is a 3xx reply. Mesh never follows one, because a redirect
	// would move the request and its credentials away from the approved endpoint.
	ErrRedirect = errors.New("MCP upstream answered with a redirect")
)

func notSent(err error) error { return fmt.Errorf("%w: %w", ErrNotSent, err) }

// HTTPHeader is one credential header. The caller reads its value from a
// secret file; this package never sees the file or its path.
type HTTPHeader struct {
	Name  string
	Value string
}

type HTTPUpstreamConfig struct {
	// URL is the approved loopback endpoint, for example http://127.0.0.1:8080/mcp.
	URL         string
	Credentials []HTTPHeader
}

// HTTPUpstream is the owning daemon's Streamable HTTP client for one hosted
// server. It sends one POST per message and carries no session or resume state.
type HTTPUpstream struct {
	endpoint    httpEndpoint
	credentials []HTTPHeader
	transport   *http.Transport
}

func NewHTTPUpstream(config HTTPUpstreamConfig) (*HTTPUpstream, error) {
	endpoint, err := parseHTTPEndpoint(config.URL)
	if err != nil {
		return nil, err
	}
	credentials, err := validCredentials(config.Credentials)
	if err != nil {
		return nil, err
	}
	return &HTTPUpstream{endpoint: endpoint, credentials: credentials, transport: newUpstreamTransport(endpoint)}, nil
}

func newUpstreamTransport(endpoint httpEndpoint) *http.Transport {
	return &http.Transport{
		// Environment proxies would carry the request off the machine.
		Proxy:       nil,
		DialContext: endpoint.dial,
		// A reused connection that the server closed leaves the request's
		// fate unknown. A fresh loopback connection costs microseconds and
		// keeps every transport error meaningful.
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: maxUpstreamResponseHeader,
	}
}

func validCredentials(headers []HTTPHeader) ([]HTTPHeader, error) {
	seen := make(map[string]struct{}, len(headers))
	for _, header := range headers {
		lower := strings.ToLower(header.Name)
		if _, duplicate := seen[lower]; duplicate {
			return nil, fmt.Errorf("MCP upstream credential header %q is listed twice", header.Name)
		}
		seen[lower] = struct{}{}
		if !mcpwire.CredentialHeader(header.Name) {
			return nil, fmt.Errorf("MCP upstream credential header %q is reserved or invalid", header.Name)
		}
		if !credentialValue(header.Value) {
			return nil, fmt.Errorf("MCP upstream credential header %q has an empty or malformed value", header.Name)
		}
	}
	return append([]HTTPHeader(nil), headers...), nil
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

// HTTPCall is one request to forward. ToolHeaders holds the selected tool's
// compiled x-mcp-header parameters and matters only for tools/call.
type HTTPCall struct {
	Request     mcpwire.Message
	ToolHeaders mcpwire.ParameterHeaders
	// AfterRefresh marks the single retry that follows a schema refresh. A
	// second header mismatch on it is final.
	AfterRefresh bool
}

// HTTPStatusError reports an upstream reply that is not an MCP response.
type HTTPStatusError struct {
	Status int
	Err    error
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("MCP upstream answered HTTP %d: %v", e.Status, e.Err)
}

func (e *HTTPStatusError) Unwrap() error { return e.Err }

// Do posts one request and returns once the response headers are validated.
// Cancelling ctx, or closing the stream, closes the connection, which is how
// Streamable HTTP cancels a request.
func (u *HTTPUpstream) Do(ctx context.Context, call HTTPCall) (*HTTPStream, error) {
	req, cancel, err := u.newRequest(ctx, call)
	if err != nil {
		return nil, err
	}
	response, err := u.transport.RoundTrip(req)
	if err != nil {
		cancel()
		return nil, upstreamError(err)
	}
	stream, err := openStream(call, response, cancel)
	if err != nil {
		_ = response.Body.Close()
		cancel()
		return nil, err
	}
	return stream, nil
}

func (u *HTTPUpstream) newRequest(ctx context.Context, call HTTPCall) (*http.Request, context.CancelFunc, error) {
	raw := call.Request.Raw()
	if call.Request.Kind() != mcpwire.Request {
		return nil, nil, notSent(errors.New("an MCP HTTP upstream accepts requests only"))
	}
	if len(raw) > mcpwire.MaxMessage {
		return nil, nil, notSent(mcpwire.ErrTooLarge)
	}
	// Built from the final body, so a rewritten request never keeps a stale header.
	headers, err := mcpwire.BuildHTTPHeaders(call.Request, call.ToolHeaders)
	if err != nil {
		return nil, nil, notSent(err)
	}
	for _, credential := range u.credentials {
		headers.Set(credential.Name, credential.Value)
	}
	callCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, u.endpoint.url.String(), bytes.NewReader(raw))
	if err != nil {
		cancel()
		return nil, nil, notSent(errors.New("build MCP upstream request"))
	}
	req.Header = headers
	return req, cancel, nil
}

// upstreamError keeps the dial classification and drops the URL the standard
// library would otherwise print.
func upstreamError(err error) error {
	if errors.Is(err, ErrNotSent) {
		return err
	}
	return fmt.Errorf("MCP upstream request: %w", err)
}

func openStream(call HTTPCall, response *http.Response, cancel context.CancelFunc) (*HTTPStream, error) {
	status := response.StatusCode
	if status >= 300 && status < 400 {
		return nil, &HTTPStatusError{Status: status, Err: ErrRedirect}
	}
	// Mesh never asks for a content coding, so one would hide the byte count.
	if encoding := response.Header.Values("Content-Encoding"); len(encoding) > 0 && !identityEncoding(encoding) {
		return nil, &HTTPStatusError{Status: status, Err: errors.New("MCP upstream used a content coding")}
	}
	reply, err := mcpwire.OpenHTTPResponse(call.Request, response)
	if err != nil && status != http.StatusOK {
		return nil, &HTTPStatusError{Status: status, Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("open MCP upstream response: %w", err)
	}
	return &HTTPStream{response: reply, body: response.Body, cancel: cancel, call: call, status: status}, nil
}

func identityEncoding(values []string) bool {
	for _, value := range values {
		if !strings.EqualFold(strings.TrimSpace(value), "identity") {
			return false
		}
	}
	return true
}

// HTTPStream carries the correlated notifications and the one terminal
// message of a request. A single goroutine reads it; Close may come from any.
type HTTPStream struct {
	response *mcpwire.HTTPResponse
	body     io.Closer
	cancel   context.CancelFunc
	call     HTTPCall
	status   int
	refresh  bool
}

// Next returns the next validated event. After the terminal event it keeps
// reading to the end of the response, so a stray frame after the terminal one
// is an ErrPeerMessage and a clean end is io.EOF. A caller that does not need
// that check closes the stream after the terminal event instead.
//
// A response that ends early returns mcpwire.ErrInterrupted, and an oversize
// message returns mcpwire.ErrTooLarge.
func (s *HTTPStream) Next() (mcpwire.Event, error) {
	event, err := s.response.Next()
	if errors.Is(err, io.EOF) {
		return mcpwire.Event{}, io.EOF
	}
	if err != nil {
		return mcpwire.Event{}, fmt.Errorf("MCP upstream response: %w", err)
	}
	if event.Terminal {
		s.refresh = s.confirmsHeaderMismatch(event.Message)
	}
	return event, nil
}

// SchemaRefresh reports, once the terminal event has been read, that the
// upstream rejected the original request for its parameter headers before
// running it. The caller may refresh the tool list for the same service
// revision, instance generation and credential scope, rebuild the request's
// ToolHeaders and send one more call with AfterRefresh set. Nothing ran
// upstream, so the retry is not a replay. Every other failure stays final.
func (s *HTTPStream) SchemaRefresh() bool { return s.refresh }

func (s *HTTPStream) confirmsHeaderMismatch(terminal mcpwire.Message) bool {
	if s.call.AfterRefresh || s.status != http.StatusBadRequest || s.call.Request.Method() != mcpwire.MethodCallTool {
		return false
	}
	var failure struct {
		Code int `json:"code"`
	}
	return json.Unmarshal(terminal.ErrorObject(), &failure) == nil && failure.Code == mcpwire.HeaderMismatch
}

// Close cancels the request and closes the response stream. It is safe to
// call more than once and after the stream ended.
func (s *HTTPStream) Close() error {
	s.cancel()
	if err := s.body.Close(); err != nil {
		return fmt.Errorf("close MCP upstream response: %w", err)
	}
	return nil
}

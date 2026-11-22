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

// ErrRedirect is a 3xx reply. Mesh never follows one, because a redirect
// would move the request and its credentials away from the approved endpoint.
var ErrRedirect = errors.New("MCP upstream answered with a redirect")

func notSent(err error) error { return fmt.Errorf("%w: %w", ErrNotSent, err) }

// HTTPStatusError reports an upstream reply that is not an MCP response.
type HTTPStatusError struct {
	Status int
	Err    error
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("MCP upstream answered HTTP %d: %v", e.Status, e.Err)
}

func (e *HTTPStatusError) Unwrap() error { return e.Err }

type credentialHeader struct {
	name  string
	value string
}

// httpCall is one request to forward. toolHeaders holds the selected tool's
// compiled x-mcp-header parameters and matters only for tools/call.
type httpCall struct {
	request     mcpwire.Message
	toolHeaders mcpwire.ParameterHeaders
	// afterRefresh marks the single retry that follows a schema refresh. A
	// second header mismatch on it is final.
	afterRefresh bool
}

// httpClient owns the transport for one approved endpoint. It builds the
// request, posts it and validates the reply headers; the event loop above it
// decides what the caller sees.
type httpClient struct {
	endpoint    httpEndpoint
	credentials []credentialHeader
	transport   *http.Transport
}

func newHTTPClient(endpoint httpEndpoint, credentials []credentialHeader) *httpClient {
	return &httpClient{endpoint: endpoint, credentials: credentials, transport: &http.Transport{
		// Environment proxies would carry the request off the machine.
		Proxy:       nil,
		DialContext: endpoint.dial,
		// A reused connection that the server closed leaves the request's
		// fate unknown. A fresh loopback connection costs microseconds and
		// keeps every transport error meaningful.
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: maxUpstreamResponseHeader,
	}}
}

// do posts one request and returns once the response headers are validated.
// Cancelling ctx, or closing the stream, closes the connection, which is how
// Streamable HTTP cancels a request.
func (c *httpClient) do(ctx context.Context, call httpCall) (*httpStream, error) {
	req, cancel, err := c.newRequest(ctx, call)
	if err != nil {
		return nil, err
	}
	response, err := c.transport.RoundTrip(req)
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

func (c *httpClient) newRequest(ctx context.Context, call httpCall) (*http.Request, context.CancelFunc, error) {
	raw := call.request.Raw()
	if call.request.Kind() != mcpwire.Request {
		return nil, nil, notSent(errors.New("an MCP HTTP upstream accepts requests only"))
	}
	// Built from the final body, so a rewritten request never keeps a stale header.
	headers, err := mcpwire.BuildHTTPHeaders(call.request, call.toolHeaders)
	if err != nil {
		return nil, nil, notSent(err)
	}
	for _, credential := range c.credentials {
		headers.Set(credential.name, credential.value)
	}
	callCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.endpoint.url.String(), bytes.NewReader(raw))
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

func openStream(call httpCall, response *http.Response, cancel context.CancelFunc) (*httpStream, error) {
	status := response.StatusCode
	if status >= 300 && status < 400 {
		return nil, &HTTPStatusError{Status: status, Err: ErrRedirect}
	}
	// Mesh never asks for a content coding, so one would hide the byte count.
	if encoding := response.Header.Values("Content-Encoding"); len(encoding) > 0 && !identityEncoding(encoding) {
		return nil, &HTTPStatusError{Status: status, Err: errors.New("MCP upstream used a content coding")}
	}
	reply, err := mcpwire.OpenHTTPResponse(call.request, response)
	if err != nil && status != http.StatusOK {
		return nil, &HTTPStatusError{Status: status, Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("open MCP upstream response: %w", err)
	}
	return &httpStream{response: reply, body: response.Body, cancel: cancel, call: call, status: status}, nil
}

func identityEncoding(values []string) bool {
	for _, value := range values {
		if !strings.EqualFold(strings.TrimSpace(value), "identity") {
			return false
		}
	}
	return true
}

// httpStream carries the correlated notifications and the one terminal
// message of a request. A single goroutine reads it; close may come from any.
type httpStream struct {
	response *mcpwire.HTTPResponse
	body     io.Closer
	cancel   context.CancelFunc
	call     httpCall
	status   int
	refresh  bool
}

// next returns the next validated event. After the terminal event it keeps
// reading to the end of the response, so a stray frame after the terminal one
// is an ErrPeerMessage and a clean end is io.EOF.
func (s *httpStream) next() (mcpwire.Event, error) {
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

// confirmsHeaderMismatch recognizes the one rejection that happens before the
// tool runs: HTTP 400 with -32020 on a tool call. A stale schema that sent too
// few or too many parameter headers produces exactly this. Nothing ran
// upstream, so one retry after a schema refresh is not a replay.
func (s *httpStream) confirmsHeaderMismatch(terminal mcpwire.Message) bool {
	if s.call.afterRefresh || s.status != http.StatusBadRequest || s.call.request.Method() != mcpwire.MethodCallTool {
		return false
	}
	var failure struct {
		Code int `json:"code"`
	}
	return json.Unmarshal(terminal.ErrorObject(), &failure) == nil && failure.Code == mcpwire.HeaderMismatch
}

// close cancels the request and closes the response stream. It is safe to
// call more than once and after the stream ended.
func (s *httpStream) close() {
	s.cancel()
	_ = s.body.Close()
}

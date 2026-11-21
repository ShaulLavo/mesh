package mcpwire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	jsonMedia = "application/json"
	sseMedia  = "text/event-stream"
)

// EndpointStatus checks the configured authority, path and Origin on every
// request. The caller supplies the allowed origins and authenticates before dispatch.
func EndpointStatus(r *http.Request, authority, path string, allowedOrigin func(string) bool) int {
	origins := headerValues(r.Header, "Origin")
	if len(origins) > 0 && !validOrigin(origins, allowedOrigin) {
		return http.StatusForbidden
	}
	if r.Host != authority || r.URL.Host != "" {
		return http.StatusForbidden
	}
	if r.URL.EscapedPath() != path || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return http.StatusNotFound
	}
	if r.Method != http.MethodPost {
		return http.StatusMethodNotAllowed
	}
	return http.StatusOK
}

func validOrigin(origins []string, allowed func(string) bool) bool {
	if len(origins) != 1 || allowed == nil {
		return false
	}
	origin, err := url.Parse(origins[0])
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") &&
		origin.Host != "" && origin.User == nil && origin.Path == "" &&
		origin.RawQuery == "" && !origin.ForceQuery && !strings.Contains(origins[0], "#") &&
		allowed(origins[0])
}

func BuildHTTPHeaders(m Message, mirrored ParameterHeaders) (http.Header, error) {
	if m.kind != Request {
		return nil, fmt.Errorf("MCP HTTP headers require a request")
	}
	version, ok := decodeString(m.meta[VersionKey])
	if !ok || version != Version || !plainHeaderValue(m.method) {
		return nil, fmt.Errorf("invalid MCP protocol version or method")
	}
	headers := make(http.Header)
	headers.Set("Accept", jsonMedia+", "+sseMedia)
	headers.Set("Content-Type", jsonMedia)
	headers.Set(ProtocolHeader, version)
	headers.Set(MethodHeader, m.method)
	if requiresName(m.method) {
		name, ok := standardName(m)
		if !ok || name == "" {
			return nil, fmt.Errorf("missing MCP request name")
		}
		headers.Set(NameHeader, EncodeHeaderValue(name))
	}
	if err := mirrored.apply(m, headers, false); err != nil {
		return nil, err
	}
	return headers, nil
}

// HTTPPolicy supplies the implemented methods and the selected tool's
// compiled parameter headers. An optional predicate admits specific extension
// notifications. The pinned core has no client HTTP notifications.
type HTTPPolicy struct {
	Methods      Methods
	ToolHeaders  func(name string) ParameterHeaders
	Notification func(Message) bool
}

type HTTPInput struct {
	Message Message
	Status  int
	Fault   *Fault
}

// ReadHTTP validates a POST body after endpoint checks and authentication.
func ReadHTTP(r *http.Request, policy HTTPPolicy) (HTTPInput, error) {
	if !mediaIs(r.Header, jsonMedia) {
		return HTTPInput{Status: http.StatusUnsupportedMediaType}, nil
	}
	// Notification POSTs have no Accept requirement. Check after parsing.
	raw, err := readBounded(r.Body)
	if err != nil {
		return HTTPInput{}, err
	}
	return parseHTTPInput(raw, r.Header, policy), nil
}

func parseHTTPInput(raw []byte, headers http.Header, policy HTTPPolicy) HTTPInput {
	m, fault := Parse(raw)
	if fault != nil {
		return HTTPInput{Message: m, Status: http.StatusBadRequest, Fault: fault}
	}
	if m.kind == Notification {
		return acceptHTTPNotification(m, policy)
	}
	if m.kind != Request {
		return httpFault(m, InvalidRequest, "Expected an MCP request or notification")
	}
	return validateHTTPCall(m, headers, policy)
}

func acceptHTTPNotification(m Message, policy HTTPPolicy) HTTPInput {
	if m.method == "notifications/cancelled" || m.method == "notifications/initialized" ||
		!optionalObject(m.fields["params"]) || !optionalObject(m.params["_meta"]) ||
		policy.Notification == nil || !policy.Notification(m) {
		return httpFault(m, InvalidRequest, "Unsupported MCP HTTP notification")
	}
	return HTTPInput{Message: m, Status: http.StatusAccepted}
}

func validateHTTPCall(m Message, headers http.Header, policy HTTPPolicy) HTTPInput {
	if !standardHeadersMatch(m, headers) {
		return httpFault(m, HeaderMismatch, "Missing, malformed or mismatched MCP routing header")
	}
	if fault := validateRequest(m, policy.Methods); fault != nil {
		return HTTPInput{Message: m, Status: faultStatus(fault.Code), Fault: fault}
	}
	if !acceptsMCP(headers) {
		return HTTPInput{Message: m, Status: http.StatusNotAcceptable}
	}
	if err := validateMirrored(m, headers, policy.ToolHeaders); err != nil {
		return httpFault(m, HeaderMismatch, err.Error())
	}
	return HTTPInput{Message: m, Status: http.StatusOK}
}

func standardHeadersMatch(m Message, headers http.Header) bool {
	version, ok := decodeString(m.meta[VersionKey])
	if !ok || version == "" || !fieldToken(version) || !headerMatches(headers, ProtocolHeader, version, false) ||
		!plainHeaderValue(m.method) || !headerMatches(headers, MethodHeader, m.method, false) {
		return false
	}
	if !requiresName(m.method) {
		return len(headerValues(headers, NameHeader)) == 0
	}
	name, ok := standardName(m)
	return ok && name != "" && headerMatches(headers, NameHeader, name, true)
}

func validateMirrored(m Message, headers http.Header, lookup func(string) ParameterHeaders) error {
	if m.method != MethodCallTool || lookup == nil {
		return nil
	}
	name, _ := standardName(m)
	return lookup(name).apply(m, headers, true)
}

func httpFault(m Message, code int, message string) HTTPInput {
	return HTTPInput{Message: m, Status: faultStatus(code), Fault: m.fault(code, message)}
}

func faultStatus(code int) int {
	switch code {
	case MethodNotFound:
		return http.StatusNotFound
	case HeaderMismatch, MissingCapability, UnsupportedVersion, ParseError, InvalidRequest, InvalidParams:
		return http.StatusBadRequest
	default:
		return http.StatusOK
	}
}

func acceptsMCP(headers http.Header) bool {
	var jsonOK, sseOK bool
	for _, value := range headerValues(headers, "Accept") {
		for part := range strings.SplitSeq(value, ",") {
			media := acceptedMedia(part)
			jsonOK = jsonOK || media == jsonMedia
			sseOK = sseOK || media == sseMedia
		}
	}
	return jsonOK && sseOK
}

func acceptedMedia(value string) string {
	media, params, err := mime.ParseMediaType(value)
	if err != nil || !acceptableQuality(params["q"]) {
		return ""
	}
	return media
}

func acceptableQuality(q string) bool {
	value, err := strconv.ParseFloat(q, 64)
	return q == "" || (err == nil && value > 0 && value <= 1)
}

func mediaIs(headers http.Header, media string) bool {
	values := headerValues(headers, "Content-Type")
	if len(values) != 1 {
		return false
	}
	got, params, err := mime.ParseMediaType(values[0])
	return err == nil && got == media && (params["charset"] == "" || strings.EqualFold(params["charset"], "utf-8"))
}

func readBounded(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxMessage+1))
	if err != nil {
		return nil, fmt.Errorf("read MCP HTTP body: %w", err)
	}
	return raw, nil
}

// HTTPResponse reads one JSON response or a request-scoped SSE exchange.
// The caller owns the original response body and closes it on cancellation.
type HTTPResponse struct {
	request  Message
	response *http.Response
	stream   *SSEReader
	exchange *Exchange
	read     bool
}

func OpenHTTPResponse(request Message, response *http.Response) (*HTTPResponse, error) {
	if request.kind != Request && request.kind != Notification {
		return nil, peerError("HTTP response has no originating MCP request or notification")
	}
	if len(headerValues(response.Header, ProtocolHeader)) != 0 && !headerMatches(response.Header, ProtocolHeader, Version, false) {
		return nil, peerError("HTTP response declared a different protocol version")
	}
	if len(headerValues(response.Header, "Mcp-Session-Id")) != 0 {
		return nil, peerError("HTTP response attempted to establish a protocol session")
	}
	r := &HTTPResponse{request: request, response: response, exchange: NewExchange(request, HTTPReply)}
	if request.kind == Notification && response.StatusCode == http.StatusAccepted {
		return r, nil
	}
	if response.StatusCode == http.StatusOK && mediaIs(response.Header, sseMedia) {
		r.stream = NewSSEReader(response.Body)
		return r, nil
	}
	if !mediaIs(response.Header, jsonMedia) {
		return nil, peerError("HTTP status %d without an MCP response media type", response.StatusCode)
	}
	return r, nil
}

func (r *HTTPResponse) Next() (Event, error) {
	if r.stream != nil {
		return r.nextSSE()
	}
	if r.read {
		return Event{}, io.EOF
	}
	r.read = true
	raw, err := readBounded(r.response.Body)
	if err != nil {
		return Event{}, err
	}
	if len(raw) > MaxMessage {
		return Event{}, ErrTooLarge
	}
	if r.request.kind == Notification {
		return Event{}, r.notificationResult(raw)
	}
	return r.jsonResponse(raw)
}

func (r *HTTPResponse) nextSSE() (Event, error) {
	raw, err := r.stream.Read()
	if err != nil {
		return Event{}, r.sseReadError(err)
	}
	event, err := r.exchange.Accept(raw)
	if err != nil {
		return Event{}, err
	}
	if err := checkResponseStatus(event.Message, http.StatusOK); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (r *HTTPResponse) sseReadError(err error) error {
	if r.exchange.Finish() == nil || errors.Is(err, ErrPeerMessage) ||
		errors.Is(err, ErrTooLarge) || errors.Is(err, ErrInterrupted) {
		return err
	}
	if errors.Is(err, io.EOF) {
		return ErrInterrupted
	}
	return fmt.Errorf("%w: %w", ErrInterrupted, err)
}

func (r *HTTPResponse) jsonResponse(raw []byte) (Event, error) {
	event, err := r.exchange.Accept(raw)
	if err != nil {
		return Event{}, err
	}
	if !event.Terminal {
		return Event{}, peerError("expected a terminal JSON HTTP response")
	}
	if err := checkResponseStatus(event.Message, r.response.StatusCode); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (r *HTTPResponse) notificationResult(raw []byte) error {
	status := r.response.StatusCode
	if status == http.StatusAccepted && len(raw) == 0 {
		return io.EOF
	}
	if status >= 400 && len(bytes.TrimSpace(raw)) == 0 {
		return peerError("HTTP notification rejected with status %d", status)
	}
	return peerError("unexpected HTTP notification response with status %d", status)
}

func checkResponseStatus(message Message, status int) error {
	if message.fields["error"] == nil && status == http.StatusOK {
		return nil
	}
	if code, err := validateRPCError(message); err == nil && faultStatus(code) == status {
		return nil
	}
	return peerError("HTTP status %d inconsistent with MCP response", status)
}

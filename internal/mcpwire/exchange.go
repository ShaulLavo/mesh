package mcpwire

import (
	"bytes"
)

type ReplyTransport uint8

const (
	MeshReply ReplyTransport = iota + 1
	StdioReply
	HTTPReply
)

// Exchange validates one outstanding request after the transport has selected
// it. The shared stdio reader uses Correlation to select the exchange first.
type Exchange struct {
	request      Message
	transport    ReplyTransport
	acknowledged *subscriptionFilter
	terminal     bool
}

func NewExchange(request Message, transport ReplyTransport) *Exchange {
	return &Exchange{request: request, transport: transport}
}

type Event struct {
	Message  Message
	Terminal bool
}

// Accept checks attribution and sequence before allowing an event to be forwarded.
func (x *Exchange) Accept(raw []byte) (Event, error) {
	if x.terminal {
		return Event{}, peerError("message after terminal response")
	}
	reply, fault := Parse(raw)
	if fault != nil {
		return Event{}, peerError("%s", fault)
	}
	switch reply.kind {
	case Response:
		return x.acceptResponse(reply)
	case Notification:
		return x.acceptNotification(reply)
	case Request:
		return Event{}, peerError("unsolicited server request")
	default:
		return Event{}, peerError("unrecognized peer message kind")
	}
}

func (x *Exchange) acceptResponse(reply Message) (Event, error) {
	if !x.request.id.Equal(reply.id) {
		return Event{}, peerError("response ID does not match request")
	}
	if reply.fields["error"] != nil {
		if _, err := validateRPCError(reply); err != nil {
			return Event{}, err
		}
	} else if err := x.validateSuccess(reply); err != nil {
		return Event{}, err
	}
	x.terminal = true
	return Event{Message: reply, Terminal: true}, nil
}

func (x *Exchange) validateSuccess(reply Message) error {
	if x.request.method == MethodListen && x.acknowledged == nil {
		return peerError("subscription completed before acknowledgment")
	}
	return validateResult(reply, x.request)
}

func (x *Exchange) acceptNotification(m Message) (Event, error) {
	if !optionalObject(m.fields["params"]) || !optionalObject(m.params["_meta"]) {
		return Event{}, peerError("invalid server notification parameters")
	}
	if x.request.method == MethodListen {
		return x.acceptSubscription(m)
	}
	if m.meta[SubscriptionID] != nil || !x.validRequestNotification(m) {
		return Event{}, peerError("unexpected request notification")
	}
	return Event{Message: m}, nil
}

func (x *Exchange) validRequestNotification(m Message) bool {
	switch m.method {
	case "notifications/progress":
		token := m.params["progressToken"]
		return validToken(token) && equalToken(token, x.request.meta["progressToken"]) &&
			isNumber(m.params["progress"]) && (m.params["total"] == nil || isNumber(m.params["total"])) &&
			optionalString(m.params["message"])
	case "notifications/message":
		// Stdio logs carry no request ID or progress token, so they cannot be
		// attributed to a request on a shared hosted pipe.
		return x.transport != StdioReply && x.request.meta[LogLevelKey] != nil &&
			validLogLevel(m.params["level"]) && optionalString(m.params["logger"]) && m.params["data"] != nil
	default:
		return false
	}
}

func equalToken(a, b []byte) bool {
	if value, ok := decodeString(a); ok {
		want, matches := decodeString(b)
		return matches && want == value
	}
	return bytes.Equal(a, b)
}

// Correlation exposes the routing key supplied by an otherwise parsed stdio
// envelope. Progress tokens occupy a different namespace from request IDs.
func (m Message) Correlation() (id ID, progressToken []byte) {
	if m.kind == Response {
		return m.id, nil
	}
	if m.kind != Notification {
		return ID{}, nil
	}
	if m.method == "notifications/progress" {
		return ID{}, m.params["progressToken"]
	}
	if id, ok := cancellationID(m); ok {
		return id, nil
	}
	id, _ = ParseID(m.meta[SubscriptionID])
	return id, nil
}

type subscriptionFilter struct {
	tools, prompts, resources bool
	uris                      map[string]struct{}
}

func parseFilter(raw []byte) (subscriptionFilter, bool) {
	fields, ok := decodeObject(raw)
	if !ok || !optionalBool(fields["toolsListChanged"]) || !optionalBool(fields["promptsListChanged"]) ||
		!optionalBool(fields["resourcesListChanged"]) {
		return subscriptionFilter{}, false
	}
	filter := subscriptionFilter{
		tools:     bytes.Equal(fields["toolsListChanged"], []byte("true")),
		prompts:   bytes.Equal(fields["promptsListChanged"], []byte("true")),
		resources: bytes.Equal(fields["resourcesListChanged"], []byte("true")),
	}
	if fields["resourceSubscriptions"] != nil {
		var uris []string
		uris, ok = stringArray(fields["resourceSubscriptions"])
		filter.uris = make(map[string]struct{}, len(uris))
		for _, uri := range uris {
			filter.uris[uri] = struct{}{}
		}
	}
	return filter, ok
}

func (f subscriptionFilter) subset(requested subscriptionFilter) bool {
	return (!f.tools || requested.tools) && (!f.prompts || requested.prompts) &&
		(!f.resources || requested.resources) && allURIsRequested(f.uris, requested.uris)
}

func allURIsRequested(actual, requested map[string]struct{}) bool {
	for uri := range actual {
		if _, ok := requested[uri]; !ok {
			return false
		}
	}
	return true
}

func (x *Exchange) acceptSubscription(m Message) (Event, error) {
	if id, ok := cancellationID(m); ok && x.transport == StdioReply && id.Equal(x.request.id) {
		x.terminal = true
		return Event{Message: m, Terminal: true}, nil
	}
	id, ok := ParseID(m.meta[SubscriptionID])
	if !ok || !id.Equal(x.request.id) {
		return Event{}, peerError("notification subscription ID does not match request")
	}
	if x.acknowledged == nil {
		return x.acknowledge(m)
	}
	if !x.acknowledged.permits(m) {
		return Event{}, peerError("notification outside acknowledged subscription filter")
	}
	return Event{Message: m}, nil
}

func (x *Exchange) acknowledge(m Message) (Event, error) {
	actual, ok := parseFilter(m.params["notifications"])
	requested, _ := parseFilter(x.request.params["notifications"])
	if m.method != "notifications/subscriptions/acknowledged" || !ok || !actual.subset(requested) {
		return Event{}, peerError("invalid subscription acknowledgment")
	}
	x.acknowledged = &actual
	return Event{Message: m}, nil
}

func (f subscriptionFilter) permits(m Message) bool {
	switch m.method {
	case "notifications/tools/list_changed":
		return f.tools
	case "notifications/prompts/list_changed":
		return f.prompts
	case "notifications/resources/list_changed":
		return f.resources
	case "notifications/resources/updated":
		uri, ok := decodeString(m.params["uri"])
		_, subscribed := f.uris[uri]
		return ok && subscribed
	default:
		return false
	}
}

// Finish distinguishes graceful termination from a truncated exchange.
func (x *Exchange) Finish() error {
	if !x.terminal {
		return ErrInterrupted
	}
	return nil
}

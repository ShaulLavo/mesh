package mcphost

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
)

const tasksExtension = "io.modelcontextprotocol/tasks"

// HostedMethods are the requests a hosted route forwards. None needs an
// optional client capability at the route; the upstream asks for what it
// needs through input_required.
var HostedMethods = mcpwire.Methods{
	mcpwire.MethodDiscover: {}, mcpwire.MethodListTools: {}, mcpwire.MethodCallTool: {},
	mcpwire.MethodListPrompts: {}, mcpwire.MethodGetPrompt: {}, mcpwire.MethodListResources: {},
	mcpwire.MethodListResourceTemplates: {}, mcpwire.MethodReadResource: {}, mcpwire.MethodComplete: {},
	mcpwire.MethodListen: {},
}

// Caller is one verified application caller on one admitted grant.
type Caller struct {
	// Account is the authenticated Mesh principal: the local account or a
	// device identity.
	Account string
	// App is the verified application ID, empty for a caller without one.
	App string
	// Grant identifies the grant incarnation the request was admitted under.
	Grant string
	// Current reports whether that grant is still valid. It is checked before
	// dispatch and before every delivery.
	Current func() bool
	// Admin marks whole-account administration from the CLI, which may list,
	// stop and read diagnostics of every instance. It never grants use of
	// another caller's isolated instance.
	Admin bool
}

// Key identifies the caller for ownership. It never includes the grant, so a
// re-approved device keeps its isolated instances but not its continuations.
func (c Caller) Key() string { return c.Account + "\x00" + c.App }

func (c Caller) current() bool { return c.Current == nil || c.Current() }

// owns compares a persisted owner with this caller in constant time.
func (c Caller) owns(owner string) bool {
	return subtle.ConstantTimeCompare([]byte(owner), []byte(c.Key())) == 1
}

// Route is everything the router needs to forward one request.
type Route struct {
	Caller   Caller
	Service  string
	Revision int64
	// Account is the credential scope: the service's account label, or empty
	// for account mode none.
	Account    string
	Instance   string
	Generation string
	Upstream   Upstream
	// ServerInfo is the upstream's own implementation info from its probe.
	ServerInfo json.RawMessage
}

// Router forwards caller requests to upstreams under host-assigned IDs.
type Router struct {
	ids           *hostIDs
	continuations *continuations
	// budget bounds reply bytes queued across every stdio instance.
	budget *byteBudget
}

func NewRouter() (*Router, error) {
	ids, err := newHostIDs()
	if err != nil {
		return nil, err
	}
	return &Router{ids: ids, continuations: newContinuations(), budget: &byteBudget{limit: hostQueueBytes}}, nil
}

// Emit receives restored messages for the caller. The last call carries the
// terminal response.
type Emit func(raw []byte, terminal bool) error

// Forward sends one validated caller request and emits its restored replies.
// It returns an error only when no terminal response was emitted; the caller
// turns that error into a correlated error reply with ErrorReply.
func (r *Router) Forward(ctx context.Context, route Route, request mcpwire.Message, emit Emit) error {
	if !route.Caller.current() {
		return errRevoked
	}
	state, err := r.continuations.resolve(route, request)
	if err != nil {
		return err
	}
	upstream, restore, err := r.rewrite(request, state)
	if err != nil {
		return err
	}
	sink := func(event mcpwire.Event) error {
		if !route.Caller.current() {
			return errRevoked
		}
		raw, err := r.restore(route, request, restore, event)
		if err != nil {
			return err
		}
		return emit(raw, event.Terminal)
	}
	if err := route.Upstream.Call(ctx, upstream, sink); err != nil {
		return fmt.Errorf("%s to service %s: %w", request.Method(), route.Service, err)
	}
	return nil
}

type restoration struct {
	progress json.RawMessage
}

// rewrite replaces caller identifiers with host-assigned ones and removes
// capabilities a hosted route cannot carry.
func (r *Router) rewrite(request mcpwire.Message, state *upstreamState) (mcpwire.Message, restoration, error) {
	id, _ := r.ids.request()
	raw, err := request.WithID(id)
	if err != nil {
		return mcpwire.Message{}, restoration{}, fmt.Errorf("assign host request ID: %w", err)
	}
	var restore restoration
	if token := member(raw, paramsKey, metaKey, "progressToken"); token != nil {
		restore.progress = token
		if raw, err = setMember(raw, []string{paramsKey, metaKey, "progressToken"}, quotedJSON(r.ids.progress())); err != nil {
			return mcpwire.Message{}, restoration{}, err
		}
	}
	if member(raw, paramsKey, metaKey, mcpwire.ClientCapsKey, "extensions", tasksExtension) != nil {
		raw, err = setMember(raw, []string{paramsKey, metaKey, mcpwire.ClientCapsKey, "extensions", tasksExtension}, nil)
		if err != nil {
			return mcpwire.Message{}, restoration{}, err
		}
	}
	if raw, err = state.apply(raw); err != nil {
		return mcpwire.Message{}, restoration{}, err
	}
	m, err := reparse(raw)
	return m, restore, err
}

func (r *Router) restore(route Route, request mcpwire.Message, restore restoration, event mcpwire.Event) ([]byte, error) {
	m := event.Message
	if m.Kind() == mcpwire.Notification && event.Terminal {
		// A stdio server ends a listen by cancelling it. Downstream, a
		// graceful close is one complete result.
		raw, err := mcpwire.Complete(request, json.RawMessage(`{}`), serverInfo(route), 0)
		if err != nil {
			return nil, fmt.Errorf("complete ended listen: %w", err)
		}
		return raw, nil
	}
	raw := []byte(m.Raw())
	var err error
	if m.Kind() == mcpwire.Response {
		if raw, err = r.restoreResponse(route, request, m); err != nil {
			return nil, err
		}
	}
	if m.Method() == progressMethod {
		if raw, err = setMember(raw, []string{paramsKey, "progressToken"}, restore.progress); err != nil {
			return nil, err
		}
	}
	if request.Method() == mcpwire.MethodListen {
		original, _ := request.ID().MarshalJSON()
		if raw, err = replaceSubscription(raw, original); err != nil {
			return nil, err
		}
	}
	if len(raw) > mcpwire.MaxMessage {
		return nil, mcpwire.ErrTooLarge
	}
	return raw, nil
}

// restoreResponse gives a terminal response the caller's ID and swaps any
// upstream continuation state for a host reference.
func (r *Router) restoreResponse(route Route, request, m mcpwire.Message) ([]byte, error) {
	raw, err := m.WithID(request.ID())
	if err != nil {
		return nil, fmt.Errorf("restore caller request ID: %w", err)
	}
	// Hosted routes cannot carry task handles, so discovery never offers them.
	tasks := []string{resultKey, "capabilities", "extensions", tasksExtension}
	if request.Method() == mcpwire.MethodDiscover && member(raw, tasks...) != nil {
		if raw, err = setMember(raw, tasks, nil); err != nil {
			return nil, err
		}
	}
	return r.continuations.capture(route, request, raw)
}

func replaceSubscription(raw []byte, original json.RawMessage) ([]byte, error) {
	location := paramsKey
	if member(raw, resultKey) != nil {
		location = resultKey
	}
	if member(raw, location, metaKey, mcpwire.SubscriptionID) == nil {
		return raw, nil
	}
	return setMember(raw, []string{location, metaKey, mcpwire.SubscriptionID}, original)
}

func serverInfo(route Route) json.RawMessage {
	if route.ServerInfo != nil {
		return route.ServerInfo
	}
	return json.RawMessage(`{"name":"mesh","version":"hosted"}`)
}

var errRevoked = errors.New("mcp host: caller authorization was retired")

// Mesh error codes used in JSON-RPC error data (section 2.4).
const (
	MeshErrorCode         = -31000
	codeAccessDenied      = "access.denied"
	codeUnavailable       = "service.unavailable"
	codeFailed            = "service.failed"
	codeIncompatible      = "service.incompatible"
	codeNotFound          = "service.not_found"
	codeOutcomeUnknown    = "request.outcome_unknown"
	codeResourceLimit     = "resource.limit"
	codeStateInvalid      = "mcp.state_invalid"
	codeMessageTooLarge   = "mcp.message_too_large"
	codeInputBackpressure = "input.backpressure"
)

// Failure is a Mesh failure outside a tool result. It becomes a JSON-RPC
// error with code -31000, or -32602 for invalid continuation state.
type Failure struct {
	Code      string
	Retryable bool
	Err       error
}

func (f *Failure) Error() string { return f.Code + ": " + f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

func failure(code string, retryable bool, format string, args ...any) *Failure {
	return &Failure{Code: code, Retryable: retryable, Err: fmt.Errorf(format, args...)}
}

// Classify maps a forwarding error to its stable Mesh code.
func Classify(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	switch {
	case errors.Is(err, errRevoked):
		return &Failure{Code: codeAccessDenied, Err: err}
	case errors.Is(err, mcpwire.ErrTooLarge):
		return &Failure{Code: codeMessageTooLarge, Err: err}
	case errors.Is(err, ErrSlowConsumer):
		return &Failure{Code: codeResourceLimit, Err: err}
	case errors.Is(err, ErrOutcomeUnknown):
		return &Failure{Code: codeOutcomeUnknown, Err: err}
	case errors.Is(err, ErrNotSent):
		return &Failure{Code: codeUnavailable, Retryable: true, Err: err}
	case errors.Is(err, ErrProtocolFault):
		return &Failure{Code: codeFailed, Err: err}
	default:
		return &Failure{Code: codeOutcomeUnknown, Err: err}
	}
}

// ErrorReply builds the correlated JSON-RPC error for a failed forward.
func ErrorReply(request mcpwire.Message, err error, host, service string) ([]byte, error) {
	f := Classify(err)
	code := MeshErrorCode
	if f.Code == codeStateInvalid {
		code = mcpwire.InvalidParams
	}
	return encodeJSON(map[string]any{
		jsonrpcKey: jsonrpcVersion,
		"id":       request.ID(),
		"error": map[string]any{
			"code":    code,
			"message": f.Error(),
			"data":    map[string]any{"meshCode": f.Code, "retryable": f.Retryable, "host": host, "service": service},
		},
	})
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

const (
	mcpWriteWatchdog = 30 * time.Second
	mcpAccessDenied  = "access.denied"
	mcpInstanceKey   = "instance"
	mcpServicesKey   = "services"
)

// mcpSweepInterval is the supervisor's default; in-process tests shorten it.
var mcpSweepInterval time.Duration

// mcpController serves the hosted MCP controls of this host.
type mcpController struct {
	l        *lifecycle
	stateDir string
	sup      *mcphost.Supervisor
}

func startMCPHost(ctx context.Context, l *lifecycle, store *storage.Store, stateDir string) (*mcpController, error) {
	c := &mcpController{l: l, stateDir: stateDir}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/"
	}
	sup, err := mcphost.NewSupervisor(mcphost.Config{Store: store, Launcher: mcpLauncher{l: l}, GrantCurrent: c.grantCurrent, JobActive: c.jobActive, HostDefaultCwd: home, SweepInterval: mcpSweepInterval})
	if err != nil {
		return nil, fmt.Errorf("daemon: configure MCP hosting: %w", err)
	}
	if err := sup.Start(ctx); err != nil {
		return nil, fmt.Errorf("daemon: restore MCP hosting: %w", err)
	}
	c.sup = sup
	return c, nil
}

func (c *mcpController) Close() { c.sup.Close() }

// grantCurrent reports whether grant is the device's current approval
// line. A revoked and re-approved device has a new incarnation, so state
// bound to the old one is retired. The local account cannot be revoked.
func (c *mcpController) grantCurrent(account, grant string) bool {
	id, device := strings.CutPrefix(account, "device:")
	if !device {
		return true
	}
	incarnation, ok := identity.GrantIncarnation(c.stateDir, id)
	return ok && "device:"+incarnation == grant
}

// latchedGrant reads the device's grant incarnation and binds it to the
// connection: a connection admitted under an older incarnation is no
// longer current, so a successful check afterwards proves the two match.
func (c *mcpController) latchedGrant(peer transport.AuthenticatedPeer, id string) (string, error) {
	incarnation, ok := identity.GrantIncarnation(c.stateDir, id)
	if !ok || !peer.Current() {
		return "", access.ErrDenied
	}
	return "device:" + incarnation, nil
}

// jobActive accepts "SESSION/INSTANCE" naming a running native job owned by
// the caller: an application's own job, or account-owned work otherwise.
func (c *mcpController) jobActive(ctx context.Context, account, app, job string) bool {
	sessionID, instance, ok := strings.Cut(job, "/")
	if !ok {
		return false
	}
	journal, err := c.l.executionJournal()
	if err != nil {
		return false
	}
	b, err := c.l.executionOutput.Binding(sessionID, instance, string(c.l.host.ID))
	if err != nil || b.Mode() == execution.KindLine {
		return false
	}
	r, err := journal.Exact(ctx, b)
	if err != nil || r.Phase == executionTerminalPhase {
		return false
	}
	if app != "" {
		return r.Key.Class == execution.ApplicationClass && (appcaller.Caller{ID: appcaller.ID(app)}).Owns(r.Key.Principal)
	}
	return r.Key.Class+":"+r.Key.Principal == account
}

// caller derives the application caller from the admitted connection and
// the presented credential. A request without a credential is whole-account
// administration under the connection's existing authorization.
func (c *mcpController) caller(ctx context.Context, credential protocol.CallerCredential) (mcphost.Caller, error) {
	// The same verification every application control uses: the ID is
	// derived from the credential in the connection's authenticated scope.
	app, present, err := applicationCaller(ctx, protocol.Control{Caller: credential})
	if err != nil {
		return mcphost.Caller{}, err
	}
	scope, err := applicationCallerScope(ctx)
	if err != nil {
		return mcphost.Caller{}, err
	}
	caller := mcphost.Caller{Account: scope.Class + ":" + scope.Principal, Grant: "local", App: string(app.ID), Admin: !present}
	if scope.Class == appcaller.ScopeDevice {
		peer, _ := transport.Peer(ctx)
		if caller.Grant, err = c.latchedGrant(peer, scope.Principal); err != nil {
			return mcphost.Caller{}, err
		}
		caller.Current = peer.Current
	}
	return caller, nil
}

// serve handles one hosted MCP control. A forward takes over the connection
// and the caller returns once it ends.
func (c *mcpController) serve(ctx context.Context, conn transport.Conn, relay *clientRelay, frame protocol.Frame, kind string) (bool, error) {
	if kind == mcpframe.Forward {
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := relay.drainWriter(writeCtx)
		cancel()
		if err != nil {
			return true, err
		}
		return true, c.forward(ctx, conn, frame)
	}
	req, err := protocol.DecodeMCPServiceRequest(frame.Payload)
	response := protocol.MCPServiceResponse{Type: protocol.TypeMCPServiceResult, RequestID: req.RequestID}
	if err == nil {
		response.Result, err = c.handle(ctx, req)
	}
	if err != nil {
		response = mcpErrorResponse(req.RequestID, err)
	}
	out, err := protocol.EncodeMCP(response)
	if err != nil {
		return false, fmt.Errorf("encode %s reply: %w", kind, err)
	}
	return false, relay.enqueueAdmittedResponse(out, func() error { return workerExecutionAdmission(ctx) })
}

func mcpErrorResponse(requestID string, err error) protocol.MCPServiceResponse {
	code, retryable := mcpErrorCode(err)
	return protocol.MCPServiceResponse{Type: protocol.TypeError, RequestID: requestID, ErrorCode: code, Message: err.Error(), Retryable: retryable}
}

func mcpErrorCode(err error) (string, bool) {
	var f *mcphost.Failure
	switch {
	case errors.As(err, &f):
		return f.Code, f.Retryable
	case errors.Is(err, access.ErrDenied), errors.Is(err, mcphost.ErrDenied):
		return mcpAccessDenied, false
	case errors.Is(err, mcphost.ErrNotFound):
		return "service.not_found", false
	case errors.Is(err, mcphost.ErrLimit):
		return resourceLimitCode, false
	case errors.Is(err, mcphost.ErrConflict):
		return "service.conflict", false
	case errors.Is(err, mcphost.ErrInvalid), errors.Is(err, mcphost.ErrSecretFile), errors.As(err, new(*callerValidationError)):
		return executionUsageInvalidCode, false
	case errors.Is(err, mcphost.ErrHTTPUnavailable), errors.Is(err, mcphost.ErrManagedHTTPUnsupported):
		return "protocol.unsupported", false
	default:
		return stateUnavailableCode, true
	}
}

func (c *mcpController) handle(ctx context.Context, req protocol.MCPServiceRequest) (json.RawMessage, error) {
	caller, err := c.caller(ctx, req.Caller)
	if err != nil {
		return nil, err
	}
	target := mcphost.Target{Service: req.Service, Instance: req.Instance, New: req.New}
	switch req.Type {
	case protocol.TypeMCPCatalog:
		return mcpResult(map[string]any{"hostId": string(c.l.host.ID), mcpServicesKey: c.sup.Services(caller)}, nil)
	case protocol.TypeMCPServiceUpsert:
		return c.upsert(ctx, caller, req)
	case protocol.TypeMCPServiceDelete:
		return c.admin(caller, func() (any, error) { return map[string]any{}, c.sup.Remove(ctx, req.Service) })
	case protocol.TypeMCPServiceStart:
		view, err := c.sup.StartInstance(ctx, caller, target)
		return mcpResult(map[string]any{mcpInstanceKey: view}, err)
	case protocol.TypeMCPServiceRestart:
		view, err := c.sup.Restart(ctx, caller, target)
		return mcpResult(map[string]any{mcpInstanceKey: view}, err)
	case protocol.TypeMCPServiceStop:
		views, err := c.sup.StopInstance(ctx, caller, target)
		return mcpResult(map[string]any{"instances": views}, err)
	case protocol.TypeMCPServiceLogs:
		stderr, err := c.sup.Logs(ctx, caller, target, req.MaxBytes)
		return mcpResult(map[string]any{"stderr": stderr}, err)
	case protocol.TypeMCPServiceLease:
		lease, view, err := c.sup.Lease(ctx, caller, target, req.LeaseID, time.Duration(req.LeaseTTLMillis)*time.Millisecond, req.Job)
		return mcpResult(map[string]any{"lease": map[string]any{"id": lease.ID, "expires": lease.Expires, "job": lease.Job}, mcpInstanceKey: view}, err)
	default:
		return nil, fmt.Errorf("%w %q", errUnknownControl, req.Type)
	}
}

// admin runs definition changes, which only whole-account administration
// may make: an application caller cannot publish or remove services.
func (c *mcpController) admin(caller mcphost.Caller, run func() (any, error)) (json.RawMessage, error) {
	if !caller.Admin {
		return nil, fmt.Errorf("changing service definitions needs whole-account access: %w", access.ErrDenied)
	}
	return mcpResult(run())
}

func (c *mcpController) upsert(ctx context.Context, caller mcphost.Caller, req protocol.MCPServiceRequest) (json.RawMessage, error) {
	return c.admin(caller, func() (any, error) {
		var d mcphost.Definition
		if err := json.Unmarshal(req.Definition, &d); err != nil {
			return nil, fmt.Errorf("%w: %w", mcphost.ErrInvalid, err)
		}
		saved, err := c.sup.Publish(ctx, d, req.ExpectedRevision)
		if err != nil {
			return nil, fmt.Errorf("publish MCP service: %w", err)
		}
		view, err := c.sup.Service(caller, saved.ID)
		if err != nil {
			return nil, fmt.Errorf("show published MCP service: %w", err)
		}
		return map[string]any{"service": view}, nil
	})
}

func mcpResult(value any, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode MCP result: %w", err)
	}
	return raw, nil
}

// forward serves one H3 stream: the request, zero or more mcp.message
// notifications, then one mcp.result. The client closing its side cancels
// the exact upstream request.
func (c *mcpController) forward(ctx context.Context, conn transport.Conn, frame protocol.Frame) error {
	// A connection whose grant lapsed gets no reply at all, not even a fault.
	if err := workerExecutionAdmission(ctx); err != nil {
		return err
	}
	route, request, err := mcpframe.DecodeForward(frame)
	if err != nil {
		return writeMCPFault(conn, request, err)
	}
	if fault := mcpwire.ValidateRequest(request, mcphost.HostedMethods); fault != nil {
		return writeMCPFault(conn, request, fault)
	}
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		// Any frame or error from the client ends the stream: H3 carries one
		// request, and closing the connection is the cancellation.
		_, _ = conn.ReadFrame()
		cancel()
	}()
	defer func() { _ = conn.Close(); <-readerDone }()
	host := string(c.l.host.ID)
	caller, err := c.caller(lifetime, route.Caller)
	if err == nil {
		err = c.sup.Forward(lifetime, caller, mcphost.Target{Service: route.Service, Instance: route.Instance, New: route.New}, request, func(raw []byte, terminal bool) error {
			return writeMCPEvent(lifetime, conn, raw, terminal)
		})
	}
	if err == nil || lifetime.Err() != nil {
		return nil
	}
	reply, encodeErr := mcphost.ErrorReply(request, mcpForwardError(err), host, route.Service)
	if encodeErr != nil {
		return fmt.Errorf("encode MCP error reply: %w", encodeErr)
	}
	return writeMCPEvent(lifetime, conn, reply, true)
}

// mcpForwardError gives daemon-level refusals their Mesh codes.
func mcpForwardError(err error) error {
	var f *mcphost.Failure
	if errors.As(err, &f) {
		return err
	}
	code, retryable := mcpErrorCode(err)
	if code == stateUnavailableCode {
		return err
	}
	return &mcphost.Failure{Code: code, Retryable: retryable, Err: err}
}

func writeMCPEvent(ctx context.Context, conn transport.Conn, raw []byte, terminal bool) error {
	kind := mcpframe.Message
	if terminal {
		kind = mcpframe.Result
	}
	frame, err := mcpframe.Encode(kind, raw)
	if err != nil {
		return fmt.Errorf("encode %s: %w", kind, err)
	}
	// A client that stops reading loses its stream instead of holding the
	// daemon's writer.
	watchdog := time.AfterFunc(mcpWriteWatchdog, func() { _ = conn.Close() })
	defer watchdog.Stop()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write %s: %w", kind, err)
	}
	if err := conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("write %s: %w", kind, err)
	}
	return nil
}

// writeMCPFault answers a request that failed wire validation with its
// standard JSON-RPC error.
func writeMCPFault(conn transport.Conn, request mcpwire.Message, err error) error {
	var fault *mcpwire.Fault
	if !errors.As(err, &fault) {
		fault = &mcpwire.Fault{Code: mcpwire.InvalidRequest, Message: err.Error()}
	}
	if request.ID().Valid() {
		fault.ID = request.ID()
	}
	reply, err := fault.Reply()
	if err != nil {
		return fmt.Errorf("encode MCP fault: %w", err)
	}
	frame, err := mcpframe.Encode(mcpframe.Result, reply)
	if err != nil {
		return fmt.Errorf("encode MCP fault: %w", err)
	}
	if err := conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("write MCP fault: %w", err)
	}
	return nil
}

package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/transport"
)

// errCallerRequired reports an application control sent without an
// application credential.
var errCallerRequired = errors.New("this control needs an application identity (mesh mcp identity create NAME)")

// errCallerDenied reports an application caller reaching work another caller
// owns, or account administration sent without the explicit flag.
var errCallerDenied = fmt.Errorf("application caller does not own this work: %w", access.ErrDenied)

// callerValidationError marks a malformed credential as a usage error.
type callerValidationError struct{ err error }

func (e *callerValidationError) Error() string { return e.err.Error() }
func (e *callerValidationError) Unwrap() error { return e.err }

// applicationCallerScope returns the authenticated namespace of the
// connection behind ctx: the daemon account on the local socket, or the
// pinned identity of a current full device.
func applicationCallerScope(ctx context.Context) (appcaller.Scope, error) {
	if err := workerExecutionAdmission(ctx); err != nil {
		return appcaller.Scope{}, err
	}
	if local, _ := ctx.Value(localClientKey{}).(bool); local {
		return appcaller.Scope{Class: appcaller.ScopeLocalAccount, Principal: "daemon-account"}, nil
	}
	peer, ok := transport.Peer(ctx)
	if !ok || peer.Identity == "" {
		return appcaller.Scope{}, fmt.Errorf("application caller: trusted connection required: %w", access.ErrDenied)
	}
	return appcaller.Scope{Class: appcaller.ScopeDevice, Principal: peer.Identity}, nil
}

// applicationCaller verifies the credential in request against the
// connection that carried it. present is false when no credential was sent.
// Handlers that serve application controls call this; the M4 and M7
// frontends rely on it to bind work to its caller.
func applicationCaller(ctx context.Context, request protocol.Control) (caller appcaller.Caller, present bool, err error) {
	if request.Caller == "" {
		return appcaller.Caller{}, false, nil
	}
	scope, err := applicationCallerScope(ctx)
	if err != nil {
		return appcaller.Caller{}, true, err
	}
	caller, err = appcaller.Verify(scope, string(request.Caller))
	if err != nil {
		return appcaller.Caller{}, true, &callerValidationError{err: fmt.Errorf("application caller: %w", err)}
	}
	return caller, true, nil
}

// requireApplicationCaller is applicationCaller for controls that only an
// application may send.
func requireApplicationCaller(ctx context.Context, request protocol.Control) (appcaller.Caller, error) {
	caller, present, err := applicationCaller(ctx, request)
	if err == nil && !present {
		err = &callerValidationError{err: errCallerRequired}
	}
	return caller, err
}

// authorizeExecutionOwner is the common owner guard for raw execution
// controls on one exact receipt. An application caller reaches only its own
// jobs, and only through the job's context when it names one. A request
// without a credential keeps the existing whole-account authorization for
// account-owned jobs; for application-owned jobs it must say so explicitly
// with Administration.
func authorizeExecutionOwner(ctx context.Context, request protocol.Control, owner execution.Key) error {
	caller, present, err := applicationCaller(ctx, request)
	if err != nil {
		return err
	}
	if !present {
		if request.ExecutionContext != nil {
			return &callerValidationError{err: errCallerRequired}
		}
		if owner.Class == execution.ApplicationClass && !request.Administration {
			return errCallerDenied
		}
		return nil
	}
	if request.Administration || owner.Class != execution.ApplicationClass || !caller.Owns(owner.Principal) {
		return errCallerDenied
	}
	if request.ExecutionContext != nil && request.ExecutionContext.ContextID != owner.Context {
		return errCallerDenied
	}
	return nil
}

// rejectStrayCaller keeps a credential from riding on controls that do not
// check it, so a frontend never mistakes an unguarded path for an isolated one.
func rejectStrayCaller(request protocol.Control) error {
	if request.Caller == "" && !request.Administration && request.ExecutionContext == nil {
		return nil
	}
	// File controls check the caller when they name a context.
	if protocol.IsExecutionControl(request.Type) || protocol.IsContextControl(request.Type) || protocol.IsFileControl(request.Type) {
		return nil
	}
	return &callerValidationError{err: fmt.Errorf("%s does not accept an application caller", request.Type)}
}

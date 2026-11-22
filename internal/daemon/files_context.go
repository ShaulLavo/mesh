package daemon

import (
	"context"
	"errors"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// fileContext authorizes an execution context for a file request and returns
// its working directory. The context must belong to the application caller
// the request carries and must still be open; a closed context never resolves
// a path, so closing one ends the workspace it granted.
func (l *lifecycle) fileContext(ctx context.Context, request protocol.Control, contextID string) (string, error) {
	if !protocol.ValidContextID(contextID) {
		return "", fileFailure(agentapi.UsageInvalid, "daemon: contextId must come from a context open")
	}
	caller, err := requireApplicationCaller(ctx, request)
	if err != nil {
		return "", fileContextFailure(err)
	}
	journal, err := l.executionJournal()
	if err != nil {
		return "", fileContextFailure(err)
	}
	found, err := journal.GetContext(admittedContext(ctx), string(caller.ID), contextID)
	if err != nil {
		return "", fileContextFailure(err)
	}
	if found.ClosedAt != nil {
		return "", fileFailure(agentapi.ContextClosed, "daemon: context %s is closed", contextID)
	}
	return found.Cwd, nil
}

// fileContextFailure gives caller and context failures their stable codes.
func fileContextFailure(err error) error {
	if errors.Is(err, access.ErrDenied) {
		return &fileError{code: agentapi.AccessDenied, err: err}
	}
	if code := applicationErrorCode(err); code != "" {
		return &fileError{code: agentapi.Code(code), err: err}
	}
	return &fileError{code: agentapi.InternalError, err: err}
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// contextControl serves the context.* family. Every request needs a verified
// application caller, and every record it reaches is bound to that caller.
func (l *lifecycle) contextControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	caller, err := requireApplicationCaller(ctx, request)
	if err != nil {
		return protocol.Control{}, err
	}
	journal, err := l.executionJournal()
	if err != nil {
		return protocol.Control{}, err
	}
	params := protocol.ExecutionContextRequest{}
	if request.ExecutionContext != nil {
		params = *request.ExecutionContext
	}
	response := protocol.Control{Type: protocol.TypeContextResult, RequestID: request.RequestID}
	if request.Type == protocol.TypeContextList {
		list, err := journal.ListContexts(ctx, string(caller.ID))
		response.ContextInfos = make([]protocol.ExecutionContextInfo, 0, len(list))
		for _, c := range list {
			response.ContextInfos = append(response.ContextInfos, wireContext(c))
		}
		return response, contextError("list contexts", err)
	}
	c, err := l.contextOperation(ctx, journal, caller, request, params)
	if c.ID != "" {
		info := wireContext(c)
		response.ContextInfo = &info
	}
	if err != nil {
		return response, &contextFailure{response: response, err: err}
	}
	return response, nil
}

func (l *lifecycle) contextOperation(ctx context.Context, journal *execution.Journal, caller appcaller.Caller, request protocol.Control, params protocol.ExecutionContextRequest) (execution.Context, error) {
	owner := string(caller.ID)
	switch request.Type {
	case protocol.TypeContextOpen:
		if params.Open == nil {
			return execution.Context{}, contextUsage("context.open needs executionContext.open")
		}
		spec, err := normalizeContextSpec(*params.Open)
		if err != nil {
			return execution.Context{}, err
		}
		c, _, err := journal.OpenContext(ctx, l.contextOpenKey(caller, request.RequestKey), spec)
		return c, contextError("open context", err)
	case protocol.TypeContextReceipt:
		c, err := journal.ContextReceipt(ctx, l.contextOpenKey(caller, request.RequestKey))
		return c, contextError("read context receipt", err)
	case protocol.TypeContextGet:
		c, err := journal.GetContext(ctx, owner, params.ContextID)
		return c, contextError("context "+params.ContextID, err)
	case protocol.TypeContextUpdate:
		return l.updateContext(ctx, journal, owner, params)
	case protocol.TypeContextClose:
		c, err := journal.CloseContext(ctx, owner, params.ContextID, params.ExpectedRevision)
		return c, contextError("close context "+params.ContextID, err)
	default:
		return execution.Context{}, errUnknownControl
	}
}

func (l *lifecycle) updateContext(ctx context.Context, journal *execution.Journal, owner string, params protocol.ExecutionContextRequest) (execution.Context, error) {
	if params.ExpectedRevision == nil || params.Change == nil {
		return execution.Context{}, contextUsage("context.update needs expectedRevision and change")
	}
	change := execution.ContextChange{SetEnv: params.Change.SetEnv, UnsetEnv: params.Change.UnsetEnv, Label: params.Change.Label}
	if params.Change.Cwd != "" {
		change.Cwd = &params.Change.Cwd
	}
	for _, name := range params.Change.UnsetEnv {
		if err := validContextEnvName(name); err != nil {
			return execution.Context{}, err
		}
	}
	c, err := journal.UpdateContext(ctx, owner, params.ContextID, *params.ExpectedRevision, change, validateContextSpec)
	return c, contextError("update context "+params.ContextID, err)
}

func (l *lifecycle) contextOpenKey(caller appcaller.Caller, opaque string) execution.Key {
	return execution.Key{Host: string(l.host.ID), Class: execution.ApplicationClass, Principal: string(caller.ID), Opaque: opaque, Operation: execution.OperationContextOpen}
}

// normalizeContextSpec validates an open request and resolves the login
// shell once, so later jobs in the context use the shell it opened with.
func normalizeContextSpec(in protocol.ContextSpec) (execution.ContextSpec, error) {
	spec := execution.ContextSpec{Cwd: in.Cwd, Shell: in.Shell, Env: in.Env, Label: in.Label}
	if spec.Shell == "" {
		spec.Shell = protocol.ContextShellLogin
	}
	switch spec.Shell {
	case protocol.ContextShellLogin:
		spec.ShellPath = hostShell()
	case protocol.ContextShellSh:
		spec.ShellPath = posixShellPath
	case protocol.ContextShellNone:
	default:
		return execution.ContextSpec{}, contextUsage("shell must be login, sh or none")
	}
	if len(spec.Env) == 0 {
		spec.Env = nil
	}
	return spec, validateContextSpec(spec)
}

func validateContextSpec(spec execution.ContextSpec) error {
	if err := validContextDirectory(spec.Cwd); err != nil {
		return err
	}
	if len(spec.Label) > protocol.MaxContextLabelBytes || !validContextText(spec.Label) {
		return contextUsage("label must be valid text of at most 256 bytes")
	}
	return validContextEnv(spec.Env)
}

func validContextDirectory(dir string) error {
	if len(dir) > 4096 || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !validContextText(dir) {
		return contextUsage("cwd must be an absolute clean path on the host")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return contextUsage(fmt.Sprintf("cwd %s is not a directory on this host", dir))
	}
	return nil
}

func validContextEnv(env map[string]string) error {
	if len(env) > protocol.MaxContextEnvVars {
		return contextUsage("env holds at most 128 variables")
	}
	size := 0
	for name, value := range env {
		if err := validContextEnvName(name); err != nil {
			return err
		}
		if !validContextText(value) {
			return contextUsage(fmt.Sprintf("env %s must be valid text without NUL", name))
		}
		size += len(name) + len(value) + 2
	}
	if size > protocol.MaxContextEnvBytes {
		return contextUsage("env exceeds 32 KiB")
	}
	return nil
}

// validContextEnvName keeps Mesh's own session variables out of overlays, so
// a context cannot impersonate another session to nested Mesh commands.
func validContextEnvName(name string) error {
	if name == "" || strings.HasPrefix(name, "MESH_") {
		return contextUsage(fmt.Sprintf("env name %q is empty or reserved", name))
	}
	for i, ch := range name {
		if ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || (i > 0 && ch >= '0' && ch <= '9') {
			continue
		}
		return contextUsage(fmt.Sprintf("env name %q must match [A-Za-z_][A-Za-z0-9_]*", name))
	}
	return nil
}

func validContextText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func wireContext(c execution.Context) protocol.ExecutionContextInfo {
	names := make([]string, 0, len(c.Env))
	for name := range c.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	state := protocol.ContextStateOpen
	if c.ClosedAt != nil {
		state = protocol.ContextStateClosed
	}
	return protocol.ExecutionContextInfo{HostID: c.Host, ContextID: c.ID, Handle: protocol.ContextHandle(c.Host, c.ID), RequestKey: c.Key.Opaque, State: state, Revision: c.Revision, Cwd: c.Cwd, Shell: c.Shell, ShellPath: c.ShellPath, Label: c.Label, EnvNames: names, EnvDigest: execution.Digest(envOrEmpty(c.Env)), RunningJobs: c.RunningJobs, CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt, ClosedAt: c.ClosedAt, ExpiresAt: c.ExpiresAt()}
}

func envOrEmpty(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	return env
}

// contextFailure keeps the context record in an error reply, as an
// executionError keeps a partial receipt.
type contextFailure struct {
	response protocol.Control
	err      error
}

func (e *contextFailure) Error() string { return e.err.Error() }
func (e *contextFailure) Unwrap() error { return e.err }

type contextUsageError struct{ message string }

func (e *contextUsageError) Error() string { return "execution context: " + e.message }
func contextUsage(message string) error    { return &contextUsageError{message: message} }

func contextError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

const (
	accessDeniedCode = "access.denied"
	posixShellPath   = "/bin/sh"
)

// applicationErrorCode maps caller and context failures to agent codes.
func applicationErrorCode(err error) string {
	var usage *contextUsageError
	var invalid *callerValidationError
	switch {
	case errors.As(err, &usage), errors.As(err, &invalid):
		return executionUsageInvalidCode
	case errors.Is(err, errCallerDenied):
		return accessDeniedCode
	case errors.Is(err, execution.ErrContextNotFound):
		return "context.not_found"
	case errors.Is(err, execution.ErrContextClosed):
		return "context.closed"
	case errors.Is(err, execution.ErrContextConflict):
		return "context.conflict"
	case errors.Is(err, execution.ErrContextLimit):
		return resourceLimitCode
	default:
		return ""
	}
}

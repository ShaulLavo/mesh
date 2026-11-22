package daemon

import (
	"context"
	"encoding/json"
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

// contextListPage bounds one list reply: a page of records, and a byte
// budget well inside the frame cap even for records with maximal overlays.
const (
	contextListPage   = 64
	contextListBudget = 1 << 20
)

// admittedContext rechecks the connection's grant whenever the journal has
// waited for its lock, before any effect.
func admittedContext(ctx context.Context) context.Context {
	return execution.WithAdmission(ctx, func() error { return workerExecutionAdmission(ctx) })
}

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
	ctx = admittedContext(ctx)
	params := protocol.ExecutionContextRequest{}
	if request.ExecutionContext != nil {
		params = *request.ExecutionContext
	}
	response := protocol.Control{Type: protocol.TypeContextResult, RequestID: request.RequestID}
	if request.Type == protocol.TypeContextList {
		return l.listContexts(ctx, journal, string(caller.ID), params.Cursor, response)
	}
	c, err := l.contextOperation(ctx, journal, caller, request, params)
	if c.ID != "" {
		info := l.observedContext(ctx, journal, c)
		response.ContextInfo = &info
	}
	if err != nil {
		return response, &contextFailure{response: response, err: err}
	}
	return response, nil
}

func (l *lifecycle) listContexts(ctx context.Context, journal *execution.Journal, owner, cursor string, response protocol.Control) (protocol.Control, error) {
	if cursor != "" && !protocol.ValidContextID(cursor) {
		return response, contextUsage("cursor must come from an earlier context.list reply")
	}
	list, more, err := journal.ListContexts(ctx, owner, cursor, contextListPage)
	if err != nil {
		return response, contextError("list contexts", err)
	}
	response.ContextInfos = make([]protocol.ExecutionContextInfo, 0, len(list))
	used := 0
	for index, c := range list {
		info := l.observedContext(ctx, journal, c)
		encoded, err := json.Marshal(info)
		if err != nil {
			return response, contextError("encode context list", err)
		}
		if index > 0 && used+len(encoded) > contextListBudget {
			more = true
			break
		}
		used += len(encoded)
		response.ContextInfos = append(response.ContextInfos, info)
	}
	if more && len(response.ContextInfos) > 0 {
		response.ContextNextCursor = response.ContextInfos[len(response.ContextInfos)-1].ContextID
	}
	return response, nil
}

func (l *lifecycle) contextOperation(ctx context.Context, journal *execution.Journal, caller appcaller.Caller, request protocol.Control, params protocol.ExecutionContextRequest) (execution.Context, error) {
	owner := string(caller.ID)
	switch request.Type {
	case protocol.TypeContextOpen:
		return l.openContext(ctx, journal, caller, request.RequestKey, params.Open)
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

// openContext digests the caller's request alone. Host-dependent resolution,
// the directory check and the login shell, runs only for a new key, so a
// retry returns the original result even after the host changed.
func (l *lifecycle) openContext(ctx context.Context, journal *execution.Journal, caller appcaller.Caller, key string, open *protocol.ContextSpec) (execution.Context, error) {
	if open == nil {
		return execution.Context{}, contextUsage("context.open needs executionContext.open")
	}
	spec, err := canonicalContextSpec(*open)
	if err != nil {
		return execution.Context{}, err
	}
	resolve := func() (execution.ContextSpec, error) { return resolveContextSpec(spec) }
	c, _, err := journal.OpenContext(ctx, l.contextOpenKey(caller, key), execution.Digest(spec), resolve)
	return c, contextError("open context", err)
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
	check := func(next execution.ContextSpec) error {
		if err := validateContextSyntax(next); err != nil {
			return err
		}
		return validContextDirectory(next.Cwd)
	}
	c, err := journal.UpdateContext(ctx, owner, params.ContextID, *params.ExpectedRevision, change, check)
	return c, contextError("update context "+params.ContextID, err)
}

func (l *lifecycle) contextOpenKey(caller appcaller.Caller, opaque string) execution.Key {
	return execution.Key{Host: string(l.host.ID), Class: execution.ApplicationClass, Principal: string(caller.ID), Opaque: opaque, Operation: execution.OperationContextOpen}
}

// canonicalContextSpec validates and normalizes the caller's open request
// without consulting the host.
func canonicalContextSpec(in protocol.ContextSpec) (protocol.ContextSpec, error) {
	if in.Shell == "" {
		in.Shell = protocol.ContextShellLogin
	}
	if in.Shell != protocol.ContextShellLogin && in.Shell != protocol.ContextShellSh && in.Shell != protocol.ContextShellNone {
		return in, contextUsage("shell must be login, sh or none")
	}
	if len(in.Env) == 0 {
		in.Env = nil
	}
	return in, validateContextSyntax(execution.ContextSpec{Cwd: in.Cwd, Shell: in.Shell, Env: in.Env, Label: in.Label})
}

// resolveContextSpec binds a new context to this host: its directory must
// exist, and the login shell is resolved once, so later jobs use the shell it
// opened with.
func resolveContextSpec(in protocol.ContextSpec) (execution.ContextSpec, error) {
	spec := execution.ContextSpec{Cwd: in.Cwd, Shell: in.Shell, Env: in.Env, Label: in.Label}
	switch spec.Shell {
	case protocol.ContextShellLogin:
		spec.ShellPath = hostShell()
	case protocol.ContextShellSh:
		spec.ShellPath = posixShellPath
	}
	if len(spec.ShellPath) > maxContextPath || !validContextText(spec.ShellPath) {
		return execution.ContextSpec{}, contextUsage("the host's login shell path is not usable")
	}
	return spec, validContextDirectory(spec.Cwd)
}

const maxContextPath = 4096

func validateContextSyntax(spec execution.ContextSpec) error {
	if len(spec.Cwd) > maxContextPath || !filepath.IsAbs(spec.Cwd) || filepath.Clean(spec.Cwd) != spec.Cwd || !validContextText(spec.Cwd) {
		return contextUsage("cwd must be an absolute clean path on the host")
	}
	if len(spec.Label) > protocol.MaxContextLabelBytes || !validContextText(spec.Label) {
		return contextUsage("label must be valid text of at most 256 bytes")
	}
	return validContextEnv(spec.Env)
}

func validContextDirectory(dir string) error {
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

// observedContext reports the context with its jobs observed now. Observing
// settles finished jobs, so a job whose daemon died before launch reads as
// uncertain, not running.
func (l *lifecycle) observedContext(ctx context.Context, journal *execution.Journal, c execution.Context) protocol.ExecutionContextInfo {
	info := wireContext(c)
	for _, key := range c.Members {
		r, _ := l.lookupExecution(ctx, journal, key)
		switch r.Phase {
		case executionTerminalPhase:
		case executionLivePhase:
			info.RunningJobs++
		default:
			info.UncertainJobs++
		}
	}
	return info
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
	return protocol.ExecutionContextInfo{HostID: c.Host, ContextID: c.ID, Handle: protocol.ContextHandle(c.Host, c.ID), RequestKey: c.Key.Opaque, State: state, Revision: c.Revision, Cwd: c.Cwd, Shell: c.Shell, ShellPath: c.ShellPath, Label: c.Label, EnvNames: names, EnvDigest: execution.Digest(envOrEmpty(c.Env)), CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt, ClosedAt: c.ClosedAt, ExpiresAt: c.ExpiresAt()}
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

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

// contextJobIntent is the canonical caller-supplied request of a context
// job. Its digest decides replays; wait and page sizes are not part of it.
type contextJobIntent struct {
	Kind         string            `json:"kind"`
	Command      []string          `json:"command,omitempty"`
	ShellCommand string            `json:"shellCommand,omitempty"`
	Cwd          string            `json:"cwd,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Stdin        string            `json:"stdin"`
	// Terminal jobs carry their initial size and terminal type. A pipe job
	// leaves them empty, so its digest is unchanged by their presence.
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Term string `json:"term,omitempty"`
}

// executionKeyFor scopes a creation or lookup key. An application caller's
// key covers host, caller, context, operation and the caller key; other
// requests keep the account or device namespace.
func (l *lifecycle) executionKeyFor(ctx context.Context, request protocol.Control) (execution.Key, error) {
	caller, present, err := applicationCaller(ctx, request)
	if err != nil {
		return execution.Key{}, err
	}
	if !present && request.ExecutionContext == nil {
		return l.executionKey(ctx, request.RequestKey)
	}
	if !present {
		return execution.Key{}, &callerValidationError{err: errCallerRequired}
	}
	if request.ExecutionContext == nil || !protocol.ValidContextID(request.ExecutionContext.ContextID) {
		return execution.Key{}, contextUsage("application jobs need executionContext.contextId")
	}
	key := execution.Key{Host: string(l.host.ID), Class: execution.ApplicationClass, Principal: string(caller.ID), Opaque: request.RequestKey, Context: request.ExecutionContext.ContextID, Operation: execution.OperationExec}
	return key, executionBoundaryError("execution.go executionKeyFor", key.Validate())
}

func (l *lifecycle) contextExecutionCreate(ctx context.Context, request protocol.Control, journal *execution.Journal, key execution.Key) (protocol.Control, error) {
	if err := updategate.Check(filepath.Dir(l.sessionsDir)); err != nil {
		return protocol.Control{}, executionBoundaryError("execution.go contextExecutionCreate", err)
	}
	job, err := contextJobRequest(request)
	if err != nil {
		return protocol.Control{}, err
	}
	ctx = admittedContext(ctx)
	resolve := func(c execution.Context) (execution.Intent, map[string]string, error) {
		return resolveContextJob(c, job)
	}
	reserve := func() (execution.Receipt, bool, []string, execution.Intent, error) {
		var allocated string
		r, owner, err := journal.ReserveInContextAndDispatch(ctx, key, execution.Digest(job), resolve, l.allocateContextJob(key, &allocated))
		if err != nil || !owner {
			// Allocated but not accepted: nothing will launch it.
			l.standbys.drop(allocated)
			return r, owner, nil, execution.Intent{}, executionBoundaryError("reserve context job", err)
		}
		r, owner, env, intent, err := l.persistedContextLaunch(ctx, journal, r)
		if err != nil || !owner {
			l.standbys.drop(allocated)
		}
		return r, owner, env, intent, err
	}
	return l.createExecution(ctx, request, journal, key, reserve)
}

// allocateContextJob reserves the worker slot for a new context job and
// records its session ID in allocated.
func (l *lifecycle) allocateContextJob(key execution.Key, allocated *string) func(execution.LaunchSnapshot) (execution.Binding, error) {
	return func(s execution.LaunchSnapshot) (execution.Binding, error) {
		preferred := l.standbys.preferredID()
		ref, err := worker.ReservePreferring(l.sessionsDir, execution.PurposeForKind(s.Intent.Kind), preferred)
		l.standbys.reserved(preferred, ref.ID)
		*allocated = ref.ID
		if err != nil {
			return execution.Binding{}, executionBoundaryError("reserve context job", err)
		}
		return execution.NewBinding(key, s.Intent, ref, l.contextJobEnvironment(s, ref)), nil
	}
}

func (l *lifecycle) contextJobEnvironment(s execution.LaunchSnapshot, ref session.Reservation) []string {
	return worker.ExecutionEnvironment(withEnvOverlay(l.environment(), s.Env), string(l.host.ID), ref, s.Intent.Term)
}

// persistedContextLaunch launches from the snapshot stored with the
// reservation, so what runs is exactly what the receipt accepted. A snapshot
// that cannot be read means the job is never started.
func (l *lifecycle) persistedContextLaunch(ctx context.Context, journal *execution.Journal, r execution.Receipt) (execution.Receipt, bool, []string, execution.Intent, error) {
	snapshot, err := journal.ReadLaunchSnapshot(r)
	if err != nil {
		next, saveErr := journal.NotStarted(ctx, r.Binding, "launch_snapshot_unreadable")
		return next, false, nil, execution.Intent{}, errors.Join(executionBoundaryError("read context launch", err), saveErr)
	}
	return r, true, l.contextJobEnvironment(snapshot, r.Binding.Reservation), snapshot.Intent, nil
}

func (l *lifecycle) environment() []string {
	if l.env != nil {
		return l.env
	}
	return os.Environ()
}

// contextJobRequest validates the caller-supplied part of a context job.
// Terminal jobs are allowed because the scoped session-output route serves
// only account-owned sessions (privateSession), so application-owned
// terminal output stays private to its caller.
func contextJobRequest(request protocol.Control) (contextJobIntent, error) {
	params := request.ExecutionContext
	job := contextJobIntent{Kind: request.ExecutionKind, Command: request.Command, ShellCommand: params.ShellCommand, Cwd: request.Cwd, Env: params.Env, Stdin: request.Stdin, Cols: request.Cols, Rows: request.Rows, Term: request.Term}
	switch job.Kind {
	case execution.KindPipe:
		if job.Cols != 0 || job.Rows != 0 || job.Term != "" {
			return job, contextUsage("terminal size and type need kind pty")
		}
		if job.Stdin == "" {
			job.Stdin = execution.StdinClosed
		}
	case execution.KindPTY:
		if job.Stdin != "" {
			return job, contextUsage("terminal jobs take input through their terminal; omit stdin")
		}
	default:
		return job, contextUsage("context jobs use kind pipe or pty")
	}
	if (len(job.Command) == 0) == (job.ShellCommand == "") {
		return job, contextUsage("pass exactly one of command (argv) or executionContext.shellCommand")
	}
	if len(job.Env) == 0 {
		job.Env = nil
	}
	if !validContextText(job.ShellCommand) || !validContextText(job.Cwd) || len(job.Cwd) > maxContextPath {
		return job, contextUsage("command and cwd must be valid text")
	}
	// Equivalent paths resolve to the same directory, so they are one intent.
	if job.Cwd != "" {
		job.Cwd = filepath.Clean(job.Cwd)
	}
	return job, validContextEnv(job.Env)
}

// resolveContextJob turns a job request into its launch intent against one
// context revision. The child's own cd or export never reaches the context.
func resolveContextJob(c execution.Context, job contextJobIntent) (execution.Intent, map[string]string, error) {
	cwd := c.Cwd
	if job.Cwd != "" && filepath.IsAbs(job.Cwd) {
		cwd = filepath.Clean(job.Cwd)
	} else if job.Cwd != "" {
		cwd = filepath.Join(c.Cwd, job.Cwd)
	}
	command := job.Command
	if job.ShellCommand != "" {
		switch c.Shell {
		case protocol.ContextShellLogin:
			command = []string{c.ShellPath, "-l", "-c", job.ShellCommand}
		case protocol.ContextShellSh:
			command = []string{posixShellPath, "-c", job.ShellCommand}
		default:
			return execution.Intent{}, nil, contextUsage("this context has shell none; pass command as argv")
		}
	}
	env := make(map[string]string, len(c.Env)+len(job.Env))
	for name, value := range c.Env {
		env[name] = value
	}
	for name, value := range job.Env {
		env[name] = value
	}
	intent := execution.Intent{Kind: job.Kind, Command: command, Cwd: cwd, Stdin: job.Stdin, Cols: job.Cols, Rows: job.Rows, Term: job.Term}
	return intent, env, nil
}

// withEnvOverlay replaces or adds overlay variables in a copy of base.
func withEnvOverlay(base []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overlay))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overlay[name]; !replaced {
			out = append(out, entry)
		}
	}
	names := make([]string, 0, len(overlay))
	for name := range overlay {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+overlay[name])
	}
	return out
}

// decorateApplicationError gives context replies their record and specific
// codes. Raw execution replies already carry their partial receipt.
func decorateApplicationError(reply *protocol.Control, request protocol.Control, err error) {
	if code := applicationErrorCode(err); code != "" {
		reply.ErrorCode = code
	}
	if !protocol.IsContextControl(request.Type) {
		return
	}
	if applicationErrorCode(err) == "" {
		reply.ErrorCode = executionCode(err)
	}
	if errors.Is(err, access.ErrDenied) {
		reply.ErrorCode = accessDeniedCode
	}
	var failure *contextFailure
	if errors.As(err, &failure) {
		reply.ContextInfo = failure.response.ContextInfo
	}
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

// mcpLauncher runs hosted MCP servers as line workers on the execution
// journal's reserved ownership path, like keyed pipe jobs. Each generation
// has its own journal key, so a launch is reserved once and recovery finds
// the exact reservation again.
type mcpLauncher struct {
	l *lifecycle
}

var _ mcphost.Launcher = mcpLauncher{}

func (m mcpLauncher) key(k mcphost.LaunchKey) execution.Key {
	return execution.Key{Host: string(m.l.host.ID), Class: execution.InternalServiceClass, Principal: k.Principal(), Opaque: k.Opaque()}
}

func (m mcpLauncher) ref(b execution.Binding) mcphost.WorkerRef {
	return mcphost.WorkerRef{SessionsDir: m.l.sessionsDir, SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, LaunchDigest: b.LaunchDigest}
}

func (m mcpLauncher) Launch(ctx context.Context, spec mcphost.LaunchSpec) (mcphost.WorkerRef, error) {
	journal, err := m.l.executionJournal()
	if err != nil {
		return mcphost.WorkerRef{}, err
	}
	intent, err := execution.Normalize(execution.Intent{Kind: execution.KindLine, Stdin: execution.StdinOpen, Command: spec.Argv, Cwd: spec.Cwd})
	if err != nil {
		return mcphost.WorkerRef{}, fmt.Errorf("launch MCP worker: %w", err)
	}
	key := m.key(spec.Key)
	var env []string
	r, created, err := journal.Reserve(ctx, key, intent, func() (execution.Binding, error) {
		ref, err := worker.Reserve(m.l.sessionsDir, execution.PurposeForKind(intent.Kind))
		if err != nil {
			return execution.Binding{}, fmt.Errorf("reserve MCP worker: %w", err)
		}
		base := m.l.env
		if base == nil {
			base = os.Environ()
		}
		// The definition's variables go before the session identity, which
		// the launcher re-applies last; the worker checks the exact order.
		env = worker.ExecutionEnvironment(slices.Concat(base, spec.Env), string(m.l.host.ID), ref, "")
		return execution.NewBinding(key, intent, ref, env), nil
	})
	if err != nil {
		return mcphost.WorkerRef{}, fmt.Errorf("reserve MCP worker: %w", err)
	}
	if !created {
		// A reservation from an earlier attempt of this generation exists.
		// Never launch a second worker for it.
		return m.ref(r.Binding), mcphost.ErrLaunchUncertain
	}
	return m.dispatch(ctx, journal, r.Binding, intent, env)
}

func (m mcpLauncher) dispatch(ctx context.Context, journal *execution.Journal, b execution.Binding, intent execution.Intent, env []string) (mcphost.WorkerRef, error) {
	if err := journal.Dispatch(ctx, b, intent); err != nil {
		return mcphost.WorkerRef{}, errors.Join(mcphost.ErrLaunchUncertain, err)
	}
	executionDaemonBoundary(m.l.sessionsDir, "before_mcp_worker_launch", execution.Receipt{Binding: b})
	ref := b.Reservation
	launched, err := m.l.launch(worker.LaunchConfig{SessionsDir: m.l.sessionsDir, HostID: string(m.l.host.ID), Executable: m.l.executable, Reservation: &ref, Execution: &b, Command: intent.Command, Cwd: intent.Cwd, Env: env, Label: "agent request " + ref.Instance})
	if err != nil {
		var started *worker.StartedError
		if errors.As(err, &started) {
			return m.ref(b), errors.Join(mcphost.ErrLaunchUncertain, err)
		}
		_, saveErr := journal.NotStarted(m.l.context, b, "worker_start_failed")
		return mcphost.WorkerRef{}, errors.Join(err, saveErr)
	}
	if launched.Meta.Reservation == nil || *launched.Meta.Reservation != ref {
		return m.ref(b), mcphost.ErrLaunchUncertain
	}
	executionDaemonBoundary(m.l.sessionsDir, "before_mcp_publication", execution.Receipt{Binding: b})
	if _, err := journal.Published(ctx, b); err != nil {
		return m.ref(b), errors.Join(mcphost.ErrLaunchUncertain, err)
	}
	return m.ref(b), nil
}

func (m mcpLauncher) Locate(ctx context.Context, k mcphost.LaunchKey) (mcphost.WorkerRef, mcphost.WorkerStatus, *int, error) {
	journal, err := m.l.executionJournal()
	if err != nil {
		return mcphost.WorkerRef{}, mcphost.WorkerUncertain, nil, err
	}
	r, err := journal.Lookup(ctx, m.key(k))
	if errors.Is(err, execution.ErrNotFound) {
		return mcphost.WorkerRef{}, mcphost.WorkerAbsent, nil, nil
	}
	if err != nil && !errors.Is(err, execution.ErrUnknown) {
		return mcphost.WorkerRef{}, mcphost.WorkerUncertain, nil, fmt.Errorf("look up MCP worker: %w", err)
	}
	ref := m.ref(r.Binding)
	status, code := m.observe(ctx, journal, r)
	return ref, status, code, nil
}

func (m mcpLauncher) observe(ctx context.Context, journal *execution.Journal, r execution.Receipt) (mcphost.WorkerStatus, *int) {
	if r.Phase == executionTerminalPhase {
		return mcphost.WorkerEnded, exitCode(r)
	}
	if r.Phase == "reserved" {
		return mcphost.WorkerAbsent, nil
	}
	if r.Binding.Reservation.BootID != worker.BootID() {
		return mcphost.WorkerInterrupted, nil
	}
	if worker.ProbePipe(ctx, m.l.sessionsDir, r.Binding) == nil {
		return mcphost.WorkerLive, nil
	}
	// The worker may have finished between the journal read and the probe.
	if settled, err := journal.Exact(ctx, r.Binding); err == nil && settled.Phase == executionTerminalPhase {
		return mcphost.WorkerEnded, exitCode(settled)
	}
	return mcphost.WorkerUncertain, nil
}

func exitCode(r execution.Receipt) *int {
	if r.Result == nil || r.Result.Termination.Kind != "exit" {
		return nil
	}
	return r.Result.Termination.Code
}

func (m mcpLauncher) binding(ref mcphost.WorkerRef) (*execution.Journal, execution.Binding, error) {
	journal, err := m.l.executionJournal()
	if err != nil {
		return nil, execution.Binding{}, err
	}
	b, err := m.l.executionOutput.Binding(ref.SessionID, ref.Instance, string(m.l.host.ID))
	if err != nil {
		return nil, execution.Binding{}, fmt.Errorf("MCP worker %s: %w", ref.SessionID, err)
	}
	if b.Mode() != execution.KindLine || b.LaunchDigest != ref.LaunchDigest {
		return nil, execution.Binding{}, fmt.Errorf("MCP worker %s: %w", ref.SessionID, execution.ErrInstance)
	}
	return journal, b, nil
}

// Stop records kill intent and stops the exact worker instance.
func (m mcpLauncher) Stop(ctx context.Context, ref mcphost.WorkerRef) error {
	journal, b, err := m.binding(ref)
	if err != nil {
		return err
	}
	r, err := journal.Exact(ctx, b)
	if err != nil {
		return fmt.Errorf("stop MCP worker %s: %w", ref.SessionID, err)
	}
	if r.Phase == executionTerminalPhase {
		return nil
	}
	if err := journal.MarkKill(ctx, b); err != nil {
		return fmt.Errorf("stop MCP worker %s: %w", ref.SessionID, err)
	}
	req := protocol.Control{Type: protocol.TypeExecutionKill, RequestID: "mcp-stop", SessionID: b.Reservation.ID, Instance: b.Reservation.Instance}
	if _, err := m.l.executionDeliver(ctx, req, b); err != nil {
		return fmt.Errorf("stop MCP worker %s: %w", ref.SessionID, err)
	}
	return nil
}

// Stderr reads the private 1 MiB diagnostics ring the worker retains.
func (m mcpLauncher) Stderr(ctx context.Context, ref mcphost.WorkerRef, limit int) ([]byte, error) {
	_, b, err := m.binding(ref)
	if err != nil {
		return nil, err
	}
	page, err := m.l.executionOutput.Page(ctx, b, execution.StreamStderr, "", limit)
	if err != nil {
		return nil, fmt.Errorf("read MCP worker %s stderr: %w", ref.SessionID, err)
	}
	return page.Bytes, nil
}

func (m mcpLauncher) Dial(ctx context.Context, ref mcphost.WorkerRef) (mcphost.LineConn, error) {
	ref.SessionsDir = m.l.sessionsDir
	conn, err := mcphost.DialWorkerLines(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("attach MCP worker: %w", err)
	}
	return conn, nil
}

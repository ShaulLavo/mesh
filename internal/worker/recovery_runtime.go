package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
)

type RecoveryRuntime struct {
	SessionsDir, HostID, Executable string
	Env                             []string
}

func (r RecoveryRuntime) Inspect(ctx context.Context, id string) (recovery.Source, error) {
	canonical, err := session.ParseID(id)
	if err != nil || canonical != id {
		return recovery.Source{}, fmt.Errorf("worker: invalid recovery session ID %q", id)
	}
	dir := filepath.Join(r.SessionsDir, id)
	if _, err := os.Lstat(paths.Forgotten(dir)); err == nil {
		return recovery.Source{}, fmt.Errorf("session %s was forgotten: %w", id, os.ErrNotExist)
	}
	if _, err := os.Lstat(paths.Launching(dir)); err == nil {
		return recovery.Source{ID: id}, nil
	}
	meta, err := ReadMeta(dir)
	if err != nil {
		return recovery.Source{}, fmt.Errorf("session %s: read recovery metadata: %w", id, err)
	}
	if meta.ID != id {
		return recovery.Source{}, fmt.Errorf("session %s: metadata belongs to %s", id, meta.ID)
	}
	source := recovery.Source{ID: id, State: meta.State, Cwd: meta.Cwd, BootID: meta.BootID, RecoveredFrom: meta.RecoveredFrom, Command: meta.Command, Shell: r.shell(), Published: true}
	if source.State != StateRunning && source.State != StateDetached {
		return source, nil
	}
	if current := BootID(); current != "" && meta.BootID != "" && current != meta.BootID {
		source.State = StateInterrupted
		return source, nil
	}
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", paths.Socket(dir))
	if err == nil {
		_ = conn.Close()
		return source, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return recovery.Source{}, fmt.Errorf("session %s: worker liveness is inconclusive: %w", id, err)
	}
	refreshed, err := ReadMeta(dir)
	if err != nil {
		return recovery.Source{}, fmt.Errorf("session %s: reread unavailable worker: %w", id, err)
	}
	source.State = StateInterrupted
	if refreshed.State == StateExited {
		source.State = StateExited
	}
	return source, nil
}

func (r RecoveryRuntime) Launch(_ context.Context, launch recovery.Launch) error {
	_, err := LaunchDetached(LaunchConfig{SessionsDir: r.SessionsDir, HostID: r.HostID, Executable: r.Executable, Env: r.Env, ReservedID: launch.ID, RecoveredFrom: launch.SourceID, Command: launch.Command, Cwd: launch.Cwd, Cols: launch.Cols, Rows: launch.Rows, Term: launch.Term, Depth: launch.Depth})
	return err
}

func (r RecoveryRuntime) ConfigureCommand(ctx context.Context, id string, command *recovery.Command) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", paths.Socket(filepath.Join(r.SessionsDir, id)))
	if err != nil {
		return fmt.Errorf("session %s: connect recovery writer: %w", id, err)
	}
	defer conn.Close() //nolint:errcheck // one bounded request; the response is authoritative
	deadline := time.Now().Add(5 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("session %s: set recovery deadline: %w", id, err)
	}
	request := protocol.Control{Type: protocol.TypeRecoveryCommand, RequestID: "recovery-command", SessionID: id, RecoveryCommand: command, ClearRecoveryCommand: command == nil}
	if err := protocol.NewWriter(conn).WriteControlMsg(request); err != nil {
		return fmt.Errorf("session %s: configure recovery command: %w", id, err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return fmt.Errorf("session %s: read recovery acknowledgement: %w", id, err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	if response.Type != protocol.TypeOK || response.SessionID != id {
		return fmt.Errorf("session %s: configure recovery command: %s", id, response.Message)
	}
	return nil
}

func (r RecoveryRuntime) shell() string {
	for _, entry := range r.Env {
		if strings.HasPrefix(entry, "SHELL=") && len(entry) > len("SHELL=") {
			return strings.TrimPrefix(entry, "SHELL=")
		}
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

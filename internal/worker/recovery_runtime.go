package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if saved, err := recovery.Read(dir); err == nil && saved.HostID == r.HostID && saved.SessionID == id {
		source.AgentResume = saved.AgentResume
	}
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
		err = r.probeRecoveryWorker(ctx, conn, id)
		_ = conn.Close()
	}
	if err == nil {
		return source, nil
	}
	if !unavailableRecoveryWorker(err) {
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

func (r RecoveryRuntime) probeRecoveryWorker(ctx context.Context, conn net.Conn, id string) error {
	deadline := time.Now().Add(500 * time.Millisecond)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	request := protocol.Control{Type: protocol.TypeContainment, RequestID: "recovery-liveness", SessionID: id}
	if err := protocol.NewWriter(conn).WriteControlMsg(request); err != nil {
		return err
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return err
	}
	if frame.Kind != protocol.KindControl {
		return fmt.Errorf("worker liveness reply is not a control frame")
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	return r.validateRecoveryReply(response, id)
}

func (r RecoveryRuntime) validateRecoveryReply(response protocol.Control, id string) error {
	if response.Type == protocol.TypeError && (response.SessionID == "" || response.SessionID == id) {
		return nil
	}
	if response.Type != protocol.TypeContained || response.SessionID != id || len(response.ContainingSessions) == 0 {
		return fmt.Errorf("worker liveness reply does not identify the expected session")
	}
	if err := protocol.ValidateContainingSessions(response.ContainingSessions); err != nil {
		return err
	}
	owner := response.ContainingSessions[0]
	if owner.HostID != r.HostID || owner.SessionID != id {
		return fmt.Errorf("worker liveness reply does not identify the expected host and session")
	}
	return nil
}

func unavailableRecoveryWorker(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func (r RecoveryRuntime) Launch(_ context.Context, launch recovery.Launch) error {
	if launch.Agent != nil {
		executable, err := r.agentResumeExecutable()
		if err != nil {
			return &recovery.LaunchFailure{Err: err}
		}
		launch.Command = []string{executable, "agent-resume"}
	}
	_, err := LaunchDetached(LaunchConfig{SessionsDir: r.SessionsDir, HostID: r.HostID, Executable: r.Executable, Env: r.Env, ReservedID: launch.ID, RecoveredFrom: launch.SourceID, Command: launch.Command, Cwd: launch.Cwd, Cols: launch.Cols, Rows: launch.Rows, Term: launch.Term, Depth: launch.Depth})
	return err
}

func (r RecoveryRuntime) agentResumeExecutable() (string, error) {
	if r.Executable != "" {
		return r.Executable, nil
	}
	return os.Executable()
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

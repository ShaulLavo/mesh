package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/worker"
)

func (l *lifecycle) recoveryConfig() recovery.Config {
	return recovery.Config{SessionsDir: l.sessionsDir, HostID: string(l.host.ID), BootID: worker.BootID(), Runtime: worker.RecoveryRuntime{SessionsDir: l.sessionsDir, HostID: string(l.host.ID), Executable: l.executable, Env: l.env}}
}

func (l *lifecycle) recoveryControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if ctx == nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s has nil context", request.Type)
	}
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	id, err := session.ParseID(request.SessionID)
	if err != nil {
		return protocol.Control{}, err
	}
	request.SessionID = id
	switch request.Type {
	case protocol.TypeRecover:
		return l.recover(ctx, request)
	case protocol.TypeRecoveryCommand:
		return l.configureRecoveryCommand(ctx, request)
	default:
		return l.readRecovery(ctx, request)
	}
}

func (l *lifecycle) recover(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := protocol.ValidateContainingSessions(request.ContainingSessions); err != nil {
		return protocol.Control{}, err
	}
	result, err := recovery.Recover(ctx, l.recoveryConfig(), recovery.Request{SessionID: request.SessionID, Action: recovery.Action(request.RecoveryAction), Cols: request.Cols, Rows: request.Rows, Term: request.Term, Depth: request.Depth})
	if err != nil {
		return protocol.Control{}, err
	}
	if result.Remote == nil {
		if err := l.catalog.Reconcile(ctx); err != nil {
			return protocol.Control{}, fmt.Errorf("daemon: publish recovered session %s: %w", result.SessionID, err)
		}
	}
	return protocol.Control{Type: protocol.TypeRecovered, RequestID: request.RequestID, SessionID: result.SessionID, RecoveryResult: &result, RecoverySupported: true}, nil
}

func (l *lifecycle) configureRecoveryCommand(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if request.ClearRecoveryCommand == (request.RecoveryCommand != nil) {
		return protocol.Control{}, fmt.Errorf("daemon: supply a recovery command or clear it")
	}
	if err := recovery.ConfigureCommand(ctx, l.recoveryConfig(), request.SessionID, request.RecoveryCommand); err != nil {
		return protocol.Control{}, err
	}
	return protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: request.SessionID, RecoverySupported: true}, nil
}

func (l *lifecycle) readRecovery(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	source, err := l.recoveryConfig().Runtime.Inspect(ctx, request.SessionID)
	if err != nil {
		return protocol.Control{}, err
	}
	fallback := recovery.Record{Version: recovery.Version, HostID: string(l.host.ID), SessionID: source.ID, Shell: source.Shell, ShellDirectory: source.Cwd, DirectorySource: recovery.DirectoryLaunch, Command: source.Command}
	record, err := recovery.ReadSaved(filepath.Join(l.sessionsDir, source.ID), string(l.host.ID), source.ID, fallback)
	if err != nil {
		return protocol.Control{}, err
	}
	return protocol.Control{Type: protocol.TypeRecoveryRecord, RequestID: request.RequestID, SessionID: source.ID, Recovery: &record, RecoverySupported: true}, nil
}

func (l *lifecycle) addRecoveryInfo(info *protocol.SessionInfo) {
	dir := filepath.Join(l.sessionsDir, info.ID)
	meta, err := worker.ReadMeta(dir)
	if err == nil {
		info.RecoveredFrom = meta.RecoveredFrom
	}
	info.ReplacementID, err = recovery.ReplacementID(dir, string(l.host.ID), info.ID)
	if err != nil {
		info.RecoveryError = err.Error()
	}
	fallback := recovery.Record{Version: recovery.Version, HostID: string(l.host.ID), SessionID: info.ID, Shell: hostShell(), ShellDirectory: info.Cwd, DirectorySource: recovery.DirectoryLaunch, Command: info.Command}
	record, err := recovery.ReadSaved(dir, string(l.host.ID), info.ID, fallback)
	if err != nil {
		info.RecoveryError = err.Error()
		return
	}
	// Lists carry recognition data. Full previous output has its own bounded,
	// authenticated request so a host with many sessions stays listable.
	record = protocol.RecoveryPreview(record)
	info.Recovery = &record
	if info.RecoveredFrom != "" {
		info.AgentStatus = recovery.AgentStatus(l.sessionsDir, string(l.host.ID), info.RecoveredFrom, info.ID, record.AgentResume)
	}
}

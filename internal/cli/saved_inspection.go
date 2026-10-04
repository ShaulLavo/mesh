package cli

import (
	"fmt"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func savedInspection(host HostRecord, sessionID string, record recovery.Record) (SessionInspection, error) {
	if record.SessionID != sessionID || host.ID != "" && record.HostID != host.ID {
		return SessionInspection{}, fmt.Errorf("host %s returned a checkpoint for a different session", HostLabel(host))
	}
	// Launch-only recovery has no checkpoint timestamp; validate its remaining
	// fields with a temporary timestamp without changing the displayed record.
	checked := record
	if checked.CheckpointAt.IsZero() {
		checked.CheckpointAt = time.Now()
	}
	if err := recovery.Validate(checked); err != nil {
		return SessionInspection{}, fmt.Errorf("host %s returned an invalid saved inspection: %w", HostLabel(host), err)
	}
	if !slices.Equal(protocol.RecoveryPreview(record).Lines, record.Lines) {
		return SessionInspection{}, fmt.Errorf("host %s returned an oversized saved preview", HostLabel(host))
	}
	return SessionInspection{Recovery: &record}, nil
}

func inspectSavedLocalSession(current Session) (SessionInspection, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return SessionInspection{}, fmt.Errorf("inspect saved session %s: state directory: %w", current.ID, err)
	}
	host, err := existingLocalIdentity(stateDir)
	if err != nil {
		return SessionInspection{}, err
	}
	fallback := recovery.Record{Version: recovery.Version, HostID: host.ID, SessionID: current.ID, Shell: defaultShell(), ShellDirectory: current.Cwd, DirectorySource: recovery.DirectoryLaunch, Command: current.Command}
	saved, err := recovery.ReadSaved(current.Dir, host.ID, current.ID, fallback)
	if err != nil {
		return SessionInspection{}, fmt.Errorf("inspect saved session %s: read recovery: %w", current.ID, err)
	}
	// ReadSaved validates disk ownership; legacy launch-only rows may predate
	// the host identity and remain displayable without wire validation.
	saved = protocol.RecoveryPreview(saved)
	return SessionInspection{Recovery: &saved}, nil
}

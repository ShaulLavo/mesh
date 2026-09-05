package cli

import (
	"errors"
	"os"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func addLocalRecoveryInfo(row *protocol.SessionInfo, current Session, hostID string) {
	row.RecoveredFrom = current.RecoveredFrom
	row.HostID = hostID
	replacement, err := recovery.ReplacementID(current.Dir, hostID, current.ID)
	row.ReplacementID = replacement
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		row.RecoveryError = err.Error()
	}
	fallback := recovery.Record{Version: recovery.Version, HostID: hostID, SessionID: current.ID,
		Shell: defaultShell(), ShellDirectory: current.Cwd, DirectorySource: recovery.DirectoryLaunch, Command: current.Command}
	record, err := recovery.ReadSaved(current.Dir, hostID, current.ID, fallback)
	if err != nil {
		row.RecoveryError = err.Error()
		return
	}
	preview := protocol.RecoveryPreview(record)
	row.Recovery = &preview
}

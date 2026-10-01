package cli

import (
	"context"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
)

func localSessionRowsWithDaemonMemory(parent context.Context, stateDir string) ([]protocol.SessionInfo, error) {
	rows, err := localSessionRows()
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	ctx, cancel := context.WithTimeout(parent, localQueryTimeout)
	defer cancel()
	sampled, err := ListViaDaemon(ctx, meshdaemon.SocketPath(stateDir))
	if err != nil {
		// Disk-backed sessions remain usable without a daemon. Unknown memory is
		// zero, as on hosts where process memory cannot be measured.
		return rows, nil
	}
	sizes := make(map[protocol.SessionIdentity]uint64, len(sampled))
	for _, row := range sampled {
		sizes[protocol.SessionIdentity{HostID: row.HostID, SessionID: row.ID}] = row.MemoryBytes
	}
	for i := range rows {
		if liveState(rows[i].State) {
			rows[i].MemoryBytes = sizes[protocol.SessionIdentity{HostID: rows[i].HostID, SessionID: rows[i].ID}]
		}
	}
	return rows, nil
}

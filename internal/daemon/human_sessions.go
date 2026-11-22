package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

// humanSessionControl serves the session list, inspect and hibernate
// controls limited to sessions a person started. Keyed jobs keep their
// owner-bound execution controls; service and app workers belong to Mesh.
func (l *lifecycle) humanSessionControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if ctx == nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request has nil context", request.Type)
	}
	switch request.Type {
	case protocol.TypeList:
		response, err := l.list(ctx, request)
		if err != nil {
			return response, err
		}
		response.Sessions = l.personSessions(response.Sessions)
		return response, nil
	case protocol.TypeInspect, protocol.TypeHibernate:
		if !isCanonicalSessionID(request.SessionID) || !worker.PersonSession(filepath.Join(l.sessionsDir, request.SessionID)) {
			return protocol.Control{}, fmt.Errorf("daemon: session %s: %w", request.SessionID, errAccessNotFound)
		}
		request.HumanSessionOnly = false
		response, _, err := l.HandleControl(ctx, request)
		return response, err
	default:
		return protocol.Control{}, fmt.Errorf("daemon: %s does not take humanSessionOnly", request.Type)
	}
}

func (l *lifecycle) personSessions(rows []protocol.SessionInfo) []protocol.SessionInfo {
	kept := rows[:0]
	for _, row := range rows {
		if isCanonicalSessionID(row.ID) && worker.PersonSession(filepath.Join(l.sessionsDir, row.ID)) {
			kept = append(kept, row)
		}
	}
	return kept
}

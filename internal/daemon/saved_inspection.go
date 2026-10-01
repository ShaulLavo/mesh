package daemon

import (
	"context"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
)

func (l *lifecycle) inspect(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if err := protocol.ValidateInspectDimensions(request.PreviewCols, request.PreviewRows); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: inspect session %s: dimensions: %w", request.SessionID, err)
	}
	id, err := session.ParseID(request.SessionID)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: inspect session %s: session ID: %w", request.SessionID, err)
	}
	request.SessionID = id
	response, err := l.forwardOneShot(ctx, request, l.connector.ConnectWorker)
	if err == nil || ctx.Err() != nil {
		return response, err
	}
	stored, catalogErr := l.catalog.Get(ctx, storage.SessionID(request.SessionID))
	if catalogErr != nil || stored.State == storage.StateRunning || stored.State == storage.StateDetached {
		return response, err
	}
	info := sessionInfo(stored)
	l.addRecoveryInfo(&info, false)
	if info.RecoveryError != "" {
		return protocol.Control{}, fmt.Errorf("daemon: inspect saved session %s: %s", request.SessionID, info.RecoveryError)
	}
	return protocol.Control{Type: protocol.TypeInspected, RequestID: request.RequestID, SessionID: request.SessionID, Recovery: info.Recovery}, nil
}

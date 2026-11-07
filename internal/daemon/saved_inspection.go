package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/recovery"

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

func (l *lifecycle) outputPreview(ctx context.Context, request protocol.Control, admit readAdmission) (protocol.AccessOutputResult, error) {
	if err := admit(ctx); err != nil {
		return protocol.AccessOutputResult{}, err
	}
	stored, err := l.catalog.Get(ctx, storage.SessionID(request.SessionID))
	if err != nil {
		return protocol.AccessOutputResult{}, fmt.Errorf("daemon: preview session lookup: %w", err)
	}
	if stored.State != storage.StateRunning && stored.State != storage.StateDetached {
		return l.savedOutputPreview(ctx, request, admit)
	}
	response, err := l.forwardRead(ctx, request, admit)
	if err == nil {
		return liveOutputPreview(response)
	}
	if ctx.Err() != nil || errors.Is(err, access.ErrDenied) {
		return protocol.AccessOutputResult{}, err
	}
	if err := admit(ctx); err != nil {
		return protocol.AccessOutputResult{}, err
	}
	stored, lookupErr := l.catalog.Get(ctx, storage.SessionID(request.SessionID))
	if lookupErr != nil || stored.State == storage.StateRunning || stored.State == storage.StateDetached {
		return protocol.AccessOutputResult{}, err
	}
	return l.savedOutputPreview(ctx, request, admit)
}

func liveOutputPreview(response protocol.Control) (protocol.AccessOutputResult, error) {
	inspection := response.Inspection
	if len(inspection.Preview) == 0 {
		return protocol.AccessOutputResult{}, errAccessUnavailable
	}
	styled := inspection.StyledPreview
	if styled == nil {
		styled = []protocol.PreviewLine{}
	}
	plain := inspection.Preview
	if plain == nil {
		plain = []string{}
	}
	return protocol.AccessOutputResult{Source: "live-preview", ObservedAt: &inspection.ObservedAt, Preview: plain, StyledPreview: styled}, nil
}
func (l *lifecycle) savedOutputPreview(ctx context.Context, request protocol.Control, admit readAdmission) (protocol.AccessOutputResult, error) {
	if err := admit(ctx); err != nil {
		return protocol.AccessOutputResult{}, err
	}
	// Read the checkpoint only. Recipe overlays and fallback launch records are not preview sources.
	record, err := recovery.Read(filepath.Join(l.sessionsDir, request.SessionID))
	if err != nil {
		return protocol.AccessOutputResult{}, fmt.Errorf("daemon: read saved preview: %w", err)
	}
	if record.HostID != string(l.host.ID) || record.SessionID != request.SessionID || len(record.Lines) == 0 {
		return protocol.AccessOutputResult{}, errAccessUnavailable
	}
	record = protocol.RecoveryPreview(record)
	if len(record.Lines) == 0 {
		return protocol.AccessOutputResult{}, errAccessUnavailable
	}
	lines := record.Lines
	if len(lines) > request.PreviewRows {
		lines = lines[len(lines)-request.PreviewRows:]
	}
	cropped := make([]string, 0, len(lines))
	for _, line := range lines {
		cropped = append(cropped, ansi.Truncate(line, request.PreviewCols, ""))
	}
	return protocol.AccessOutputResult{Source: "saved-preview", CheckpointAt: &record.CheckpointAt, Preview: cropped, StyledPreview: []protocol.PreviewLine{}}, nil
}

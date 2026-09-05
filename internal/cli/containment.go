package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

const containmentQueryTimeout = 500 * time.Millisecond

// discoverContainingSessions asks the worker above this process for the exact
// terminal path installed by nested attachments. A worker from an older Mesh
// version cannot answer, so the read-only local host identity supplies the
// immediate session without guessing from host-local session IDs elsewhere.
func discoverContainingSessions(parent context.Context) []protocol.SessionIdentity {
	location, ok := worker.ContainingSessionWorker()
	if !ok {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, containmentQueryTimeout)
	contained, err := queryContainingSessionWorker(ctx, location)
	cancel()
	if err == nil {
		return contained
	}
	return legacyContainingSession(location, os.Getenv(worker.MeshSessionIDVariable), os.Getenv(worker.MeshHostIDVariable), identity.Load)
}

func queryContainingSessionWorker(ctx context.Context, location worker.SessionWorkerLocation) ([]protocol.SessionIdentity, error) {
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(location.Dir))
	if err != nil {
		return nil, err
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // one read-only request

	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type:      protocol.TypeContainment,
		RequestID: requestID,
		SessionID: location.SessionID,
	})
	if err != nil {
		return nil, err
	}
	if response.Type != protocol.TypeContained || response.SessionID != location.SessionID {
		return nil, errUnexpectedContainmentResponse
	}
	if err := protocol.ValidateContainingSessions(response.ContainingSessions); err != nil {
		return nil, err
	}
	if len(response.ContainingSessions) == 0 || response.ContainingSessions[0].SessionID != location.SessionID {
		return nil, errUnexpectedContainmentResponse
	}
	return protocol.CloneSessionIdentities(response.ContainingSessions), nil
}

var errUnexpectedContainmentResponse = errors.New("worker returned an unexpected containment response")

type existingIdentityLoader func(string) (identity.Host, error)

func rejectContainingTarget(target protocol.SessionIdentity, containing []protocol.SessionIdentity) error {
	if slices.Contains(containing, target) {
		return fmt.Errorf("cannot attach to session %s: that session already contains this terminal", target.SessionID)
	}
	return nil
}

func legacyContainingSession(
	location worker.SessionWorkerLocation,
	environmentSessionID string,
	environmentHostID string,
	loadIdentity existingIdentityLoader,
) []protocol.SessionIdentity {
	hostID := ""
	if parsed, err := session.ParseID(environmentSessionID); err == nil && parsed == location.SessionID {
		hostID = environmentHostID
	}
	if hostID == "" && loadIdentity != nil {
		// Production workers live at <state>/s/<session>. Deriving the state
		// directory from the proven ancestor avoids matching this host-local ID
		// against an unrelated remote catalog row.
		stateDir := filepath.Dir(filepath.Dir(location.Dir))
		if local, err := loadIdentity(stateDir); err == nil {
			hostID = local.ID
		}
	}
	candidate := protocol.SessionIdentity{HostID: hostID, SessionID: location.SessionID}
	if protocol.ValidateSessionIdentity(candidate) != nil {
		return nil
	}
	return []protocol.SessionIdentity{candidate}
}

func resolvedTargetIdentity(resolved resolvedSession) (protocol.SessionIdentity, error) {
	if resolved.local != nil && resolved.host != nil {
		return protocol.SessionIdentity{}, errors.New("attach target is both local and remote")
	}
	if resolved.host != nil {
		target := protocol.SessionIdentity{HostID: resolved.host.ID, SessionID: resolved.remote.ID}
		if err := protocol.ValidateSessionIdentity(target); err != nil {
			return protocol.SessionIdentity{}, fmt.Errorf("remote attach target: %w", err)
		}
		return target, nil
	}
	if resolved.local == nil {
		return protocol.SessionIdentity{}, errors.New("attach target has no session")
	}

	id, err := session.ParseID(resolved.local.ID)
	if err != nil || id != resolved.local.ID {
		return protocol.SessionIdentity{}, fmt.Errorf("local attach target has invalid session ID %q", resolved.local.ID)
	}
	dir := resolved.local.Dir
	sessionsDir := filepath.Dir(dir)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Base(dir) != id || filepath.Base(sessionsDir) != "s" {
		return protocol.SessionIdentity{}, fmt.Errorf("local attach target has invalid session directory %q", dir)
	}
	local, err := identity.Load(filepath.Dir(sessionsDir))
	if err != nil {
		return protocol.SessionIdentity{}, fmt.Errorf("load local attach target identity: %w", err)
	}
	target := protocol.SessionIdentity{HostID: local.ID, SessionID: id}
	if err := protocol.ValidateSessionIdentity(target); err != nil {
		return protocol.SessionIdentity{}, fmt.Errorf("local attach target: %w", err)
	}
	return target, nil
}

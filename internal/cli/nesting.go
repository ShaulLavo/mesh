package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

// A registration belongs to the client process, not its transport to the
// destination. Reconnecting that transport must not restore the outer key.
func registerAttachmentNesting(opts AttachOptions) (transport.Conn, bool, error) {
	if opts.HostID != "" {
		if err := protocol.ValidateSessionIdentity(protocol.SessionIdentity{HostID: opts.HostID, SessionID: opts.SessionID}); err != nil {
			return nil, false, err
		}
	}
	if len(opts.ContainingSessions) == 0 {
		return nil, false, nil
	}
	location, inside := worker.ContainingSessionWorker()
	if !inside {
		return nil, SessionDepth() > 0, nil
	}
	containing := legacyContainingSession(location, os.Getenv(worker.MeshSessionIDVariable), os.Getenv(worker.MeshHostIDVariable), identity.Load)
	if len(containing) == 0 || containing[0] != opts.ContainingSessions[0] {
		// Containment is supplied by the caller that resolved the destination.
		// Do not let an unrelated ancestor of an embedded caller register on
		// its behalf merely because this library runs inside a Mesh terminal.
		return nil, false, nil
	}
	target, err := attachmentTargetIdentity(opts)
	if err != nil {
		return nil, true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), containmentQueryTimeout)
	defer cancel()
	conn, err := registerNesting(ctx, location, target)
	if err != nil {
		if errors.Is(err, errNestingRejected) {
			return nil, true, err
		}
		// Old workers reject this request or close the connection. Their key
		// ownership stays unchanged, so retain the depth-based escape key.
		return nil, true, nil
	}
	return conn, true, nil
}

var errNestingRejected = errors.New("worker rejected nesting registration")

func attachmentTargetIdentity(opts AttachOptions) (protocol.SessionIdentity, error) {
	target := protocol.SessionIdentity{HostID: opts.HostID, SessionID: opts.SessionID}
	if target.HostID == "" && opts.SocketPath != "" {
		dir := filepath.Dir(opts.SocketPath)
		sessionsDir := filepath.Dir(dir)
		// Only a direct worker socket proves this is a local destination. A
		// remote transport must never inherit MESH_HOST_ID from its parent.
		if filepath.IsAbs(dir) && filepath.Clean(opts.SocketPath) == opts.SocketPath &&
			filepath.Base(dir) == opts.SessionID && filepath.Base(sessionsDir) == "s" &&
			opts.SocketPath == paths.Socket(dir) {
			if local, err := identity.Load(filepath.Dir(sessionsDir)); err == nil {
				target.HostID = local.ID
			}
		}
	}
	return target, protocol.ValidateSessionIdentity(target)
}

func registerNesting(ctx context.Context, location worker.SessionWorkerLocation, target protocol.SessionIdentity) (transport.Conn, error) {
	if err := protocol.ValidateSessionIdentity(target); err != nil {
		return nil, err
	}
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(location.Dir))
	if err != nil {
		return nil, err
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	registered := false
	defer func() {
		if !registered {
			_ = conn.Close()
		}
	}()
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type:          protocol.TypeNest,
		RequestID:     requestID,
		SessionID:     location.SessionID,
		NestedSession: &target,
	})
	if err != nil {
		return nil, err
	}
	if !response.NestingSupported {
		return nil, errors.New("worker does not support nesting registration")
	}
	if response.Type == protocol.TypeError {
		return nil, fmt.Errorf("%w: %w", errNestingRejected, daemonResponseError("nesting", response.Message))
	}
	if response.Type != protocol.TypeNesting || response.SessionID != location.SessionID {
		return nil, fmt.Errorf("%w: unexpected worker response", errNestingRejected)
	}
	if err := protocol.ValidateContainingSessions(response.Nested); err != nil {
		return nil, fmt.Errorf("%w: %w", errNestingRejected, err)
	}
	if !slices.Contains(response.Nested, target) {
		return nil, fmt.Errorf("%w: target is missing from worker response", errNestingRejected)
	}
	registered = true
	return conn, nil
}

type attachmentKeys struct {
	mu sync.Mutex

	detachKey byte
	leaveKey  byte
	explicit  bool
	raw       bool
	outermost bool
	leaveOff  bool
	dynamic   bool
	supported bool
	nested    bool
}

func newAttachmentKeys(opts AttachOptions, depth int, inside, registered bool) *attachmentKeys {
	if inside && depth == 0 {
		depth = 1
	}
	detachKey := opts.DetachKey
	explicit := opts.DetachKeyExplicit || detachKey != 0 && detachKey != DefaultDetachKey && detachKey != DetachKeyForDepth(depth)
	if detachKey == 0 {
		detachKey = DetachKeyForDepth(depth)
	}
	if registered && !explicit {
		detachKey = DefaultDetachKey
	}
	leaveKey := opts.LeaveKey
	if leaveKey == 0 {
		leaveKey = DefaultLeaveKey
	}
	return &attachmentKeys{
		detachKey: detachKey,
		leaveKey:  leaveKey,
		explicit:  explicit,
		raw:       opts.Raw,
		outermost: !inside && depth == 0,
		leaveOff:  opts.DisableLeaveKey,
		dynamic:   (registered || !inside && depth == 0) && (opts.Raw || !explicit || detachKey != DefaultDetachKey),
	}
}

func (keys *attachmentKeys) update(message protocol.Control) error {
	if message.NestingSupported {
		if err := protocol.ValidateContainingSessions(message.Nested); err != nil {
			return err
		}
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	keys.supported = message.NestingSupported
	keys.nested = message.NestingSupported && len(message.Nested) > 0
	return nil
}

func (keys *attachmentKeys) detachIndex(input []byte) int {
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if keys.raw {
		return -1
	}
	for index, value := range input {
		if value == keys.detachKey && (keys.explicit || !keys.dynamic || !keys.nested) {
			return index
		}
		// A legacy inner client cannot register and still owns ctrl+^ at
		// depth one. Claim leave-all only while registration proves that an
		// inner client understands the new key ownership.
		if value == keys.leaveKey && keys.outermost && keys.supported && keys.nested && !keys.leaveOff {
			return index
		}
	}
	return -1
}

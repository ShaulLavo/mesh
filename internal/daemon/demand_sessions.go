package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

const (
	// A served session has no client to size it. This is wide enough that a
	// dev server's banner does not wrap when someone attaches to read it.
	servedSessionCols = 120
	servedSessionRows = 40
	servedSessionTerm = "xterm-256color"
	// servedOutputTail is how much output a failed start reads back, before
	// it is cut to the last few lines.
	servedOutputTail = 16 << 10
	servedStopPoll   = 50 * time.Millisecond
)

func demandRequestID(action string) string {
	var random [8]byte
	_, _ = rand.Read(random[:])
	return "serve-" + action + "-" + hex.EncodeToString(random[:])
}

func (l *lifecycle) startLabelled(ctx context.Context, label string, command []string, cwd string, env []string) (string, error) {
	requestID := demandRequestID("start")
	response, err := l.createSession(ctx, protocol.TypeCreate, requestID, creationRequest{
		command: command, cwd: cwd, cols: servedSessionCols, rows: servedSessionRows,
		term: servedSessionTerm, label: label, env: env,
	})
	l.forgetCreation(requestID)
	if err != nil {
		return "", err
	}
	return response.SessionID, nil
}

// forgetCreation drops the entry that makes a client's retried create
// idempotent. startLabelled never retries a request ID, so its entries would
// only accumulate, one per start.
func (l *lifecycle) forgetCreation(requestID string) {
	l.creationsMu.Lock()
	defer l.creationsMu.Unlock()
	delete(l.creations, requestID)
}

// stopSession ends a session the way `mesh kill` does and returns once its
// process group is gone. A session that already ended is already stopped.
func (l *lifecycle) stopSession(ctx context.Context, id string) error {
	if _, ended := l.sessionExit(id); ended {
		return nil
	}
	_, err := l.forwardOneShot(ctx, protocol.Control{Type: protocol.TypeKill, RequestID: demandRequestID("stop"), SessionID: id})
	if err != nil {
		if _, ended := l.sessionExit(id); ended {
			return nil
		}
		return err
	}
	// The kill acknowledgement means the process group is gone; the exit
	// record lands a moment later, and a restart must not see it running.
	for {
		if _, ended := l.sessionExit(id); ended {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("session %s: wait for exit record: %w", id, ctx.Err())
		case <-time.After(servedStopPoll):
		}
	}
}

func (l *lifecycle) sessionExit(id string) (*int, bool) {
	meta, err := worker.ReadMeta(filepath.Join(l.sessionsDir, id))
	if err != nil {
		// A session whose record is gone cannot be serving anything.
		return nil, true
	}
	if meta.State == worker.StateExited {
		return meta.ExitCode, true
	}
	// A worker killed outright never writes its exit. Its command led the
	// process group, so a missing group with no worker answering its socket
	// means nothing is left. The socket check matters: a live worker spends a
	// moment draining output between reaping the command and recording why
	// it exited, and that exit status is what a failed start reports.
	if meta.PID > 0 && errors.Is(syscall.Kill(-meta.PID, 0), syscall.ESRCH) && errors.Is(syscall.Kill(meta.PID, 0), syscall.ESRCH) {
		connection, err := net.DialTimeout("unix", paths.Socket(filepath.Join(l.sessionsDir, id)), demandProbeTimeout)
		if err != nil {
			return nil, true
		}
		_ = connection.Close()
	}
	return nil, false
}

func (l *lifecycle) outputTail(ctx context.Context, id string) string {
	var output []byte
	response, err := l.forwardOneShot(ctx, protocol.Control{
		Type: protocol.TypeLogs, RequestID: demandRequestID("logs"), SessionID: id, Tail: servedOutputTail,
	})
	if err == nil {
		output = response.Output
	} else if output, err = worker.ReadLogTail(filepath.Join(l.sessionsDir, id), servedOutputTail); err != nil {
		return ""
	}
	return lastLines(ansi.Strip(string(output)), demandFailureLines, demandFailureLineBytes)
}

// findLabelled returns the newest live session carrying label.
func (l *lifecycle) findLabelled(ctx context.Context, label string) (string, bool, error) {
	sessions, err := l.catalog.List(ctx)
	if err != nil {
		return "", false, err
	}
	var newest storage.Session
	found := false
	for _, stored := range sessions {
		if stored.State != storage.StateRunning && stored.State != storage.StateDetached {
			continue
		}
		meta, err := worker.ReadMeta(filepath.Join(l.sessionsDir, string(stored.ID)))
		if err != nil || meta.Label != label {
			continue
		}
		if _, ended := l.sessionExit(string(stored.ID)); ended {
			continue
		}
		if !found || stored.CreatedAt.After(newest.CreatedAt) {
			newest, found = stored, true
		}
	}
	return string(newest.ID), found, nil
}

// forgetLabelled removes the ended sessions an earlier start of this route
// left behind, keeping the one now running.
func (l *lifecycle) forgetLabelled(ctx context.Context, label, keep string) {
	// The catalog learns of a stop on its next pass; without this the
	// session just stopped would still read as running and be kept.
	if err := l.catalog.Reconcile(ctx); err != nil {
		return
	}
	sessions, err := l.catalog.List(ctx)
	if err != nil {
		return
	}
	for _, stored := range sessions {
		if string(stored.ID) == keep || stored.State == storage.StateRunning || stored.State == storage.StateDetached {
			continue
		}
		meta, err := worker.ReadMeta(filepath.Join(l.sessionsDir, string(stored.ID)))
		if err != nil || meta.Label != label {
			continue
		}
		_, _ = l.remove(ctx, protocol.Control{Type: protocol.TypeRemove, RequestID: demandRequestID("forget"), SessionID: string(stored.ID)})
	}
}

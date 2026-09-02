// Package worker implements mesh session-worker: a detached process that owns
// exactly one PTY and outlives every client attached to it.
package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Session states. The worker reports running while a client is attached,
// detached once the last one leaves, and exited when the command ends. The
// daemon still infers interrupted, which only a reboot can produce.
//
// The worker owns detached because only it knows whether anyone is watching.
// While nothing wrote this, every live session read as running, so a session
// left running in the background looked exactly like the one in front of you.
const (
	StateRunning     = "running"
	StateDetached    = "detached"
	StateExited      = "exited"
	StateInterrupted = "interrupted"
)

// Meta is the worker's authoritative on-disk record. It exists so a session's
// fate survives the daemon being down: the worker can record its own death
// without anyone listening.
type Meta struct {
	ID        string     `json:"id"`
	PID       int        `json:"pid"`
	Command   []string   `json:"command"`
	Cwd       string     `json:"cwd"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	ExitedAt  *time.Time `json:"exitedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	// BootID ties a running session to the kernel boot that hosted it. After a
	// reboot the PID is meaningless, so a mismatch means interrupted, not alive.
	BootID string `json:"bootId,omitempty"`
}

// WriteMeta atomically replaces the metadata file in dir.
func WriteMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("worker: encode meta: %w", err)
	}
	b = append(b, '\n')
	tmp := filepath.Join(dir, "meta.json.tmp")
	// This record is how a session's fate survives the daemon being down, so it
	// has to survive power loss too. Without the file sync a rename can land
	// ahead of the bytes and leave a zero-length meta.json, which the daemon's
	// startup scan then reads as a fatal decode error on every boot.
	temporary, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // a fixed filename under the caller's own session directory
	if err != nil {
		return fmt.Errorf("worker: write meta: %w", err)
	}
	if _, err := temporary.Write(b); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("worker: write meta: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("worker: sync meta: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("worker: close meta: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		return fmt.Errorf("worker: commit meta: %w", err)
	}
	// The rename itself needs a directory sync to be durable.
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("worker: sync session directory: %w", err)
	}
	return nil
}

// syncDirectory flushes a directory entry so a completed rename survives a
// crash. Not every platform permits opening a directory for sync; a rejection
// there is not a write failure.
func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // the caller supplies its own session directory
	if err != nil {
		return err //nolint:wrapcheck // the caller names the operation
	}
	defer directory.Close() //nolint:errcheck // the sync below is what has to succeed
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) && !errors.Is(err, syscall.EINVAL) {
		return err //nolint:wrapcheck // the caller names the operation
	}
	return nil
}

// ReadMeta loads the metadata file from dir.
func ReadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // metadata uses a fixed filename under a locally selected session directory
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("worker: decode meta in %s: %w", dir, err)
	}
	return m, nil
}

// BootID returns an identifier for the current kernel boot, or "" if the
// platform does not expose one.
func BootID() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return string(trimSpace(b))
	}
	return ""
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

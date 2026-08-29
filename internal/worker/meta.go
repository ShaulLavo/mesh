// Package worker implements mesh session-worker: a detached process that owns
// exactly one PTY and outlives every client attached to it.
package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Session states. A worker only ever reports running or exited; the daemon
// infers detached and interrupted by looking at sockets and reboots.
const (
	StateRunning     = "running"
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
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("worker: write meta: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		return fmt.Errorf("worker: commit meta: %w", err)
	}
	return nil
}

// ReadMeta loads the metadata file from dir.
func ReadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
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

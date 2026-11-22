//go:build mesh_integration

package daemon

import (
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/session/execution"
)

func init() {
	executionDaemonBoundary = holdExecutionDaemonBoundary
}

func holdExecutionDaemonBoundary(sessionsDir, stage string, r execution.Receipt) {
	path := filepath.Join(filepath.Dir(sessionsDir), ".pipe-hold-"+stage)
	key, err := os.ReadFile(path)
	if err != nil || string(key) != r.Key.Opaque {
		return
	}
	if err := os.WriteFile(path+".reached", []byte(r.Binding.Reservation.ID), 0600); err != nil {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

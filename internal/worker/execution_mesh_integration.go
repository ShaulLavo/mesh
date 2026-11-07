//go:build mesh_integration

package worker

import (
	"os"
	"path/filepath"
	"time"
)

func init() { executionBoundary = holdExecutionBoundary }
func holdExecutionBoundary(dir, stage string) {
	hold := filepath.Join(dir, ".execution-hold-"+stage)
	if _, err := os.Stat(hold); err != nil {
		return
	}
	if err := os.WriteFile(hold+".reached", []byte(stage), 0600); err != nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(hold); err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

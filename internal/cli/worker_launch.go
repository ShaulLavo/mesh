package cli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/session/execution"
)

func workerLaunchDirectory(binding *execution.Binding, requested string) (string, error) {
	if binding == nil {
		cwd, err := os.Getwd()
		return cwd, executionBoundaryError("read worker directory", err)
	}
	if !filepath.IsAbs(requested) || filepath.Clean(requested) != requested {
		return "", errors.New("worker: missing clean absolute accepted directory")
	}
	accepted, err := os.Stat(requested)
	if err != nil {
		return "", executionBoundaryError("inspect accepted worker directory", err)
	}
	current, err := os.Stat(".")
	if err != nil {
		return "", executionBoundaryError("inspect actual worker directory", err)
	}
	if !accepted.IsDir() || !os.SameFile(accepted, current) {
		return "", errors.New("worker: actual directory differs from accepted execution directory")
	}
	return requested, nil
}

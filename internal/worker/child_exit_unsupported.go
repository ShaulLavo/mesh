//go:build !linux && !darwin

package worker

import "fmt"

func childExitSupported() error {
	return fmt.Errorf("worker: non-reaping child exit observation is unavailable in this build")
}
func observeChildExit(_ int) error { return childExitSupported() }

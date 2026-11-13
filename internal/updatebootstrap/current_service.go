package updatebootstrap

import (
	"context"
	"fmt"
	"os"
)

// InspectCurrentDaemonService verifies this process's native service before its
// control socket exists. It reads the real invocation and manager PID binding.
func InspectCurrentDaemonService(ctx context.Context, stateDir string) error {
	pid := os.Getpid()
	invocation, err := inspectInvocation(ctx, pid)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executing daemon service image: %w", err)
	}
	settings, _, err := invocationSettings(stateDir, Observation{Executable: executable, PID: pid}, invocation)
	if err != nil {
		return err
	}
	_, err = inspectDaemonService(ctx, pid, &settings, invocation)
	return err
}

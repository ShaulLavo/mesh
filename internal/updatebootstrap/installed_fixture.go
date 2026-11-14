//go:build mesh_installed_fixture

package updatebootstrap

import "context"

// InstalledEnvironmentHashes exposes no environment values. The native harness
// uses the same process observer as production to diagnose preservation holds.
func InstalledEnvironmentHashes(ctx context.Context, pid int) (map[string]string, error) {
	invocation, err := inspectInvocation(ctx, pid)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(invocation.Environment))
	for name, value := range invocation.Environment {
		result[name] = installationDigest([]byte(value))
	}
	return result, nil
}

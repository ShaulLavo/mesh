package worker

import "strings"

// ObserveChildExit blocks until the caller's child pid has exited without
// reaping it, so its PID and process-group ID stay reserved until the caller
// reaps it. Request-scoped runs use it to signal the group safely after the
// leader is gone.
func ObserveChildExit(pid int) error { return observeChildExit(pid) }

// ChildExitSupported reports whether this build can observe a child's exit
// without reaping it.
func ChildExitSupported() error { return childExitSupported() }

// RunEnvironment is the environment of a request-scoped run: it names the
// host but no session, and drops the session and nesting variables a
// daemon started inside a session would otherwise pass on.
func RunEnvironment(env []string, hostID string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case MeshDepthVariable, MeshSessionIDVariable, MeshTerminalIDVariable, MeshHostIDVariable:
			continue
		}
		out = append(out, entry)
	}
	if hostID == "" {
		return out
	}
	return append(out, MeshHostIDVariable+"="+hostID)
}

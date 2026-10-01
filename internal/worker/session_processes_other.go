//go:build !linux

package worker

// SessionProcesses has no answer where Mesh cannot attribute a socket to a
// process; callers there check listeners by address alone.
func SessionProcesses(string, int) ([]int, error) { return nil, nil }

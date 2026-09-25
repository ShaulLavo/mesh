//go:build linux

package worker

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	scopeCallTimeout = 2 * time.Second
	scopeSettle      = time.Second
)

// IsolateSession moves this worker into its own systemd scope before it starts
// the session's command. Workers otherwise live in whatever unit launched them,
// usually mesh.service, and systemd-oomd kills a whole cgroup: one runaway
// session took every other session and the daemon with it. Best effort; a host
// without a user manager keeps the old placement.
func IsolateSession(id string) {
	current, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return
	}
	unit := sessionScopeUnit(id)
	path := unifiedCgroupPath(string(current))
	if path == "" || strings.HasSuffix(path, "/"+unit) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), scopeCallTimeout)
	defer cancel()
	args := startScopeArgs(id, os.Getpid(), parentSlice(path))
	if output, err := exec.CommandContext(ctx, "busctl", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed program; the unit name is built from a validated session ID
		log.Printf("worker: own scope %s: %v: %s", unit, err, strings.TrimSpace(string(output)))
		return
	}
	// The move lands when systemd starts the scope, which can trail the call.
	// The command must fork after it, or it stays in the old cgroup.
	deadline := time.Now().Add(scopeSettle)
	for time.Now().Before(deadline) {
		current, err := os.ReadFile("/proc/self/cgroup")
		if err == nil && strings.HasSuffix(unifiedCgroupPath(string(current)), "/"+unit) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Printf("worker: own scope %s: not joined within %s", unit, scopeSettle)
}

func sessionScopeUnit(id string) string {
	return "mesh-session-" + id + ".scope"
}

func startScopeArgs(id string, pid int, slice string) []string {
	properties := []string{
		"PIDs", "au", "1", strconv.Itoa(pid),
		"Description", "s", "Mesh session " + id,
		"CollectMode", "s", "inactive-or-failed",
	}
	count := 3
	if slice != "" {
		properties = append(properties, "Slice", "s", slice)
		count++
	}
	args := []string{"--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", sessionScopeUnit(id), "fail", strconv.Itoa(count)}
	args = append(args, properties...)
	return append(args, "0")
}

// unifiedCgroupPath reads the cgroup v2 line of /proc/self/cgroup.
func unifiedCgroupPath(contents string) string {
	for line := range strings.SplitSeq(contents, "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	return ""
}

// parentSlice keeps the scope beside the unit that launched the worker, so a
// session started by mesh.service in app.slice stays in app.slice.
func parentSlice(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 2 {
		return ""
	}
	parent := segments[len(segments)-2]
	if !strings.HasSuffix(parent, ".slice") {
		return ""
	}
	return parent
}

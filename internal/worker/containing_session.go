package worker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/session"
)

const containingSessionAncestorLimit = 64

type ancestorProcess struct {
	parentID int
	args     []string
}

type ancestorProcessReader func(int) (ancestorProcess, bool)

// SessionWorkerLocation identifies the exact local worker process that owns the
// calling process's terminal. Dir is the worker's validated session directory.
type SessionWorkerLocation struct {
	SessionID string
	Dir       string
}

// ContainingSessionWorker walks the bounded parent chain and returns the exact
// local worker socket location. False means the caller is not inside a current
// Mesh worker or the worker's arguments are not safe to use as a path.
func ContainingSessionWorker() (SessionWorkerLocation, bool) {
	return containingSessionWorkerFromAncestors(os.Getpid(), readAncestorProcess)
}

// ContainingSessionID finds the session worker above the calling process. New
// workers also export MESH_SESSION_ID, but walking the bounded parent chain
// lets an updated client do the right thing inside sessions that were already
// running when that identity was introduced.
func ContainingSessionID() string {
	return containingSessionIDFromAncestors(os.Getpid(), readAncestorProcess)
}

func containingSessionWorkerFromAncestors(pid int, read ancestorProcessReader) (SessionWorkerLocation, bool) {
	seen := make(map[int]struct{}, containingSessionAncestorLimit)
	for range containingSessionAncestorLimit {
		if pid <= 1 {
			return SessionWorkerLocation{}, false
		}
		if _, exists := seen[pid]; exists {
			return SessionWorkerLocation{}, false
		}
		seen[pid] = struct{}{}
		process, ok := read(pid)
		if !ok {
			return SessionWorkerLocation{}, false
		}
		if location, ok := sessionWorkerLocationFromArgs(process.args); ok {
			return location, true
		}
		pid = process.parentID
	}
	return SessionWorkerLocation{}, false
}

func containingSessionIDFromAncestors(pid int, read ancestorProcessReader) string {
	seen := make(map[int]struct{}, containingSessionAncestorLimit)
	for range containingSessionAncestorLimit {
		if pid <= 1 {
			return ""
		}
		if _, exists := seen[pid]; exists {
			return ""
		}
		seen[pid] = struct{}{}
		process, ok := read(pid)
		if !ok {
			return ""
		}
		if id := SessionIDFromWorkerArgs(process.args); id != "" {
			return id
		}
		pid = process.parentID
	}
	return ""
}

// SessionIDFromWorkerArgs recognises the exact `mesh session-worker --id ID`
// argv, returning "" for anything else.
func SessionIDFromWorkerArgs(args []string) string {
	if len(args) < 4 || args[1] != "session-worker" {
		return ""
	}
	value, count := workerFlagValue(args, "--id")
	if count != 1 {
		return ""
	}
	id, err := session.ParseID(value)
	if err != nil {
		return ""
	}
	return id
}

func sessionWorkerLocationFromArgs(args []string) (SessionWorkerLocation, bool) {
	id := SessionIDFromWorkerArgs(args)
	if id == "" {
		return SessionWorkerLocation{}, false
	}
	dir, count := workerFlagValue(args, "--dir")
	if count != 1 || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Base(dir) != id {
		return SessionWorkerLocation{}, false
	}
	return SessionWorkerLocation{SessionID: id, Dir: dir}, true
}

func workerFlagValue(args []string, name string) (string, int) {
	value := ""
	count := 0
	for index := 2; index < len(args); index++ {
		if args[index] == "--" {
			break
		}
		if args[index] != name || index+1 >= len(args) || args[index+1] == "--" {
			continue
		}
		value = args[index+1]
		count++
		index++
	}
	return value, count
}

// hookFromAgent accepts a hook only from the provider Mesh launched: a direct
// child of the agent helper, reached directly or through the shell that runs
// hooks. A provider started below it (a test, a headless `claude -p`) inherits
// MESH_AGENT_* too, and its SessionStart overwrote the saved conversation.
func hookFromAgent(peer, agent int, read ancestorProcessReader) bool {
	if peer <= 1 || agent <= 1 {
		return false
	}
	if peer == agent {
		return true
	}
	hook, ok := read(peer)
	if !ok {
		return false
	}
	runner, ok := read(hook.parentID)
	if !ok {
		return false
	}
	if runner.parentID == agent {
		return true
	}
	if !isHookShell(runner.args) {
		return false
	}
	provider, ok := read(runner.parentID)
	return ok && provider.parentID == agent
}

func isHookShell(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.TrimPrefix(filepath.Base(args[0]), "-") {
	case "sh", "bash", "dash", "zsh":
		return true
	}
	return false
}

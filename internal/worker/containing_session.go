package worker

import (
	"os"
	"path/filepath"

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
		if id := sessionIDFromWorkerArgs(process.args); id != "" {
			return id
		}
		pid = process.parentID
	}
	return ""
}

func sessionIDFromWorkerArgs(args []string) string {
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
	id := sessionIDFromWorkerArgs(args)
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

package worker

import "github.com/shaul/mesh/internal/procmem"

// SessionMemory measures a live session's worker and everything it runs.
func SessionMemory(table procmem.Table, sessionID string, childPID int) uint64 {
	return table.Tree(sessionRoot(table, sessionID, childPID))
}

// sessionRoot climbs from the child to its parent only when that parent is
// this session's own worker, so a reparented child is never measured with its
// adopter's whole tree.
func sessionRoot(table procmem.Table, sessionID string, childPID int) int {
	if parent := table.Parent(childPID); parent > 1 && SessionIDFromWorkerArgs(table.Command(parent)) == sessionID {
		return parent
	}
	return childPID
}

// Package procmem measures the memory a process tree holds, so a session's
// cost can be shown next to it. Linux reports proportional set size plus
// swapped pages, which divides shared pages fairly between sessions; other
// platforms fall back to resident size.
package procmem

// Table is one observation of the host's process parentage. Measure trees from
// one Table so a list of sessions costs a single process scan.
type Table struct {
	parents  map[int]int
	children map[int][]int
	sample   func(pid int) (uint64, bool)
}

// Tree sums the memory of root and every descendant still in the table. It
// returns zero when root is gone or nothing could be measured.
func (t Table) Tree(root int) uint64 {
	if root <= 0 || t.sample == nil {
		return 0
	}
	var total uint64
	pending := []int{root}
	seen := map[int]bool{}
	for len(pending) > 0 {
		pid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if bytes, ok := t.sample(pid); ok {
			total += bytes
		}
		pending = append(pending, t.children[pid]...)
	}
	return total
}

// Parent returns pid's parent, or zero when unknown.
func (t Table) Parent(pid int) int { return t.parents[pid] }

// SessionRoot returns the `mesh session-worker` that owns a session's child
// process, so the worker's own memory is counted with the session. It falls
// back to the child when the parent is anything else, which also stops a
// reparented child from being measured with its adoptive parent's whole tree.
func (t Table) SessionRoot(childPID int, sessionID string) int {
	parent := t.Parent(childPID)
	if parent <= 1 {
		return childPID
	}
	arguments := commandLine(parent)
	for index := 0; index+2 < len(arguments); index++ {
		if arguments[index] == "session-worker" && arguments[index+1] == "--id" && arguments[index+2] == sessionID {
			return parent
		}
	}
	return childPID
}

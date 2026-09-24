package worker

import (
	"os/exec"
	"runtime"
	"runtime/debug"

	"github.com/charmbracelet/x/xpty"
)

const (
	workerGCPercent = 25
	workerMaxProcs  = 2
)

// TuneProcess sizes the Go runtime for a process that hosts one terminal. Call
// it first thing in a session-worker process; it changes process-wide state.
//
// With THP set to "always", khugepaged collapsed the sparse heap back into
// 2 MiB pages the scavenger could not return, several times the live heap.
// The live heap itself is a few MB of ring and emulator, and GOGC=100 let
// garbage settle at that size again for the life of the session; the
// pointerful part is small, so collecting at 25% growth cost no measurable
// throughput. Output is serialized through one lock, so more Ps bought only
// per-P caches and threads.
func TuneProcess() {
	setHugePages(false)
	debug.SetGCPercent(workerGCPercent)
	runtime.GOMAXPROCS(min(workerMaxProcs, runtime.GOMAXPROCS(0)))
}

// startSession starts the session's command under the system's THP policy: an
// opt-out survives fork and exec, and Start returns only once the child has
// exec'd. Returning free pages afterwards splits any huge page startup
// faulted, and the restored opt-out keeps khugepaged from collapsing it again.
func startSession(pty xpty.Pty, cmd *exec.Cmd) error {
	if !hugePagesDisabled() {
		return pty.Start(cmd)
	}
	setHugePages(true)
	err := pty.Start(cmd)
	setHugePages(false)
	debug.FreeOSMemory()
	return err
}

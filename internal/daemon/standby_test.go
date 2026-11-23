package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

// A keyed job is run by the waiting standby once one is ready, with the same
// result and output a fresh worker gives (and no inherited handoff socket),
// and another standby replaces it.
func TestKeyedJobRunsInTheStandbyWorker(t *testing.T) {
	f := newNativePipeDaemon(t)
	served := map[string]bool{}
	for attempt := 0; len(served) < 2 && attempt < 40; attempt++ {
		key := "standby-" + string(rune('a'+attempt%26)) + string(rune('a'+attempt/26))
		req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: key, RequestKey: key, ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf \"$PWD\"; [ -e /proc/self/fd/3 ] && printf ' inherited fd 3'; exit 4"}}
		job := f.request(t, req).ExecutionReceipt
		wait := boundPipeRequest(protocol.TypeExecutionWait, job)
		wait.TimeoutMillis = 10000
		done := f.request(t, wait)
		if done.ExecutionWait != "completed" || done.ExecutionReceipt.Result.Termination.Code == nil || *done.ExecutionReceipt.Result.Termination.Code != 4 {
			t.Fatalf("job %s = %+v", job.SessionID, done)
		}
		assertPipeBytes(t, f, job, execution.StreamStdout, []byte(f.root))
		if _, err := os.Stat(filepath.Join(f.root, "standby-"+job.SessionID)); err == nil {
			served[job.SessionID] = true
		}
	}
	if len(served) < 2 {
		t.Fatalf("standby workers served %d jobs, want a standby that is replaced after use", len(served))
	}
}

// A standby owns nothing, so it exits once its daemon is gone.
func TestStandbyExitsWithItsDaemon(t *testing.T) {
	f := newNativePipeDaemon(t)
	var standby int
	pipeEventually(t, func() bool {
		standby = standbyProcess(t, f.sessions)
		return standby != 0
	})
	f.stop(t)
	pipeEventually(t, func() bool {
		_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(standby)))
		return errors.Is(err, os.ErrNotExist) || !strings.Contains(readCmdline(standby), "--standby")
	})
}

func standbyProcess(t *testing.T, sessions string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip("standby processes are found through /proc")
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		args := readCmdline(pid)
		if strings.Contains(args, "--standby") && strings.Contains(args, sessions+string(filepath.Separator)) {
			return pid
		}
	}
	return 0
}

func readCmdline(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")) //nolint:gosec // a /proc path built from a number
	if err != nil {
		return ""
	}
	return string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}))
}

// The pool hands its standby only to the reservation that took its ID, and
// starts nothing before it is activated or after it is closed.
func TestStandbyPoolHandsOutOnlyByID(t *testing.T) {
	started := 0
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The fixture standby is this test binary, which waits like a real one.
	pool := newStandbyPool(filepath.Join(root, "s"), executable, func() []string { return []string{pipeNativeFixture + "=" + root} })
	pool.start = func(sessionsDir, executable string, env []string) (*worker.Standby, error) {
		started++
		s, err := worker.StartStandby(sessionsDir, executable, env)
		if err != nil {
			return nil, fmt.Errorf("fixture standby: %w", err)
		}
		t.Cleanup(func() { s.Discard(); <-s.Exited() })
		return s, nil
	}
	pool.refill()
	if started != 0 || pool.preferredID() != "" {
		t.Fatal("an inactive pool started a standby")
	}
	pool.activate()
	id := pool.preferredID()
	if started != 1 || id == "" {
		t.Fatalf("activated pool: started %d, preferred %q", started, id)
	}
	// The request that gets the ID claims the standby: an overlapping request
	// that sees its session directory does not disturb the claim.
	pool.reserved(id, id)
	if err := os.MkdirAll(filepath.Join(root, "s", id), 0o700); err != nil {
		t.Fatal(err)
	}
	if pool.preferredID() == id {
		t.Fatal("a claimed standby was offered again")
	}
	if pool.take("other") != nil || pool.take(id) == nil || pool.take(id) != nil {
		t.Fatal("standby was handed to the wrong reservation or twice")
	}
	// A claim whose request ended before launching is released.
	pipeEventually(t, func() bool { return pool.preferredID() != "" })
	abandoned := pool.preferredID()
	pool.reserved(abandoned, abandoned)
	pool.drop(abandoned)
	if pool.take(abandoned) != nil {
		t.Fatal("a dropped claim was still handed out")
	}
	// A waiting standby whose ID an old session holds, or that a reservation
	// wanted and missed, is dropped and replaced.
	pipeEventually(t, func() bool { return pool.preferredID() != "" })
	occupied := pool.preferredID()
	if err := os.MkdirAll(filepath.Join(root, "s", occupied), 0o700); err != nil {
		t.Fatal(err)
	}
	if pool.preferredID() != "" {
		t.Fatal("a standby whose session directory exists was still offered")
	}
	pipeEventually(t, func() bool { return pool.preferredID() != "" && pool.preferredID() != occupied })
	missed := pool.preferredID()
	pool.reserved(missed, "OTHR")
	if pool.preferredID() == missed {
		t.Fatal("a standby whose ID another reservation missed was kept")
	}
	pipeEventually(t, func() bool { return pool.preferredID() != "" })
	pool.close()
	pool.refill()
	if started != 5 || pool.preferredID() != "" {
		t.Fatalf("after close: started %d, preferred %q", started, pool.preferredID())
	}
}

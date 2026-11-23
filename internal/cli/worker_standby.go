package cli

import (
	"errors"
	"io"
	"os"

	"github.com/shaul/mesh/internal/worker"
)

// standbyHandoffFD is where the daemon passes a standby's launch socket.
const standbyHandoffFD = 3

// workerEntry handles what a worker does before its own launch: a missing
// identity is an error, and a standby waits for its launch instead.
func (a *application) workerEntry(id, dir string, standby bool) (bool, error) {
	if id == "" || dir == "" {
		return true, errors.New("session-worker requires --id and --dir")
	}
	if standby {
		return true, a.runStandbyWorker(id, dir)
	}
	return false, nil
}

// runStandbyWorker waits for the one launch a standby worker is given, takes
// on the directory, environment and log a fresh worker would have started
// with, and then runs the worker command with exactly the arguments a fresh
// worker would have received. A socket that closes first means the daemon no
// longer needs it, and the standby exits having owned nothing.
func (a *application) runStandbyWorker(id, dir string) error {
	worker.WarmStandby()
	// Join this session's own scope now, while nothing waits on it. The
	// worker command repeats both calls after the handoff, where they find
	// the work done. A standby that is never used takes its scope with it.
	worker.TuneProcess()
	worker.IsolateSession(id)
	launch, err := worker.AwaitStandbyLaunch(os.NewFile(standbyHandoffFD, "standby-handoff"), id, dir)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err //nolint:wrapcheck // the worker package names the failure
	}
	if err := launch.Apply(dir); err != nil {
		return err //nolint:wrapcheck // the worker package names the failure
	}
	// Nothing runs before the daemon has the acknowledgement; without it the
	// daemon starts a fresh worker instead.
	if err := launch.Accept(); err != nil {
		return err //nolint:wrapcheck // the worker package names the failure
	}
	command := a.workerCommand()
	command.SetArgs(launch.Args[1:])
	command.SilenceUsage, command.SilenceErrors = true, true
	return command.Execute() //nolint:wrapcheck // the worker command's own error and status
}

// Package oneshot runs one request-scoped command for execution.run: no
// session, no worker and no journal record. While the daemon is alive the
// command's process group never outlives its request; a daemon crash leaves
// it like an SSH exec channel would.
package oneshot

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/worker"
)

// Grace is how long the group has after the first signal before SIGKILL.
const Grace = 500 * time.Millisecond

// Spec is one resolved command. Path is already resolved against the run's
// own PATH, so the daemon's PATH never decides what runs.
type Spec struct {
	Path        string
	Args        []string
	Dir         string
	Env         []string
	OutputBytes int
	Timeout     time.Duration
}

// Result is what the run observed. State is the leader's own status;
// TimedOut says the deadline fired, whatever the leader then reported.
type Result struct {
	State          *os.ProcessState
	WaitErr        error
	TimedOut       bool
	Stdout, Stderr Stream
	Duration       time.Duration
}

// Run is one started command. Its single owner goroutine signals the group,
// closes both pipe readers and reaps the leader exactly once.
type Run struct {
	cmd        *exec.Cmd
	started    time.Time
	cancel     chan struct{}
	cancelOnce sync.Once
	done       chan struct{}
	result     Result
}

// Start launches spec in its own session and process group with stdin on
// /dev/null.
func Start(spec Spec) (*Run, error) {
	if err := worker.ChildExitSupported(); err != nil {
		return nil, fmt.Errorf("oneshot: %w", err)
	}
	if len(spec.Args) == 0 || spec.OutputBytes < 1 || spec.Timeout <= 0 {
		return nil, errors.New("oneshot: invalid run")
	}
	pipes, err := openPipes()
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: spec.Path, Args: spec.Args, Dir: spec.Dir, Env: spec.Env, Stdout: pipes.outWrite, Stderr: pipes.errWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	pipes.closeWriters()
	if err != nil {
		pipes.closeReaders()
		return nil, fmt.Errorf("start %s: %w", spec.Args[0], err)
	}
	r := &Run{cmd: cmd, started: time.Now(), cancel: make(chan struct{}), done: make(chan struct{})}
	streams := []*stream{newStream(pipes.outRead, spec.OutputBytes), newStream(pipes.errRead, spec.OutputBytes)}
	go r.supervise(streams, spec.Timeout)
	return r, nil
}

// Cancel hangs the command up: the caller left, its grant ended or the
// daemon is stopping. It never waits.
func (r *Run) Cancel() { r.cancelOnce.Do(func() { close(r.cancel) }) }

// Done is closed once the group was cleaned up and the leader reaped.
func (r *Run) Done() <-chan struct{} { return r.done }

// Result is valid after Done.
func (r *Run) Result() Result { return r.result }

func (r *Run) supervise(streams []*stream, timeout time.Duration) {
	defer close(r.done)
	group := r.cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- worker.ObserveChildExit(group) }()
	for _, s := range streams {
		go s.read()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	c := &cleanup{group: group, streams: streams, exited: exited, deadline: deadline.C, cancel: r.cancel, stopDrain: make(chan struct{})}
	c.run()
	r.result.TimedOut = c.timedOut
	// The unreaped leader still fences its group ID, so this reaches only the
	// command's own group: members left behind by a normal exit included.
	signalGroup(group, syscall.SIGKILL)
	for _, s := range streams {
		s.cut()
		<-s.done
	}
	r.finish(streams)
}

func (r *Run) finish(streams []*stream) {
	err := r.cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = nil
	}
	r.result.State, r.result.WaitErr = r.cmd.ProcessState, err
	r.result.Stdout, r.result.Stderr = streams[0].result(), streams[1].result()
	r.result.Duration = time.Since(r.started)
}

func signalGroup(group int, sig syscall.Signal) { _ = syscall.Kill(-group, sig) }

type pipes struct {
	outRead, outWrite, errRead, errWrite *os.File
}

func openPipes() (*pipes, error) {
	p := &pipes{}
	var err error
	if p.outRead, p.outWrite, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("oneshot: open stdout pipe: %w", err)
	}
	if p.errRead, p.errWrite, err = os.Pipe(); err != nil {
		p.closeWriters()
		p.closeReaders()
		return nil, fmt.Errorf("oneshot: open stderr pipe: %w", err)
	}
	return p, nil
}

func (p *pipes) closeWriters() {
	for _, f := range []*os.File{p.outWrite, p.errWrite} {
		if f != nil {
			_ = f.Close()
		}
	}
}

func (p *pipes) closeReaders() {
	for _, f := range []*os.File{p.outRead, p.errRead} {
		if f != nil {
			_ = f.Close()
		}
	}
}

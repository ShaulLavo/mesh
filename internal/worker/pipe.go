package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

type pipeExecution struct {
	input      *pipeInput
	lines      *lineOutput
	cancelling atomic.Bool
}

type pipeReader struct {
	file    *os.File
	capture *execution.Capture
	done    chan string
}

type childPipes struct {
	stdout, stderr, stdin *os.File
	childFiles            []*os.File
}

func openChildPipes(cmd *exec.Cmd, openStdin bool) (*childPipes, error) {
	p := &childPipes{}
	if err := p.open(cmd, openStdin); err != nil {
		p.close()
		return nil, fmt.Errorf("worker: open child pipes: %w", err)
	}
	return p, nil
}

func (p *childPipes) open(cmd *exec.Cmd, openStdin bool) error {
	var err error
	p.stdout, cmd.Stdout, err = p.openOutput()
	if err != nil {
		return err
	}
	p.stderr, cmd.Stderr, err = p.openOutput()
	if err != nil {
		return err
	}
	if !openStdin {
		return nil
	}
	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("open stdin: %w", err)
	}
	p.stdin, cmd.Stdin = write, read
	p.childFiles = append(p.childFiles, read)
	return nil
}

func (p *childPipes) openOutput() (*os.File, *os.File, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("open output pipe: %w", err)
	}
	p.childFiles = append(p.childFiles, write)
	return read, write, nil
}

func (p *childPipes) childStarted() {
	for _, file := range p.childFiles {
		_ = file.Close()
	}
	p.childFiles = nil
}

func (p *childPipes) close() {
	p.childStarted()
	for _, file := range []*os.File{p.stdout, p.stderr, p.stdin} {
		if file != nil {
			_ = file.Close()
		}
	}
}

func runPipe(cfg Config, sid protocol.SessionID, sockPath string) (int, error) {
	attempt, err := prepareExecution(cfg)
	if err != nil {
		return 0, err
	}
	defer func() { executionBoundary(cfg.Dir, "before_lifetime_release"); _ = attempt.Close() }()

	mode := cfg.Execution.Mode()
	w := &Worker{cfg: cfg, sid: sid, pipe: &pipeExecution{}, exited: make(chan struct{}), executionClients: newExecutionClients()}
	w.cmd = exec.Command(cfg.Command[0], cfg.Command[1:]...) //nolint:gosec // explicitly chosen execution argv
	w.cmd.Dir, w.cmd.Env = cfg.Cwd, cfg.Env
	w.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	code, err := w.runPipe(attempt, mode, sockPath)
	return code, workerExecutionError("pipe worker", err)
}

func (w *Worker) runPipe(attempt *execution.Attempt, mode, sockPath string) (int, error) {
	started := false
	defer func() {
		if !started {
			_ = finishUnstartedCapture(attempt, nil, *w.cfg.Execution)
		}
	}()
	if err := childExitSupported(); err != nil {
		return 0, err
	}
	p, err := openChildPipes(w.cmd, w.cfg.Execution.Stdin == execution.StdinOpen)
	if err != nil {
		return 0, err
	}
	defer p.close()
	if p.stdin != nil {
		if err := attempt.PrepareInput(); err != nil {
			return 0, workerExecutionError("initialize input evidence", err)
		}
	}
	readers, err := w.openPipeReaders(p)
	if err != nil {
		return 0, err
	}
	defer closePipeCaptures(readers)
	if mode == execution.KindLine {
		w.pipe.lines = newLineOutput()
	}
	build := buildPointer()
	executionBoundary(w.cfg.Dir, "before_spawn")
	if err := w.cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", w.cfg.Command[0], err)
	}
	started = true
	defer w.abortPipe()
	p.childStarted()
	if err := attempt.Started(); err != nil {
		return 0, fmt.Errorf("record child start: %w", err)
	}
	executionBoundary(w.cfg.Dir, "after_spawn")
	meta := Meta{Build: build, Reservation: w.cfg.Reservation, ID: w.cfg.ID, PID: w.cmd.Process.Pid, Command: w.cfg.Command, Cwd: w.cfg.Cwd, State: StateDetached, CreatedAt: time.Now(), BootID: BootID(), Label: w.cfg.Label, ExecutionKind: mode}
	return w.serveStartedPipe(attempt, p, readers, sockPath, meta)
}

func (w *Worker) serveStartedPipe(attempt *execution.Attempt, p *childPipes, readers []pipeReader, sockPath string, meta Meta) (int, error) {
	ln, err := w.publishPipe(attempt, sockPath, meta)
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close(); _ = os.Remove(sockPath) }()
	if p.stdin != nil {
		capacity := session.MaxInputBytes
		if w.pipe.lines != nil {
			capacity = lineOutputBytes
		}
		w.pipe.input = newPipeInput(p.stdin, attempt, capacity)
		defer w.pipe.input.stop()
	}
	for _, reader := range readers {
		go reader.read()
	}
	var lineDone <-chan struct{}
	if w.pipe.lines != nil {
		lineDone = w.pipe.lines.readFrom(p.stdout)
	}
	servingDone := make(chan struct{})
	go func() { defer close(servingDone); w.acceptLoop(ln) }()
	defer w.stopExecutionServing(ln, servingDone)
	return w.finishPipe(attempt, readers, p.stdout, lineDone, meta)
}

func (w *Worker) publishPipe(attempt *execution.Attempt, sockPath string, meta Meta) (net.Listener, error) {
	if err := WriteMeta(w.cfg.Dir, meta); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", sockPath, err)
	}
	if err := attempt.Published(); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("publish pipe worker: %w", err)
	}
	return ln, nil
}

func (w *Worker) openPipeReaders(p *childPipes) ([]pipeReader, error) {
	b := *w.cfg.Execution
	state := filepath.Dir(filepath.Dir(w.cfg.Dir))
	readers := make([]pipeReader, 0, len(b.Streams()))
	for _, stream := range b.Streams() {
		capture, err := execution.OpenCapture(state, b, stream)
		if err != nil {
			closePipeCaptures(readers)
			return nil, workerExecutionError("initialize pipe capture", err)
		}
		file := p.stderr
		if stream == execution.StreamStdout {
			file = p.stdout
		}
		readers = append(readers, pipeReader{file: file, capture: capture, done: make(chan string, 1)})
	}
	return readers, nil
}

func closePipeCaptures(readers []pipeReader) {
	for _, reader := range readers {
		_, _ = reader.capture.Finish(context.Background(), "interrupted")
	}
}

func (r pipeReader) read() {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.file.Read(buf)
		if n != 0 {
			r.capture.OfferExact(buf[:n])
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF):
			r.done <- "eof"
		case errors.Is(err, os.ErrClosed):
			r.done <- "drain_timeout"
		default:
			r.done <- "read_error"
		}
		return
	}
}

func (w *Worker) abortPipe() {
	if w.reaped {
		return
	}
	_, _ = w.deliverSignal(w.cfg.Execution.Reservation.Instance, "kill")
	_, _ = w.reapChild()
	w.finish(-1)
}

func (w *Worker) finishPipe(attempt *execution.Attempt, readers []pipeReader, stdout *os.File, lineDone <-chan struct{}, meta Meta) (int, error) {
	if err := observeChildExit(w.cmd.Process.Pid); err != nil {
		// Unblock any control awaiting completion while we still own the group.
		w.abortPipe()
		return -1, err
	}
	if w.pipe.input != nil {
		w.pipe.input.stop()
	}
	deadline := time.Now().Add(ptyDrainTimeout)
	reasons := drainPipe(readers, stdout, lineDone, deadline)
	state, err := w.reapChild()
	code := -1
	if state != nil {
		code = state.ExitCode()
	}
	w.finish(code)
	if err != nil {
		return code, err
	}
	ended := time.Now().UTC()
	result := execution.Result{Binding: *w.cfg.Execution, Termination: executionTermination(state, code), EndedAt: &ended, Capture: captureComplete}
	for index, reader := range readers {
		stream, err := reader.capture.Finish(context.Background(), reasons[index])
		if err != nil {
			return code, workerExecutionError("finish pipe capture", err)
		}
		result.Streams = append(result.Streams, stream)
		if captureStatus(stream) != captureComplete {
			result.Capture = captureIncomplete
		}
	}
	if err := attempt.Finish(result); err != nil {
		return code, workerExecutionError("record pipe result", err)
	}
	meta.State, meta.ExitCode, meta.ExitedAt = StateExited, &code, &ended
	if err := w.recordExit(meta); err != nil {
		return code, err
	}
	if w.pipe.lines != nil {
		w.flushLineAttachment()
	}
	return code, nil
}

func drainPipe(readers []pipeReader, stdout *os.File, lineDone <-chan struct{}, deadline time.Time) []string {
	reasons := make([]string, len(readers))
	for index, reader := range readers {
		select {
		case reasons[index] = <-reader.done:
		case <-time.After(time.Until(deadline)):
			_ = reader.file.Close()
			reasons[index] = <-reader.done
		}
	}
	if lineDone != nil {
		select {
		case <-lineDone:
		case <-time.After(time.Until(deadline)):
			_ = stdout.Close()
			<-lineDone
		}
	}
	return reasons
}

func (w *Worker) flushLineAttachment() {
	done := w.pipe.lines.attachmentDone()
	if done == nil {
		return
	}
	timer := time.NewTimer(finalFlushTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

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
	"sync"
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
	// waitingSince is when the reader began waiting for the pipe to produce
	// bytes, on the monotonic clock (see pipeNow). It is zero while the reader
	// holds bytes it has not yet handed to the capture, because then the pipe is
	// not what the drain is waiting on.
	waitingSince atomic.Int64
}

// pipeEpoch anchors the drain clock. Offsets from it come from the monotonic
// reading, so a wall clock step cannot shorten or stretch a silence window.
var pipeEpoch = time.Now()

// pipeNow is the monotonic offset from pipeEpoch. It is never zero, so zero can
// mean "not waiting".
func pipeNow() int64 { return int64(time.Since(pipeEpoch)) + 1 }

// silentFor reports how long the reader has waited on a silent pipe, counting
// no earlier than the start of the drain.
func (r *pipeReader) silentFor(drainStart time.Time) time.Duration {
	since := r.waitingSince.Load()
	if since == 0 {
		return 0
	}
	return time.Duration(pipeNow() - max(since, int64(drainStart.Sub(pipeEpoch))+1))
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
	defer abortPipeCaptures(readers)
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

func (w *Worker) serveStartedPipe(attempt *execution.Attempt, p *childPipes, readers []*pipeReader, sockPath string, meta Meta) (int, error) {
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
		w.pipe.input = newPipeInput(p.stdin, attempt, capacity, w.pipe.lines != nil)
		defer w.pipe.input.stop()
	}
	readCtx, stopReaders := context.WithCancel(context.Background())
	defer stopReaders()
	for _, reader := range readers {
		go reader.read(readCtx)
	}
	var lineDone <-chan struct{}
	if w.pipe.lines != nil {
		lineDone = w.pipe.lines.readFrom(p.stdout)
	}
	servingDone := make(chan struct{})
	go func() { defer close(servingDone); w.acceptLoop(ln) }()
	defer w.stopExecutionServing(ln, servingDone)
	return w.finishPipe(attempt, readers, p.stdout, lineDone, stopReaders, meta)
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

func (w *Worker) openPipeReaders(p *childPipes) ([]*pipeReader, error) {
	b := *w.cfg.Execution
	state := filepath.Dir(filepath.Dir(w.cfg.Dir))
	readers := make([]*pipeReader, 0, len(b.Streams()))
	for _, stream := range b.Streams() {
		capture, err := execution.OpenCapture(state, b, stream)
		if err != nil {
			abortPipeCaptures(readers)
			return nil, workerExecutionError("initialize pipe capture", err)
		}
		file := p.stderr
		if stream == execution.StreamStdout {
			file = p.stdout
		}
		readers = append(readers, &pipeReader{file: file, capture: capture, done: make(chan string, 1)})
	}
	return readers, nil
}

func abortPipeCaptures(readers []*pipeReader) {
	for _, reader := range readers {
		reader.capture.Abort()
	}
}

func (r *pipeReader) read(ctx context.Context) {
	buf := make([]byte, 32<<10)
	for {
		r.waitingSince.Store(pipeNow())
		n, err := r.file.Read(buf)
		r.waitingSince.Store(0)
		if n != 0 && !r.capture.OfferExact(ctx, buf[:n]) {
			r.done <- pipeDrainCutoff
			return
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF):
			r.done <- "eof"
		case errors.Is(err, os.ErrClosed):
			r.done <- pipeDrainCutoff
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

func (w *Worker) finishPipe(attempt *execution.Attempt, readers []*pipeReader, stdout *os.File, lineDone <-chan struct{}, stopReaders context.CancelFunc, meta Meta) (int, error) {
	if err := observeChildExit(w.cmd.Process.Pid); err != nil {
		// Unblock any control awaiting completion while we still own the group.
		w.abortPipe()
		return -1, err
	}
	if w.pipe.input != nil {
		w.pipe.input.stopWriting()
	}
	reasons := drainPipe(readers, stdout, lineDone, stopReaders, pipeDrainLimits{silence: ptyDrainTimeout, total: pipeDrainTotal})
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
	finishPipeCaptures(readers, reasons, &result)
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

// finishPipeCaptures is the final save of every stream when a pipe job ends.
// It has its own budget, separate from the attachment flush.
func finishPipeCaptures(readers []*pipeReader, reasons []string, result *execution.Result) {
	finishPipeCapture(readers, reasons, result, pipeCaptureFlushBudget)
}

func finishPipeCapture(readers []*pipeReader, reasons []string, result *execution.Result, budget time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for index, reader := range readers {
		stream, err := reader.capture.Finish(ctx, reasons[index])
		if err != nil {
			abortPipeCaptures(readers)
			result.Capture, result.Streams = execution.CaptureNotRecorded, nil
			return
		}
		result.Streams = append(result.Streams, stream)
		if captureStatus(stream) != captureComplete {
			result.Capture = captureIncomplete
		}
	}
}

const pipeDrainCutoff = "drain_timeout"

// pipeCaptureFlushBudget bounds the final capture commit after the drain. A
// busy pipe job exits with up to the 1 MiB capture queue still to write, and
// on a loaded disk (load average 35 on the PC, #318) that commit took longer
// than the attachment flush budget: the worker then dropped the tail, recorded
// no capture, and left readers on a stream that never reached EOF. A variable
// so tests can shorten it.
var pipeCaptureFlushBudget = 30 * time.Second

// pipeDrainTotal bounds the whole drain, so a descendant that keeps writing
// cannot hold the worker open. It is longer than the silence window because
// the capture commits to disk at its own pace, and a loaded disk must not cost
// output the leader wrote before it exited.
const pipeDrainTotal = 2 * time.Second

type pipeDrainLimits struct {
	// silence is how long a pipe may stay quiet after the leader exits before a
	// descendant holding it open is cut off. Time spent handing bytes already
	// read to the capture does not count as quiet.
	silence time.Duration
	// total bounds the entire drain.
	total time.Duration
}

// drainPipe waits for every descriptor at once, so each one is cut off by its
// own silence window and a busy stream cannot keep a silent one open.
func drainPipe(readers []*pipeReader, stdout *os.File, lineDone <-chan struct{}, stopReaders context.CancelFunc, limits pipeDrainLimits) []string {
	start := time.Now()
	hard, cancel := context.WithTimeout(context.Background(), limits.total)
	defer cancel()
	var waiting sync.WaitGroup
	reasons := make([]string, len(readers))
	for index, reader := range readers {
		waiting.Go(func() {
			reasons[index] = pipeDrainResult(hard, reader, start, limits.silence)
			if reasons[index] != pipeDrainCutoff {
				return
			}
			// Cancel the reader's own descriptor, and also the bytes it may be
			// holding when the total bound is what expired.
			_ = reader.file.Close()
			if hard.Err() != nil {
				stopReaders()
			}
		})
	}
	if lineDone != nil {
		waiting.Go(func() { drainLineStdout(hard, stdout, lineDone, stopReaders, limits.silence) })
	}
	waiting.Wait()
	return reasons
}

// drainLineStdout gives the line reader the silence window from the start of
// the drain, concurrently with the other streams.
func drainLineStdout(hard context.Context, stdout *os.File, lineDone <-chan struct{}, stopReaders context.CancelFunc, silence time.Duration) {
	window := time.NewTimer(silence)
	defer window.Stop()
	select {
	case <-lineDone:
		return
	case <-window.C:
	case <-hard.Done():
	}
	_ = stdout.Close()
	if hard.Err() != nil {
		stopReaders()
	}
}

func pipeDrainResult(hard context.Context, reader *pipeReader, start time.Time, silence time.Duration) string {
	for {
		wait := silence - reader.silentFor(start)
		if wait <= 0 {
			return finishedOrCut(reader.done)
		}
		timer := time.NewTimer(wait)
		select {
		case reason := <-reader.done:
			timer.Stop()
			return reason
		case <-hard.Done():
			timer.Stop()
			return finishedOrCut(reader.done)
		case <-timer.C:
		}
	}
}

// finishedOrCut prefers a reason the reader already reported over a cutoff.
func finishedOrCut(done <-chan string) string {
	select {
	case reason := <-done:
		return reason
	default:
		return pipeDrainCutoff
	}
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

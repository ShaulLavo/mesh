package worker

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/charmbracelet/x/xpty"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

// ringSize is how much recent output a session keeps for replay. Past this,
// a reattaching client gets a gap instead of a seamless continuation.
const ringSize = 4 << 20

const (
	// Bound both emulator allocation and the rendered snapshot a peer can ask
	// us to create. The cell limit permits conventional large terminals while
	// keeping a single resize from consuming unbounded memory.
	maxTerminalDimension = 2048
	maxTerminalCells     = 256 << 10
	maxSnapshotPayload   = protocol.MaxPayload - len(protocol.SessionID{})

	// A full PTY read is 32 KiB, so the byte limit normally wins after 128
	// frames. The frame limit also bounds queues made of tiny writes. Control
	// frames use reserved slots and never compete with terminal data.
	outboundQueueFrameLimit = 256
	outboundControlReserve  = 8
	outboundDataFrameSize   = 32 << 10
	// A full replay consumes ringSize. One extra PTY frame lets live output join
	// that replay before the first queued frame finishes writing.
	outboundQueueByteLimit = ringSize + outboundDataFrameSize

	// Only the attachment writer waits on the socket, so this deadline cannot
	// delay PTY reads.
	attachmentWriteTimeout = 5 * time.Second
	// A per-frame deadline alone lets many slow writes extend shutdown for
	// minutes. Bound the complete flush too.
	finalFlushTimeout = 5 * time.Second
	// A command can exit before Spawn's readiness probe becomes the real attach.
	// Keep the completed worker available long enough for that second dial.
	firstAttachTimeout = 5 * time.Second

	// After the session leader exits, the pump gets a short window to consume
	// bytes already buffered by the PTY. A descendant that inherited the slave
	// cannot keep the worker alive beyond this window.
	ptyDrainTimeout = 250 * time.Millisecond
)

func validateTerminalSize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("dimensions must be positive, got %dx%d", cols, rows)
	}
	if cols > maxTerminalDimension || rows > maxTerminalDimension {
		return fmt.Errorf("dimensions %dx%d exceed the per-axis limit of %d", cols, rows, maxTerminalDimension)
	}
	if cols > maxTerminalCells/rows {
		return fmt.Errorf("dimensions %dx%d exceed the cell limit of %d", cols, rows, maxTerminalCells)
	}
	return nil
}

// Config describes the session a worker should host.
type Config struct {
	ID                 string
	Dir                string
	Command            []string
	Cwd                string
	Env                []string
	Cols               int
	Rows               int
	AwaitInitialAttach bool
}

// Worker owns one PTY and serves clients over a Unix socket.
type Worker struct {
	cfg    Config
	sid    protocol.SessionID
	pty    xpty.Pty
	cmd    *exec.Cmd
	ring   *session.Ring
	screen terminalstate.Screen

	// mu orders ring delivery, attachment ownership, and shutdown. A client
	// attaching mid-write cannot miss bytes, and finish cannot race a new writer
	// or terminal output queued after session.exit.
	mu          sync.Mutex
	client      *attachment
	attachments map[*attachment]struct{}
	finished    bool
	pumpStopped bool
	writers     sync.WaitGroup
	attachOnce  sync.Once
	attached    chan struct{}

	exitOnce sync.Once
	exited   chan struct{}
	pumpDone chan struct{}
	exitCode int
}

type attachment struct {
	conn  net.Conn
	w     *protocol.Writer
	sid   protocol.SessionID
	queue chan outboundFrame
	done  chan struct{}

	queueMu       sync.Mutex
	payloadFrames int
	payloadBytes  int
	closed        bool
	closeOnce     sync.Once
}

type outboundFrame struct {
	kind       protocol.Kind
	seq        uint64
	payload    []byte
	control    protocol.Control
	closeAfter bool
}

func newAttachment(conn net.Conn, sid protocol.SessionID) *attachment {
	return &attachment{
		conn:  conn,
		w:     protocol.NewWriter(conn),
		sid:   sid,
		queue: make(chan outboundFrame, outboundQueueFrameLimit+outboundControlReserve),
		done:  make(chan struct{}),
	}
}

// startLocked registers the writer before it starts. w.mu must be held so
// finish can prevent new writers before waiting for the existing set.
func (a *attachment) startLocked(w *Worker) {
	if w.attachments == nil {
		w.attachments = make(map[*attachment]struct{})
	}
	w.attachments[a] = struct{}{}
	w.writers.Add(1)
	go a.writeLoop(w)
}

// enqueueData takes its own copy because the PTY pump reuses its read buffer.
// It never waits for the socket or for queue capacity.
func (a *attachment) enqueueData(seq uint64, payload []byte) bool {
	return a.enqueuePayload(protocol.KindData, seq, payload, outboundDataFrameSize, false)
}

func (a *attachment) enqueuePayload(kind protocol.Kind, seq uint64, payload []byte, maxFrameSize int, allowEmpty bool) bool {
	if len(payload) == 0 && !allowEmpty {
		return true
	}

	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.closed || len(payload) > maxFrameSize || a.payloadFrames >= outboundQueueFrameLimit || len(payload) > outboundQueueByteLimit-a.payloadBytes {
		return false
	}

	f := outboundFrame{
		kind:    kind,
		seq:     seq,
		payload: bytes.Clone(payload),
	}
	select {
	case a.queue <- f:
		a.payloadFrames++
		a.payloadBytes += len(payload)
		return true
	default:
		return false
	}
}

func (a *attachment) enqueueSnapshot(payload []byte) bool {
	// A snapshot is one frame so a client commits its resume sequence only
	// after Reader has received the complete repaint.
	return a.enqueuePayload(protocol.KindSnapshot, 0, payload, maxSnapshotPayload, true)
}

func (a *attachment) enqueueDataChunks(seq uint64, payload []byte) bool {
	for len(payload) > 0 {
		n := min(len(payload), outboundDataFrameSize)
		if !a.enqueueData(seq, payload[:n]) {
			return false
		}
		seq += uint64(n)
		payload = payload[n:]
	}
	return true
}

// enqueueControl uses capacity reserved from data frames. Controls retain FIFO
// order with already accepted output, including when the data queue is full.
func (a *attachment) enqueueControl(msg protocol.Control, closeAfter bool) bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.closed {
		return false
	}
	select {
	case a.queue <- outboundFrame{
		kind:       protocol.KindControl,
		control:    msg,
		closeAfter: closeAfter,
	}:
		return true
	default:
		return false
	}
}

func (a *attachment) writeLoop(w *Worker) {
	defer func() {
		a.close()
		w.mu.Lock()
		delete(w.attachments, a)
		if w.client == a {
			w.client = nil
		}
		w.mu.Unlock()
		w.writers.Done()
	}()

	for {
		select {
		case f := <-a.queue:
			err := a.write(f)
			a.releasePayload(f)
			if err != nil || f.closeAfter {
				return
			}
		case <-a.done:
			return
		}
	}
}

func (a *attachment) releasePayload(f outboundFrame) {
	if f.kind != protocol.KindData && f.kind != protocol.KindSnapshot {
		return
	}
	a.queueMu.Lock()
	a.payloadFrames--
	a.payloadBytes -= len(f.payload)
	a.queueMu.Unlock()
}

func (a *attachment) write(f outboundFrame) error {
	_ = a.conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	defer a.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // connection may already be closed

	switch f.kind {
	case protocol.KindControl:
		return a.w.WriteControlMsg(f.control)
	case protocol.KindData:
		return a.w.WriteData(a.sid, f.seq, f.payload)
	case protocol.KindSnapshot:
		return a.w.WriteSnapshot(a.sid, f.payload)
	default:
		return fmt.Errorf("worker: unsupported outbound frame kind %d", f.kind)
	}
}

func (a *attachment) close() {
	a.closeOnce.Do(func() {
		a.queueMu.Lock()
		a.closed = true
		close(a.done)
		a.queueMu.Unlock()
		_ = a.conn.Close()
	})
}

// Run hosts the session until its process exits. It returns the process exit
// code.
func Run(cfg Config) (int, error) {
	if len(cfg.Command) == 0 {
		return 0, errors.New("worker: no command")
	}
	sid, err := protocol.NewSessionID(cfg.ID)
	if err != nil {
		return 0, err
	}
	if cfg.Cols <= 0 {
		cfg.Cols = 80
	}
	if cfg.Rows <= 0 {
		cfg.Rows = 24
	}
	if err := validateTerminalSize(cfg.Cols, cfg.Rows); err != nil {
		return 0, fmt.Errorf("worker: invalid terminal size: %w", err)
	}

	pty, err := xpty.NewPty(cfg.Cols, cfg.Rows)
	if err != nil {
		return 0, fmt.Errorf("worker: open pty: %w", err)
	}
	defer pty.Close() //nolint:errcheck // best effort on shutdown

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.Cwd
	cmd.Env = cfg.Env
	if err := pty.Start(cmd); err != nil {
		return 0, fmt.Errorf("worker: start %s: %w", cfg.Command[0], err)
	}
	// xpty retains the parent's slave descriptor after Start. Closing that copy
	// lets the master report EOF when the child closes its descriptors.
	if p, ok := pty.(interface{ Slave() *os.File }); ok {
		_ = p.Slave().Close()
	}

	w := &Worker{
		cfg:      cfg,
		sid:      sid,
		pty:      pty,
		cmd:      cmd,
		ring:     session.NewRing(ringSize),
		screen:   terminalstate.NewScreen(cfg.Cols, cfg.Rows),
		exited:   make(chan struct{}),
		pumpDone: make(chan struct{}),
		attached: make(chan struct{}),
	}

	meta := Meta{
		ID:        cfg.ID,
		PID:       cmd.Process.Pid,
		Command:   cfg.Command,
		Cwd:       cfg.Cwd,
		State:     StateRunning,
		CreatedAt: time.Now(),
		BootID:    BootID(),
	}
	if err := WriteMeta(cfg.Dir, meta); err != nil {
		return 0, err
	}

	sockPath := paths.Socket(cfg.Dir)
	_ = os.Remove(sockPath) // a stale socket from a dead worker is not a conflict
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return 0, fmt.Errorf("worker: listen on %s: %w", sockPath, err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()
	if err := os.Remove(paths.Launching(cfg.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("worker: publish session %s: %w", cfg.ID, err)
	}

	go w.acceptLoop(ln)
	go w.pump()

	state, waitErr := cmd.Process.Wait()
	code := 0
	if state != nil {
		code = state.ExitCode()
	} else if waitErr != nil {
		code = -1
	}
	w.drainPTY()
	if cfg.AwaitInitialAttach {
		w.waitForFirstAttachment()
	}
	w.finish(code)

	now := time.Now()
	meta.State = StateExited
	meta.ExitCode = &code
	meta.ExitedAt = &now
	if err := WriteMeta(cfg.Dir, meta); err != nil {
		log.Printf("worker: record exit: %v", err)
	}
	return code, nil
}

func (w *Worker) waitForFirstAttachment() {
	timer := time.NewTimer(firstAttachTimeout)
	defer timer.Stop()
	select {
	case <-w.attached:
	case <-timer.C:
	}
}

func (w *Worker) drainPTY() {
	timer := time.NewTimer(ptyDrainTimeout)
	defer timer.Stop()
	select {
	case <-w.pumpDone:
		return
	case <-timer.C:
		// This lock orders the cutoff against a chunk that has returned from
		// Read. Either pump commits the chunk first, or it sees pumpStopped and
		// discards it. No data can enter the queue behind session.exit.
		w.mu.Lock()
		w.pumpStopped = true
		w.mu.Unlock()
		_ = w.pty.Close()
	}
}

// pump moves PTY output into the ring and out to the attached client.
func (w *Worker) pump() {
	defer close(w.pumpDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := w.pty.Read(buf)
		if n > 0 {
			w.mu.Lock()
			if w.pumpStopped {
				w.mu.Unlock()
				return
			}
			seq := w.ring.Head()
			_, _ = w.screen.Write(buf[:n])
			_, _ = w.ring.Write(buf[:n])
			if c := w.client; c != nil {
				if !c.enqueueData(seq, buf[:n]) {
					w.dropLocked(c, "")
				}
			}
			w.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (w *Worker) resize(cols, rows int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resizeLocked(cols, rows)
}

// resizeLocked keeps the PTY and rendered screen at the same dimensions.
// w.mu must be held so resize stays ordered with PTY output.
func (w *Worker) resizeLocked(cols, rows int) error {
	if err := w.pty.Resize(cols, rows); err != nil {
		return fmt.Errorf("resize PTY: %w", err)
	}
	w.screen.Resize(cols, rows)
	return nil
}

// finish records the exit code once and tells the attached client.
func (w *Worker) finish(code int) {
	w.exitOnce.Do(func() {
		w.mu.Lock()
		w.finished = true
		w.exitCode = code
		close(w.exited)
		c := w.client
		if c != nil {
			if !c.enqueueControl(protocol.Control{
				Type:      protocol.TypeExit,
				SessionID: w.cfg.ID,
				ExitCode:  &code,
				Reason:    protocol.ReasonExited,
			}, true) {
				w.dropLocked(c, "")
			}
		}
		w.mu.Unlock()
		w.waitForWriters()
	})
}

func (w *Worker) waitForWriters() {
	done := make(chan struct{})
	go func() {
		w.writers.Wait()
		close(done)
	}()

	timer := time.NewTimer(finalFlushTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}

	w.mu.Lock()
	attachments := make([]*attachment, 0, len(w.attachments))
	for a := range w.attachments {
		attachments = append(attachments, a)
	}
	w.mu.Unlock()
	for _, a := range attachments {
		a.close()
	}
	<-done
}

func (w *Worker) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go w.serve(conn)
	}
}

// dropLocked disowns c if it is still the current client. w.mu must be held.
func (w *Worker) dropLocked(c *attachment, reason string) {
	if w.client != c {
		return
	}
	w.client = nil
	if reason != "" {
		if c.enqueueControl(protocol.Control{
			Type:      protocol.TypeDetach,
			SessionID: w.cfg.ID,
			Reason:    reason,
		}, true) {
			// Stop accepting input from the old owner while its writer flushes
			// the detach notice and closes the connection.
			_ = c.conn.SetReadDeadline(time.Now())
			return
		}
	}
	c.close()
}

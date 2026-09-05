package worker

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

// serve handles one client connection: attach, replay, then relay input until
// either side goes away. Disconnecting a client never affects the process.
func (w *Worker) serve(conn net.Conn) {
	writerOwnsConn := false
	defer func() {
		if !writerOwnsConn {
			_ = conn.Close()
		}
	}()

	r := protocol.NewReader(conn)

	first, err := r.ReadFrame()
	if err != nil {
		return
	}
	if first.Kind != protocol.KindControl {
		return
	}
	msg, err := protocol.DecodeControl(first.Payload)
	if err == nil && msg.Type == protocol.TypeAgentBegin {
		w.serveAgent(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeAgentEvent {
		w.writeAgentEvent(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeShellUpdate {
		w.writeShellUpdate(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeRecoveryCommand {
		w.writeRecoveryCommand(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeLogs {
		w.writeLogs(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeInspect {
		w.writeInspection(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeContainment {
		w.writeContainment(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeNest {
		w.serveNesting(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeKill {
		w.killAndAcknowledge(conn, msg)
		return
	}
	if err == nil && msg.Type == protocol.TypeSignal {
		// Signals are a one-shot connection: delivering one must never disturb
		// whoever is currently attached.
		w.signal(msg.Signal)
		return
	}
	if err != nil || (msg.Type != protocol.TypeAttach && msg.Type != protocol.TypeAttachDetached) {
		_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type:      protocol.TypeError,
			SessionID: w.cfg.ID,
			Message:   "expected " + protocol.TypeAttach,
		})
		return
	}
	containingSessions, err := w.validateAttachmentContainment(msg.ContainingSessions)
	if err != nil {
		w.writeAttachError(conn, fmt.Sprintf("invalid terminal containment: %v", err))
		return
	}
	if msg.Cols != 0 || msg.Rows != 0 {
		if err := validateTerminalSize(msg.Cols, msg.Rows); err != nil {
			w.writeAttachError(conn, fmt.Sprintf("invalid terminal size: %v", err))
			return
		}
	}
	c := newAttachment(conn, w.sid)
	c.containingSessions = containingSessions
	c.nestingSupported = msg.NestingSupported

	// Attach under the same lock the PTY pump uses, so the replay we compute
	// and the live stream that follows it join up exactly.
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return
	}
	if msg.Type == protocol.TypeAttachDetached && w.client != nil {
		w.mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
		_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type: protocol.TypeError, RequestID: msg.RequestID, SessionID: w.cfg.ID,
			Reason: protocol.ReasonAttached, Message: "session is already attached",
		})
		return
	}
	if len(w.nesting) > 0 && !msg.NestingSupported {
		w.mu.Unlock()
		w.writeAttachError(conn, "nested sessions require an attachment that supports nested detach keys")
		return
	}
	if err := w.validateNestingContainmentLocked(containingSessions); err != nil {
		w.mu.Unlock()
		w.writeAttachError(conn, fmt.Sprintf("invalid terminal containment: %v", err))
		return
	}
	if msg.Cols > 0 && msg.Rows > 0 {
		if err := w.resizeLocked(msg.Cols, msg.Rows); err != nil {
			w.mu.Unlock()
			w.writeAttachError(conn, fmt.Sprintf("resize terminal: %v", err))
			return
		}
	}
	// A client that knows where it left off resumes exactly. A fresh attach or
	// an expired replay position gets a rendered screen followed by live output
	// at the current head.
	var (
		want       uint64
		replay     []byte
		snapshot   []byte
		isSnapshot bool
	)
	if msg.LastSeq != nil {
		want = *msg.LastSeq
		var ok bool
		replay, _, ok = w.ring.Since(want)
		if !ok {
			want = w.ring.Head()
			isSnapshot = true
		}
	} else {
		want = w.ring.Head()
		isSnapshot = true
	}
	if isSnapshot {
		capture := w.screen.Snapshot()
		if !capture.Restorable {
			w.mu.Unlock()
			w.writeAttachError(conn, "terminal state is between escape-sequence boundaries; retry attachment")
			return
		}
		snapshot = capture.Bytes
	}
	if len(snapshot) > maxSnapshotPayload {
		w.mu.Unlock()
		w.writeAttachError(conn, fmt.Sprintf("terminal snapshot is %d bytes; maximum is %d", len(snapshot), maxSnapshotPayload))
		return
	}

	if !c.enqueueControl(protocol.Control{
		Type:             protocol.TypeAttached,
		SessionID:        w.cfg.ID,
		Seq:              want,
		Snapshot:         isSnapshot,
		Nested:           w.nestedLocked(),
		NestingSupported: true,
	}, false) || (isSnapshot && !c.enqueueSnapshot(snapshot)) || !c.enqueueDataChunks(want, replay) {
		w.mu.Unlock()
		w.writeAttachError(conn, "unable to queue terminal state; retry attachment")
		return
	}
	if prev := w.client; prev != nil {
		w.dropLocked(prev, protocol.ReasonStolen)
	}
	w.client = c
	writerOwnsConn = true
	c.startLocked(w)
	w.attachOnce.Do(func() { close(w.attached) })
	w.mu.Unlock()
	w.recordAttachment(true)

	defer func() {
		w.mu.Lock()
		w.dropLocked(c, "")
		w.mu.Unlock()
	}()

	// If the process already exited, tell this client immediately rather than
	// leaving it attached to a corpse.
	select {
	case <-w.exited:
		code := w.exitCode
		if c.enqueueControl(protocol.Control{
			Type:      protocol.TypeExit,
			SessionID: w.cfg.ID,
			ExitCode:  &code,
			Reason:    protocol.ReasonExited,
		}, true) {
			<-c.done
		}
		return
	default:
	}

	for {
		f, err := r.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}
		switch f.Kind {
		case protocol.KindInput:
			if !w.enqueueInput(c, f.Payload) {
				return
			}
		case protocol.KindControl:
			msg, err := protocol.DecodeControl(f.Payload)
			if err != nil {
				continue
			}
			switch msg.Type {
			case protocol.TypeResize:
				err := validateTerminalSize(msg.Cols, msg.Rows)
				if err == nil {
					err = w.resize(msg.Cols, msg.Rows)
				}
				if err != nil {
					_ = c.enqueueControl(protocol.Control{
						Type:      protocol.TypeError,
						SessionID: w.cfg.ID,
						Message:   fmt.Sprintf("resize terminal: %v", err),
					}, false)
				}
			case protocol.TypeSignal:
				w.signal(msg.Signal)
			case protocol.TypeKill:
				w.kill()
				return
			case protocol.TypeDetach:
				return
			}
		}
	}
}

func (w *Worker) killAndAcknowledge(conn net.Conn, request protocol.Control) {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return
	}
	w.killResponders.Add(1)
	w.mu.Unlock()
	defer w.killResponders.Done()
	w.kill()
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
		Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: w.cfg.ID,
	})
}

func (w *Worker) writeLogs(conn net.Conn, request protocol.Control) {
	response := protocol.Control{
		Type:      protocol.TypeLogged,
		RequestID: request.RequestID,
		SessionID: w.cfg.ID,
	}
	if request.Tail <= 0 || request.Tail > protocol.MaxLogTail {
		response.Type = protocol.TypeError
		response.Message = fmt.Sprintf("log tail must be between 1 and %d bytes", protocol.MaxLogTail)
	} else {
		response.Output = w.ring.Last(request.Tail)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	defer conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // one-shot connection closes next
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}

func (w *Worker) writeAttachError(conn net.Conn, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	defer conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // connection may be closed by the peer
	_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeError,
		SessionID: w.cfg.ID,
		Message:   message,
	})
}

// signal delivers sig to the session's process group. Signals travel out of
// band precisely so that keys like ctrl+\ stay free for Mesh itself.
func (w *Worker) signal(name string) {
	sig, ok := signals[name]
	if !ok || w.cmd.Process == nil {
		return
	}
	// Once the child has been reaped its PID belongs to the kernel again, and
	// the negated form would target whatever process group later claims it.
	w.mu.Lock()
	reaped := w.reaped
	w.mu.Unlock()
	if reaped {
		return
	}
	// The child was started with Setsid and Setctty, so it leads its own
	// process group and negating the PID targets the whole job, matching what
	// the terminal driver does.
	if err := syscall.Kill(-w.cmd.Process.Pid, sig); err != nil {
		_ = w.cmd.Process.Signal(sig)
	}
}

// killGrace is how long a session gets to shut down after its terminal is
// taken away, before it is killed outright.
const killGrace = 5 * time.Second

// kill ends the session the way closing a terminal window does: hang up the
// process group, then insist. SIGTERM is deliberately not used, because an
// interactive shell ignores it and `mesh kill` must always mean the session is
// gone.
func (w *Worker) kill() {
	w.signal("hup")
	select {
	case <-w.exited:
		return
	case <-time.After(killGrace):
	}
	// Closing the PTY master drops the slave side too, so anything that
	// survived the hangup loses its terminal as well.
	_ = w.pty.Close()
	w.signal("kill")
	<-w.exited
}

var signals = map[string]syscall.Signal{
	"int":  syscall.SIGINT,
	"term": syscall.SIGTERM,
	"quit": syscall.SIGQUIT,
	"hup":  syscall.SIGHUP,
	"kill": syscall.SIGKILL,
	"usr1": syscall.SIGUSR1,
	"usr2": syscall.SIGUSR2,
}

// SupportsSignal reports whether a control request names a supported process
// group signal.
func SupportsSignal(name string) bool {
	_, ok := signals[name]
	return ok
}

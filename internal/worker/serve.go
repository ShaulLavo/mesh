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
	if err == nil && msg.Type == protocol.TypeKill {
		go w.kill()
		return
	}
	if err == nil && msg.Type == protocol.TypeSignal {
		// Signals are a one-shot connection: delivering one must never disturb
		// whoever is currently attached.
		w.signal(msg.Signal)
		return
	}
	if err != nil || msg.Type != protocol.TypeAttach {
		_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type:      protocol.TypeError,
			SessionID: w.cfg.ID,
			Message:   "expected " + protocol.TypeAttach,
		})
		return
	}
	if msg.Cols != 0 || msg.Rows != 0 {
		if err := validateTerminalSize(msg.Cols, msg.Rows); err != nil {
			w.writeAttachError(conn, fmt.Sprintf("invalid terminal size: %v", err))
			return
		}
	}
	c := newAttachment(conn, w.sid)

	// Attach under the same lock the PTY pump uses, so the replay we compute
	// and the live stream that follows it join up exactly.
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
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
		Type:      protocol.TypeAttached,
		SessionID: w.cfg.ID,
		Seq:       want,
		Snapshot:  isSnapshot,
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
			if _, err := w.pty.Write(f.Payload); err != nil {
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
				go w.kill()
			case protocol.TypeDetach:
				return
			}
		}
	}
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
	// The PTY gave the child its own session and process group, so negating
	// the PID targets the whole job, matching what the terminal driver does.
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

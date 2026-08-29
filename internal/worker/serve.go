package worker

import (
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

// serve handles one client connection: attach, replay, then relay input until
// either side goes away. Disconnecting a client never affects the process.
func (w *Worker) serve(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // client is going away regardless

	r := protocol.NewReader(conn)
	c := &attachment{conn: conn, w: protocol.NewWriter(conn)}

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
		_ = c.send(func(fw *protocol.Writer) error {
			return fw.WriteControlMsg(protocol.Control{
				Type:    protocol.TypeError,
				Message: "expected " + protocol.TypeAttach,
			})
		})
		return
	}

	if msg.Cols > 0 && msg.Rows > 0 {
		_ = w.pty.Resize(msg.Cols, msg.Rows)
	}

	// Attach under the same lock the PTY pump uses, so the replay we compute
	// and the live stream that follows it join up exactly.
	w.mu.Lock()
	if prev := w.client; prev != nil {
		w.dropLocked(prev, protocol.ReasonStolen)
	}

	// A client that knows where it left off resumes exactly; one attaching
	// fresh asks for a bounded tail so reattaching does not dump megabytes of
	// scrollback into the terminal.
	var want uint64
	if msg.LastSeq != nil {
		want = *msg.LastSeq
	} else {
		head := w.ring.Head()
		if tail := uint64(max(msg.Tail, 0)); tail < head {
			want = head - tail
		}
	}
	replay, _, ok := w.ring.Since(want)
	if !ok {
		// The client's position has fallen out of the replay window. Send the
		// whole window and say so; a vt-rendered screen snapshot replaces this
		// once terminal state tracking lands.
		want = w.ring.Tail()
		replay, _, _ = w.ring.Since(want)
	}

	err = c.send(func(fw *protocol.Writer) error {
		if e := fw.WriteControlMsg(protocol.Control{
			Type:      protocol.TypeAttached,
			SessionID: w.cfg.ID,
			Seq:       want,
			Snapshot:  !ok,
		}); e != nil {
			return e
		}
		if len(replay) > 0 {
			return fw.WriteData(w.sid, want, replay)
		}
		return nil
	})
	if err != nil {
		w.mu.Unlock()
		return
	}
	w.client = c
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
		_ = c.send(func(fw *protocol.Writer) error {
			return fw.WriteControlMsg(protocol.Control{
				Type:      protocol.TypeExit,
				SessionID: w.cfg.ID,
				ExitCode:  &code,
				Reason:    protocol.ReasonExited,
			})
		})
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
				if msg.Cols > 0 && msg.Rows > 0 {
					_ = w.pty.Resize(msg.Cols, msg.Rows)
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

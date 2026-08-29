package worker

import (
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
)

// ringSize is how much recent output a session keeps for replay. Past this,
// a reattaching client gets a gap instead of a seamless continuation.
const ringSize = 4 << 20

// clientWriteTimeout bounds how long the PTY reader will wait on a stuck
// client before disowning it. Terminal output must never be held hostage by
// one wedged socket.
const clientWriteTimeout = 5 * time.Second

// Config describes the session a worker should host.
type Config struct {
	ID      string
	Dir     string
	Command []string
	Cwd     string
	Env     []string
	Cols    int
	Rows    int
}

// Worker owns one PTY and serves clients over a Unix socket.
type Worker struct {
	cfg  Config
	sid  protocol.SessionID
	pty  xpty.Pty
	cmd  *exec.Cmd
	ring *session.Ring

	// mu guards ring writes together with delivery to the attached client, so
	// that a client attaching mid-write cannot miss bytes or receive them
	// twice.
	mu     sync.Mutex
	client *attachment

	exitOnce sync.Once
	exited   chan struct{}
	exitCode int
}

type attachment struct {
	conn net.Conn
	w    *protocol.Writer
	mu   sync.Mutex // serializes frame writes to conn
}

func (a *attachment) send(fn func(*protocol.Writer) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
	err := fn(a.w)
	_ = a.conn.SetWriteDeadline(time.Time{})
	return err
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

	w := &Worker{
		cfg:    cfg,
		sid:    sid,
		pty:    pty,
		cmd:    cmd,
		ring:   session.NewRing(ringSize),
		exited: make(chan struct{}),
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

	go w.acceptLoop(ln)
	go w.pump()

	state, waitErr := cmd.Process.Wait()
	code := 0
	if state != nil {
		code = state.ExitCode()
	} else if waitErr != nil {
		code = -1
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

// pump moves PTY output into the ring and out to the attached client.
func (w *Worker) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := w.pty.Read(buf)
		if n > 0 {
			w.mu.Lock()
			seq := w.ring.Head()
			_, _ = w.ring.Write(buf[:n])
			if c := w.client; c != nil {
				chunk := buf[:n]
				if sendErr := c.send(func(fw *protocol.Writer) error {
					return fw.WriteData(w.sid, seq, chunk)
				}); sendErr != nil {
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

// finish records the exit code once and tells the attached client.
func (w *Worker) finish(code int) {
	w.exitOnce.Do(func() {
		w.exitCode = code
		close(w.exited)

		w.mu.Lock()
		c := w.client
		w.mu.Unlock()
		if c != nil {
			_ = c.send(func(fw *protocol.Writer) error {
				return fw.WriteControlMsg(protocol.Control{
					Type:      protocol.TypeExit,
					SessionID: w.cfg.ID,
					ExitCode:  &code,
					Reason:    protocol.ReasonExited,
				})
			})
			// Give the client a moment to flush the final screen before its
			// socket disappears.
			time.Sleep(50 * time.Millisecond)
			_ = c.conn.Close()
		}
	})
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
		_ = c.send(func(fw *protocol.Writer) error {
			return fw.WriteControlMsg(protocol.Control{
				Type:      protocol.TypeDetach,
				SessionID: w.cfg.ID,
				Reason:    reason,
			})
		})
	}
	_ = c.conn.Close()
}

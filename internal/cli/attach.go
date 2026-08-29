// Package cli implements the client side of a Mesh session.
package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/protocol"
)

// DefaultDetachKey is ctrl+] (ASCII GS), the same escape telnet has used for
// decades. It is not a terminal control character, so intercepting it costs
// the session no signal; the one real casualty is vim's tag jump, which is why
// the key is configurable and --raw turns interception off entirely.
const DefaultDetachKey = 0x1d

// defaultTail is how much recent output a fresh attach asks to be repainted
// with when it has no sequence number to resume from.
const defaultTail = 64 << 10

// AttachOptions configures a client attachment.
type AttachOptions struct {
	SocketPath string
	SessionID  string
	// LastSeq resumes at an exact offset. Nil asks for a bounded tail instead.
	LastSeq *uint64
	// DetachKey is the byte that detaches. Zero means DefaultDetachKey.
	DetachKey byte
	// Raw disables the detach key so the session receives every byte verbatim.
	Raw bool
	In  *os.File
	Out *os.File
}

// AttachResult reports how an attachment ended.
type AttachResult struct {
	Detached bool
	Exited   bool
	ExitCode int
	// LastSeq is the offset to resume from next time.
	LastSeq uint64
}

// Attach connects to a session worker and relays the local terminal to it
// until the client detaches or the remote process exits. Returning never
// implies anything about whether the remote process is still alive.
func Attach(opts AttachOptions) (AttachResult, error) {
	var res AttachResult

	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.DetachKey == 0 {
		opts.DetachKey = DefaultDetachKey
	}

	sid, err := protocol.NewSessionID(opts.SessionID)
	if err != nil {
		return res, err
	}

	conn, err := net.Dial("unix", opts.SocketPath)
	if err != nil {
		return res, fmt.Errorf("attach %s: %w", opts.SessionID, err)
	}
	defer conn.Close() //nolint:errcheck // closing on the way out

	cols, rows := 80, 24
	if w, h, err := term.GetSize(opts.Out.Fd()); err == nil && w > 0 {
		cols, rows = w, h
	}

	var writeMu sync.Mutex
	fw := protocol.NewWriter(conn)
	send := func(fn func(*protocol.Writer) error) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return fn(fw)
	}

	attach := protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: opts.SessionID,
		LastSeq:   opts.LastSeq,
		Cols:      cols,
		Rows:      rows,
	}
	if opts.LastSeq == nil {
		attach.Tail = defaultTail
	}
	if err := send(func(w *protocol.Writer) error { return w.WriteControlMsg(attach) }); err != nil {
		return res, fmt.Errorf("attach %s: %w", opts.SessionID, err)
	}

	if term.IsTerminal(opts.In.Fd()) {
		restore, err := makeRaw(opts.In)
		if err != nil {
			return res, err
		}
		defer restore()
	}

	// Resizes are pushed as they happen; the worker owns the PTY dimensions.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			w, h, err := term.GetSize(opts.Out.Fd())
			if err != nil || w <= 0 {
				continue
			}
			_ = send(func(fw *protocol.Writer) error {
				return fw.WriteControlMsg(protocol.Control{
					Type:      protocol.TypeResize,
					SessionID: opts.SessionID,
					Cols:      w,
					Rows:      h,
				})
			})
		}
	}()

	detached := make(chan struct{})
	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			_ = send(func(fw *protocol.Writer) error {
				return fw.WriteControlMsg(protocol.Control{
					Type:      protocol.TypeDetach,
					SessionID: opts.SessionID,
					Reason:    protocol.ReasonClient,
				})
			})
			close(detached)
			// Unblocking the stdin read is not possible portably, so drop the
			// connection and let the read loop die with it.
			_ = conn.Close()
		})
	}

	go relayInput(opts, sid, send, detach)

	// Output loop owns the return value: it is the only thing that knows
	// whether the process exited or we merely walked away.
	r := protocol.NewReader(conn)
	for {
		f, err := r.ReadFrame()
		if err != nil {
			select {
			case <-detached:
				res.Detached = true
				return res, nil
			default:
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return res, nil
			}
			return res, fmt.Errorf("session %s: %w", opts.SessionID, err)
		}
		switch f.Kind {
		case protocol.KindData:
			if _, err := opts.Out.Write(f.Payload); err != nil {
				return res, err
			}
			res.LastSeq = f.Seq + uint64(len(f.Payload))
		case protocol.KindControl:
			msg, err := protocol.DecodeControl(f.Payload)
			if err != nil {
				continue
			}
			switch msg.Type {
			case protocol.TypeAttached:
				res.LastSeq = msg.Seq
			case protocol.TypeExit:
				res.Exited = true
				if msg.ExitCode != nil {
					res.ExitCode = *msg.ExitCode
				}
				return res, nil
			case protocol.TypeDetach:
				res.Detached = true
				return res, nil
			case protocol.TypeError:
				return res, fmt.Errorf("session %s: %s", opts.SessionID, msg.Message)
			}
		}
	}
}

// relayInput forwards keystrokes, watching for the detach key.
func relayInput(opts AttachOptions, sid protocol.SessionID, send func(func(*protocol.Writer) error) error, detach func()) {
	buf := make([]byte, 4096)
	for {
		n, err := opts.In.Read(buf)
		if n > 0 {
			b := buf[:n]
			if !opts.Raw {
				if i := indexByte(b, opts.DetachKey); i >= 0 {
					if i > 0 {
						_ = send(func(fw *protocol.Writer) error { return fw.WriteInput(sid, b[:i]) })
					}
					detach()
					return
				}
			}
			if err := send(func(fw *protocol.Writer) error { return fw.WriteInput(sid, b) }); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func makeRaw(f *os.File) (func(), error) {
	state, err := term.MakeRaw(f.Fd())
	if err != nil {
		return nil, fmt.Errorf("put terminal in raw mode: %w", err)
	}
	return func() { _ = term.Restore(f.Fd(), state) }, nil
}

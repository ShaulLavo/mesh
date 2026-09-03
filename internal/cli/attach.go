// Package cli implements the client side of a Mesh session.
package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// DefaultDetachKey is ctrl+] (ASCII GS), the same escape telnet has used for
// decades. It is not a terminal control character, so intercepting it costs
// the session no signal; the one real casualty is vim's tag jump, which is why
// the key is configurable and --raw turns interception off entirely.
const DefaultDetachKey = 0x1d

// detachKeysByDepth gives each nesting level its own key. The outermost client
// reads every keystroke first, so a single key would always detach the outermost
// session and drop the operator past every session in between. With one key per
// level, the outer clients forward a key that is not theirs and the level that
// owns it detaches, leaving the ones above it attached.
//
// ctrl+] then ctrl+^ then ctrl+_ : GS, RS, US, adjacent in ASCII and unused by
// anything a terminal sends.
var detachKeysByDepth = []byte{0x1d, 0x1e, 0x1f}

// DetachKeyForDepth is the key a client at this nesting depth listens for.
// Depths past the table share the last key: at that point the operator has
// nested further than the scheme can separate, and detaching the innermost two
// together is better than a key nothing listens for.
func DetachKeyForDepth(depth int) byte {
	if depth < 0 {
		depth = 0
	}
	if depth >= len(detachKeysByDepth) {
		depth = len(detachKeysByDepth) - 1
	}
	return detachKeysByDepth[depth]
}

// DetachKeyName renders a key the way the operator would type it.
func DetachKeyName(key byte) string {
	switch key {
	case 0x1d:
		return "ctrl+]"
	case 0x1e:
		return "ctrl+^"
	case 0x1f:
		return "ctrl+_"
	default:
		return fmt.Sprintf("0x%02x", key)
	}
}

// SessionDepth is how many Mesh sessions this process is already inside.
func SessionDepth() int {
	depth, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MESH_DEPTH")))
	if err != nil || depth < 0 {
		return 0
	}
	return depth
}

// AttachOptions configures a client attachment.
type AttachOptions struct {
	SocketPath string
	// Conn supplies an already connected local or remote daemon transport.
	// It is closed when Attach returns. SocketPath and Conn are exclusive.
	Conn      transport.Conn
	SessionID string
	// LastSeq resumes at an exact offset. Nil asks for a rendered snapshot.
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
	if opts.LastSeq != nil {
		res.LastSeq = *opts.LastSeq
	}

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

	conn := opts.Conn
	if conn != nil && opts.SocketPath != "" {
		return res, fmt.Errorf("attach %s: both connection and socket path supplied", opts.SessionID)
	}
	if conn == nil {
		stream, err := net.DialTimeout("unix", opts.SocketPath, 2*time.Second)
		if err != nil {
			return res, fmt.Errorf("attach %s: %w", opts.SessionID, err)
		}
		conn, err = transport.NewStreamConn(stream)
		if err != nil {
			_ = stream.Close()
			return res, fmt.Errorf("attach %s: %w", opts.SessionID, err)
		}
	}
	defer conn.Close() //nolint:errcheck // closing on the way out

	cols, rows := 80, 24
	if w, h, err := term.GetSize(opts.Out.Fd()); err == nil && w > 0 {
		cols, rows = w, h
	}

	var writeMu sync.Mutex
	send := func(frame protocol.Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteFrame(frame)
	}

	attach := protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: opts.SessionID,
		LastSeq:   opts.LastSeq,
		Cols:      cols,
		Rows:      rows,
	}
	if err := send(controlFrame(attach)); err != nil {
		return res, presentError("attach "+opts.SessionID, err)
	}

	var altScreen altScreenTracker
	inputIsTerminal := term.IsTerminal(opts.In.Fd())
	if inputIsTerminal {
		restore, err := makeRaw(opts.In)
		if err != nil {
			return res, err
		}
		defer restore()
		// Detaching leaves the remote program running, so it never emits the
		// sequences that would put this terminal back: a full-screen app stays
		// in the alternate buffer with its last frame stranded on screen. ssh
		// has no detach, so its programs always exit and clean up after
		// themselves.
		defer func() {
			if altScreen.Active() {
				_, _ = opts.Out.Write([]byte(leaveAltScreenSequence))
			}
			_, _ = opts.Out.Write([]byte(restoreTerminalState))
		}()
	}

	input := io.Reader(opts.In)
	cancelInput := func() {}
	closeInput := func() error { return nil }
	info, err := opts.In.Stat()
	if err != nil {
		return res, fmt.Errorf("inspect terminal input: %w", err)
	}
	if inputIsTerminal || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
		reader, err := uv.NewCancelReader(opts.In)
		if err != nil {
			return res, fmt.Errorf("make terminal input cancelable: %w", err)
		}
		input = reader
		cancelInput = func() { reader.Cancel() }
		closeInput = reader.Close
	}

	// Resizes are pushed as they happen; the worker owns the PTY dimensions.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	resizeDone := make(chan struct{})
	var relays sync.WaitGroup
	relays.Add(1)
	go func() {
		defer relays.Done()
		relayResizes(resizeDone, winch, func() {
			w, h, err := term.GetSize(opts.Out.Fd())
			if err != nil || w <= 0 {
				return
			}
			frame := controlFrame(protocol.Control{
				Type:      protocol.TypeResize,
				SessionID: opts.SessionID,
				Cols:      w,
				Rows:      h,
			})
			_ = send(frame)
		})
	}()
	defer func() {
		signal.Stop(winch)
		close(resizeDone)
		cancelInput()
		_ = conn.Close()
		relays.Wait()
		_ = closeInput()
	}()

	detached := make(chan struct{})
	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			// Tear down first, tell the host second. send takes the write lock
			// and then the reconnect lock, both of which the read loop holds
			// while it is redialling, so leading with the courtesy frame meant
			// ctrl+] never returned during a link failure — the terminal stayed
			// in raw mode and the only way out was killing mesh elsewhere.
			// The worker treats a dropped connection as a detach anyway, so the
			// frame is a nicety, not the mechanism.
			close(detached)
			notified := make(chan struct{})
			go func() {
				defer close(notified)
				_ = send(controlFrame(protocol.Control{
					Type:      protocol.TypeDetach,
					SessionID: opts.SessionID,
					Reason:    protocol.ReasonClient,
				}))
			}()
			select {
			case <-notified:
			case <-time.After(detachNotifyTimeout):
			}
			_ = conn.Close()
		})
	}

	relays.Add(1)
	go func() {
		defer relays.Done()
		relayInput(opts, input, sid, send, detach)
	}()

	// Output loop owns the return value: it is the only thing that knows
	// whether the process exited or we merely walked away.
	var (
		pendingSnapshot bool
		snapshotSeq     uint64
	)
	for {
		f, err := conn.ReadFrame()
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
			return res, presentError("session "+opts.SessionID, err)
		}
		switch f.Kind {
		case protocol.KindData:
			if pendingSnapshot {
				return res, fmt.Errorf("session %s: received live output before announced snapshot", opts.SessionID)
			}
			altScreen.Observe(f.Payload)
			if _, err := opts.Out.Write(f.Payload); err != nil {
				return res, err
			}
			res.LastSeq = f.Seq + uint64(len(f.Payload))
		case protocol.KindSnapshot:
			if !pendingSnapshot {
				return res, fmt.Errorf("session %s: received an unannounced snapshot", opts.SessionID)
			}
			altScreen.Observe(f.Payload)
			if _, err := opts.Out.Write(f.Payload); err != nil {
				return res, err
			}
			res.LastSeq = snapshotSeq
			pendingSnapshot = false
		case protocol.KindControl:
			msg, err := protocol.DecodeControl(f.Payload)
			if err != nil {
				continue
			}
			switch msg.Type {
			case protocol.TypeAttached:
				if msg.Snapshot {
					pendingSnapshot = true
					snapshotSeq = msg.Seq
				} else {
					res.LastSeq = msg.Seq
				}
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
				return res, daemonResponseError("session "+opts.SessionID, msg.Message)
			}
		}
	}
}

// relayInput forwards keystrokes, watching for the detach key.
func relayInput(opts AttachOptions, input io.Reader, sid protocol.SessionID, send func(protocol.Frame) error, detach func()) {
	buf := make([]byte, 4096)
	for {
		n, err := input.Read(buf)
		if n > 0 {
			b := buf[:n]
			if !opts.Raw {
				if i := indexByte(b, opts.DetachKey); i >= 0 {
					if i > 0 {
						_ = send(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: append([]byte(nil), b[:i]...)})
					}
					detach()
					return
				}
			}
			// A failed write is how the transport reports a dead link: it
			// deliberately does not retry, because a repeated keystroke is
			// worse than a lost one, and the next operation reconnects.
			// Returning here retired the keyboard for the rest of the session
			// while output kept painting, so the session looked alive and
			// ignored every key including the detach key. Drop the keystroke
			// and keep relaying.
			_ = send(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: append([]byte(nil), b...)})
		}
		if err != nil {
			return
		}
	}
}

func controlFrame(message protocol.Control) protocol.Frame {
	payload, _ := message.Encode()
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}
}

func relayResizes(done <-chan struct{}, winch <-chan os.Signal, resize func()) {
	for {
		select {
		case <-done:
			return
		case <-winch:
			resize()
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

// restoreTerminalState returns the local terminal to something usable after a
// detach. Each mode is one a full-screen program routinely turns on and would
// have turned off on its way out.
const restoreTerminalState = "\x1b[?25h" + // show the cursor
	"\x1b[?7h" + // restore autowrap
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l" + // stop mouse reporting
	"\x1b[?2004l" + // stop bracketed paste
	"\x1b[0m" // reset colours and attributes

func makeRaw(f *os.File) (func(), error) {
	state, err := term.MakeRaw(f.Fd())
	if err != nil {
		return nil, fmt.Errorf("put terminal in raw mode: %w", err)
	}
	return func() { _ = term.Restore(f.Fd(), state) }, nil
}

// detachNotifyTimeout bounds the courtesy detach frame. The worker treats a
// dropped connection as a detach, so a link that is already gone must not delay
// the user's exit.
const detachNotifyTimeout = 2 * time.Second

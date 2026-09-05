// Package cli implements the client side of a Mesh session.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

// DefaultDetachKey is ctrl+] (ASCII GS), the same escape telnet has used for
// decades. It is not a terminal control character, so intercepting it costs
// the session no signal; the one real casualty is vim's tag jump, which is why
// the key is configurable and --raw turns interception off entirely.
const DefaultDetachKey = 0x1d

// DefaultLeaveKey is ctrl+^, consumed by the outermost registered attachment.
const DefaultLeaveKey = 0x1e

// ErrSessionAttached means an attachment restricted to detached sessions lost
// the race to another client. Retrying another session cannot steal that client.
var ErrSessionAttached = errors.New("session is already attached")

// ErrSessionUnavailable means a detached candidate disappeared before the
// worker acknowledged the claim.
var ErrSessionUnavailable = errors.New("session ended before attachment")

// ErrAttachDetachedUnsupported identifies workers that cannot make an atomic
// claim. A caller may choose a fresh session, but must not retry a stealing attach.
var ErrAttachDetachedUnsupported = errors.New("worker cannot resume without taking over; start a new session, or explicitly use mesh attach ID locally or mesh ID remotely")

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
	// HostID identifies the destination host. Remote connections must supply it
	// to register their exact identity with the containing local worker.
	HostID string
	// IfDetached claims a session atomically without evicting another client.
	IfDetached bool
	// ContainingSessions is the immediate-to-outer path whose terminal screens
	// will mirror this attachment's output.
	ContainingSessions []protocol.SessionIdentity
	// LastSeq resumes at an exact offset. Nil asks for a rendered snapshot.
	LastSeq *uint64
	// DetachKey is the byte that detaches. Zero selects the default for nesting.
	DetachKey byte
	// DetachKeyExplicit preserves a requested key even when nesting changes.
	DetachKeyExplicit bool
	// LeaveKey detaches the outermost attachment while registered inner clients
	// keep running. Zero means DefaultLeaveKey.
	LeaveKey        byte
	DisableLeaveKey bool
	// Raw disables the detach key so the session receives every byte verbatim.
	Raw bool
	In  *os.File
	Out *os.File
	// Stderr receives client hints without changing the session's byte stream.
	// Nil uses os.Stderr.
	Stderr io.Writer
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
	res := initialAttachResult(opts)
	if opts.Conn != nil {
		defer opts.Conn.Close() //nolint:errcheck // release even if local terminal setup fails
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if _, err := validateAttachOptions(opts); err != nil {
		return res, err
	}
	registration, inside, err := registerAttachmentNesting(opts)
	if err != nil {
		return res, fmt.Errorf("attach %s nesting: %w", opts.SessionID, err)
	}
	if registration != nil {
		defer registration.Close() //nolint:errcheck // lifetime ends with attachment
	}
	keys := newAttachmentKeys(opts, SessionDepth(), inside, registration != nil)
	if inside && registration == nil && !keys.explicit && !opts.Raw {
		_, _ = fmt.Fprintf(opts.Stderr, "  (%s detaches this one, ctrl+] leaves them all)\r\n", DetachKeyName(keys.detachKey))
	}
	terminal, closeTerminal, inputIsTerminal, err := localAttachTerminal(opts.In, opts.Out)
	if err != nil {
		return res, err
	}
	defer closeTerminal()
	return attachWithTerminal(context.Background(), opts, terminal, keys, inputIsTerminal)
}

func initialAttachResult(opts AttachOptions) AttachResult {
	if opts.LastSeq != nil {
		return AttachResult{LastSeq: *opts.LastSeq}
	}
	return AttachResult{}
}

func validateAttachOptions(opts AttachOptions) (protocol.SessionID, error) {
	sid, err := protocol.NewSessionID(opts.SessionID)
	if err != nil {
		return sid, err
	}
	if err := protocol.ValidateContainingSessions(opts.ContainingSessions); err != nil {
		return sid, fmt.Errorf("attach %s containment: %w", opts.SessionID, err)
	}
	if opts.Conn != nil && opts.SocketPath != "" {
		return sid, fmt.Errorf("attach %s: both connection and socket path supplied", opts.SessionID)
	}
	if opts.HostID != "" {
		err = protocol.ValidateSessionIdentity(protocol.SessionIdentity{HostID: opts.HostID, SessionID: opts.SessionID})
	}
	if err != nil || len(opts.ContainingSessions) == 0 {
		return sid, err
	}
	target, err := attachmentTargetIdentity(opts)
	if err != nil {
		// Embedded callers without a destination identity cannot prove a match.
		return sid, nil
	}
	return sid, rejectContainingTarget(target, opts.ContainingSessions)
}

func attachWithTerminal(ctx context.Context, opts AttachOptions, terminal AttachTerminal, keys *attachmentKeys, restoreModes bool) (AttachResult, error) {
	res := initialAttachResult(opts)
	var cancelOnce sync.Once
	cancelInput := func() { cancelOnce.Do(terminal.CancelInput) }
	if terminal.CancelInput != nil {
		defer cancelInput()
	}
	if opts.Conn != nil {
		defer opts.Conn.Close() //nolint:errcheck // attachment owns the connection
	}
	if err := validateAttachTerminal(ctx, terminal); err != nil {
		return res, err
	}
	sid, err := validateAttachOptions(opts)
	if err != nil {
		return res, err
	}
	conn, err := openAttachment(ctx, opts)
	if err != nil {
		return res, err
	}
	defer conn.Close() //nolint:errcheck // attachment owns the connection

	send := conn.WriteFrame
	state := attachmentOutput{opts: opts, terminal: terminal, keys: keys, result: res}
	if restoreModes {
		defer state.restoreTerminal()
	}
	done := make(chan struct{})
	inputRelay := &attachmentInputRelay{terminal: terminal, done: done}
	if reconnecting, ok := conn.(interface{ SetResumeInputBarrier(func() error) }); ok {
		reconnecting.SetResumeInputBarrier(inputRelay.reset)
	}
	stopContext := closeAttachmentOnCancel(ctx, conn)
	defer stopContext()
	request := attachmentRequest(opts, terminal.Size)
	request.NestingSupported = keys.dynamic
	if err := send(controlFrame(request)); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return res, presentError("attach "+opts.SessionID, err)
	}

	detached := make(chan struct{})
	var relays sync.WaitGroup
	defer func() {
		close(done)
		cancelInput()
		_ = conn.Close()
		relays.Wait()
	}()
	relays.Go(func() { relayTerminalResizes(done, terminal.Resizes, opts.SessionID, send) })
	relays.Go(func() {
		relayInput(inputRelay, keys, sid, send, func() { detachAttachment(conn, opts.SessionID, send, detached) })
	})
	return state.read(ctx, conn, detached)
}

func openAttachment(ctx context.Context, opts AttachOptions) (transport.Conn, error) {
	if opts.Conn != nil {
		return opts.Conn, nil
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	stream, err := dialer.DialContext(ctx, "unix", opts.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("attach %s: %w", opts.SessionID, err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("attach %s: %w", opts.SessionID, err)
	}
	return conn, nil
}

func attachmentRequest(opts AttachOptions, size terminal.Size) protocol.Control {
	request := protocol.Control{
		Type: protocol.TypeAttach, SessionID: opts.SessionID, LastSeq: opts.LastSeq,
		Cols: size.Cols, Rows: size.Rows,
		ContainingSessions: protocol.CloneSessionIdentities(opts.ContainingSessions),
	}
	if opts.IfDetached {
		request.Type = protocol.TypeAttachDetached
	}
	return request
}

func closeAttachmentOnCancel(ctx context.Context, conn transport.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = conn.Close()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func relayTerminalResizes(done <-chan struct{}, resizes <-chan terminal.Size, sessionID string, send func(protocol.Frame) error) {
	for {
		size, ok := nextTerminalResize(done, resizes)
		if !ok {
			return
		}
		forwardTerminalResize(size, sessionID, send)
	}
}

func nextTerminalResize(done <-chan struct{}, resizes <-chan terminal.Size) (terminal.Size, bool) {
	select {
	case <-done:
		return terminal.Size{}, false
	case size, ok := <-resizes:
		return size, ok
	}
}

func forwardTerminalResize(size terminal.Size, sessionID string, send func(protocol.Frame) error) {
	if size.Cols <= 0 || size.Rows <= 0 {
		return
	}
	_ = send(controlFrame(protocol.Control{
		Type: protocol.TypeResize, SessionID: sessionID, Cols: size.Cols, Rows: size.Rows,
	}))
}

func detachAttachment(conn transport.Conn, sessionID string, send func(protocol.Frame) error, detached chan<- struct{}) {
	close(detached)
	notified := make(chan struct{})
	go func() {
		defer close(notified)
		_ = send(controlFrame(protocol.Control{Type: protocol.TypeDetach, SessionID: sessionID, Reason: protocol.ReasonClient}))
	}()
	select {
	case <-notified:
	case <-time.After(detachNotifyTimeout):
	}
	_ = conn.Close()
	// Close unblocks the courtesy write even when another writer held its lock.
	<-notified
}

type attachmentOutput struct {
	opts            AttachOptions
	terminal        AttachTerminal
	keys            *attachmentKeys
	result          AttachResult
	altScreen       altScreenTracker
	pendingSnapshot bool
	snapshotSeq     uint64
	acknowledged    bool
}

func (s *attachmentOutput) restoreTerminal() {
	if s.altScreen.Active() {
		_, _ = io.WriteString(s.terminal.Output, leaveAltScreenSequence)
	}
	_, _ = io.WriteString(s.terminal.Output, restoreTerminalState)
}

func (s *attachmentOutput) read(ctx context.Context, conn transport.Conn, detached <-chan struct{}) (AttachResult, error) {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			err = s.readError(ctx, err, detached)
			return s.result, err
		}
		done, err := s.accept(frame)
		if done || err != nil {
			return s.result, err
		}
	}
}

func (s *attachmentOutput) readError(ctx context.Context, err error, detached <-chan struct{}) error {
	select {
	case <-detached:
		s.result.Detached = true
		return nil
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		return presentError("session "+s.opts.SessionID, err)
	}
	if s.opts.IfDetached && !s.acknowledged {
		return fmt.Errorf("session %s: %w", s.opts.SessionID, ErrSessionUnavailable)
	}
	return nil
}

func (s *attachmentOutput) accept(frame protocol.Frame) (bool, error) {
	switch frame.Kind {
	case protocol.KindData:
		return false, s.data(frame)
	case protocol.KindSnapshot:
		return false, s.snapshot(frame)
	case protocol.KindControl:
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return false, nil
		}
		return s.control(message)
	default:
		return false, nil
	}
}

func (s *attachmentOutput) data(frame protocol.Frame) error {
	if s.pendingSnapshot {
		return fmt.Errorf("session %s: received live output before announced snapshot", s.opts.SessionID)
	}
	s.altScreen.Observe(frame.Payload)
	if _, err := s.terminal.Output.Write(frame.Payload); err != nil {
		return err
	}
	s.result.LastSeq = frame.Seq + uint64(len(frame.Payload))
	return nil
}

func (s *attachmentOutput) snapshot(frame protocol.Frame) error {
	if !s.pendingSnapshot {
		return fmt.Errorf("session %s: received an unannounced snapshot", s.opts.SessionID)
	}
	s.altScreen.Observe(frame.Payload)
	if _, err := s.terminal.Output.Write(frame.Payload); err != nil {
		return err
	}
	s.result.LastSeq = s.snapshotSeq
	s.pendingSnapshot = false
	return nil
}

func (s *attachmentOutput) control(message protocol.Control) (bool, error) {
	switch message.Type {
	case protocol.TypeAttached:
		return false, s.attached(message)
	case protocol.TypeNesting:
		if message.SessionID == s.opts.SessionID {
			return false, s.updateKeys(message)
		}
	case protocol.TypeExit:
		s.result.Exited = true
		if message.ExitCode != nil {
			s.result.ExitCode = *message.ExitCode
		}
		return true, nil
	case protocol.TypeDetach:
		s.result.Detached = true
		return true, nil
	case protocol.TypeError:
		return false, s.responseError(message)
	}
	return false, nil
}

func (s *attachmentOutput) attached(message protocol.Control) error {
	s.acknowledged = true
	if err := s.updateKeys(message); err != nil {
		return err
	}
	if message.Snapshot {
		s.pendingSnapshot = true
		s.snapshotSeq = message.Seq
		return nil
	}
	s.result.LastSeq = message.Seq
	return nil
}

func (s *attachmentOutput) updateKeys(message protocol.Control) error {
	if err := s.keys.update(message); err != nil {
		return fmt.Errorf("session %s nesting: %w", s.opts.SessionID, err)
	}
	return nil
}

func (s *attachmentOutput) responseError(message protocol.Control) error {
	if message.Reason == protocol.ReasonAttached {
		return fmt.Errorf("session %s: %w", s.opts.SessionID, ErrSessionAttached)
	}
	// Legacy workers identify unsupported atomic claims only by their text.
	legacyUnsupported := message.Message == "expected "+protocol.TypeAttach ||
		message.Message == fmt.Sprintf("daemon: unknown control %q", protocol.TypeAttachDetached)
	if s.opts.IfDetached && legacyUnsupported {
		return fmt.Errorf("session %s: %w", s.opts.SessionID, ErrAttachDetachedUnsupported)
	}
	return daemonResponseError("session "+s.opts.SessionID, message.Message)
}

func relayInputBytes(keys *attachmentKeys, input []byte, sid protocol.SessionID, send func(protocol.Frame) error) bool {
	if len(input) == 0 {
		return false
	}
	if index := keys.detachIndex(input); index >= 0 {
		if index > 0 {
			_ = send(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: append([]byte(nil), input[:index]...)})
		}
		return true
	}
	// Failed writes drop one keystroke batch. Retiring the reader would leave
	// a reconnecting session painting output with no keyboard or detach key.
	_ = send(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: append([]byte(nil), input...)})
	return false
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

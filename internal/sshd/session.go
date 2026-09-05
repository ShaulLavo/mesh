package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	charmssh "github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"

	meshsession "github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/terminal"
)

type CommandKind uint8

const (
	maximumTerminalDimension = 2048
	maximumTerminalCells     = 256 << 10
	maximumTerminalName      = 64
)

const (
	CommandBrowse CommandKind = iota
	CommandAttach
	CommandList
)

type Command struct {
	Kind      CommandKind
	SessionID string
}

type Session struct {
	In            *os.File
	Out, Err      io.Writer
	Terminal      string
	Size          func() terminal.Size
	WindowChanges <-chan terminal.Size
	Command       Command
}

type SessionHandler func(context.Context, Session) (int, error)
type SessionHandlerFactory func(stateDir string) SessionHandler

type windowStateKey struct{}
type channelKey struct{}

// Charm's context belongs to the connection. Each channel needs its own
// lifetime and values so closing one terminal does not cancel its siblings.
type channelContext struct {
	charmssh.Context
	lifetime context.Context
	values   sync.Map
}

func (c *channelContext) Deadline() (time.Time, bool) { return c.lifetime.Deadline() }
func (c *channelContext) Done() <-chan struct{}       { return c.lifetime.Done() }
func (c *channelContext) Err() error                  { return c.lifetime.Err() }

func (c *channelContext) Value(key any) any {
	if value, ok := c.values.Load(key); ok {
		return value
	}
	return c.Context.Value(key)
}

func (c *channelContext) SetValue(key, value any) { c.values.Store(key, value) }

type sessionChannel struct {
	gossh.NewChannel
	ctx *channelContext
}

func (c sessionChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	channel, requests, err := c.NewChannel.Accept()
	if err == nil {
		c.ctx.SetValue(channelKey{}, channel)
	}
	return channel, requests, err //nolint:wrapcheck // preserve the channel acceptance result
}

func configureSessions(server *charmssh.Server, application SessionHandler, health charmssh.Handler) {
	server.ChannelHandlers = maps.Clone(server.ChannelHandlers)
	if server.ChannelHandlers == nil {
		server.ChannelHandlers = make(map[string]charmssh.ChannelHandler)
	}
	server.ChannelHandlers["session"] = handleSessionChannel
	server.PtyCallback = func(_ charmssh.Context, pty charmssh.Pty) bool {
		return application != nil && validWindow(pty.Window) && validTerminalName(pty.Term)
	}
	_ = charmssh.EmulatePty()(server)
	emulate := server.PtyHandler
	server.PtyHandler = func(ctx charmssh.Context, session charmssh.Session, pty charmssh.Pty) (func() error, error) {
		return prepareTerminal(ctx, session, pty, emulate)
	}
	middleware := secureMiddleware(func(session charmssh.Session) {
		handleSession(session, application, health)
	})
	server.Handler = func(session charmssh.Session) { middleware(terminalSession{Session: session}) }
}

// Wish's logger reads Pty while Charm writes its Window without a lock.
// Give middleware the same synchronized geometry used by the application.
type terminalSession struct{ charmssh.Session }

func (s terminalSession) Pty() (charmssh.Pty, <-chan charmssh.Window, bool) {
	state, ok := s.Context().Value(windowStateKey{}).(*windowState)
	if !ok {
		return charmssh.Pty{}, nil, false
	}
	size := state.size()
	return charmssh.Pty{Term: state.term, Window: charmssh.Window{Width: size.Cols, Height: size.Rows}}, nil, true
}

func handleSessionChannel(server *charmssh.Server, conn *gossh.ServerConn, incoming gossh.NewChannel, parent charmssh.Context) {
	lifetime, cancel := context.WithCancel(parent)
	defer cancel()
	ctx := &channelContext{Context: parent, lifetime: lifetime}
	charmssh.DefaultSessionHandler(server, conn, sessionChannel{NewChannel: incoming, ctx: ctx}, ctx)
}

type windowState struct {
	term    string
	latest  atomic.Pointer[terminal.Size]
	changes chan terminal.Size
	done    chan struct{}
}

func prepareTerminal(ctx charmssh.Context, session charmssh.Session, pty charmssh.Pty, emulate charmssh.PtyHandler) (func() error, error) {
	closeTerminal, err := emulate(ctx, session, pty)
	if err != nil {
		return nil, fmt.Errorf("sshd: prepare terminal: %w", err)
	}
	_, windows, _ := session.Pty()
	state := &windowState{term: pty.Term, changes: make(chan terminal.Size, 1), done: make(chan struct{})}
	size := terminal.Size{Cols: 80, Rows: 24}
	state.latest.Store(&size)
	state.update(pty.Window)
	ctx.SetValue(windowStateKey{}, state)
	// Start at pty-req, before shell/exec. Otherwise a second window-change
	// fills Charm's channel and prevents the shell request from being handled.
	go state.pump(windows)
	return func() error {
		<-state.done
		return closeTerminal()
	}, nil
}

func (s *windowState) size() terminal.Size { return *s.latest.Load() }

func (s *windowState) pump(windows <-chan charmssh.Window) {
	defer close(s.done)
	defer close(s.changes)
	for window := range windows {
		s.update(window)
	}
}

func (s *windowState) update(window charmssh.Window) {
	if !validWindow(window) {
		return
	}
	size := terminal.Size{Cols: window.Width, Rows: window.Height}
	s.latest.Store(&size)
	select {
	case <-s.changes:
	default:
	}
	s.changes <- size
}

func validWindow(window charmssh.Window) bool {
	if window.Width <= 0 || window.Height <= 0 || window.Width > maximumTerminalDimension || window.Height > maximumTerminalDimension {
		return false
	}
	return window.Width <= maximumTerminalCells/window.Height
}

func validTerminalName(name string) bool {
	if len(name) == 0 || len(name) > maximumTerminalName {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-+._", char) {
			continue
		}
		return false
	}
	return true
}

func handleSession(channel charmssh.Session, application SessionHandler, health charmssh.Handler) {
	state, _ := channel.Context().Value(windowStateKey{}).(*windowState)
	defer waitForWindows(state)
	defer channel.Close() //nolint:errcheck // also release the channel after a handler panic
	raw := strings.TrimSpace(channel.RawCommand())
	if raw == helloCommand || raw == "" && application == nil {
		health(channel)
		_ = channel.Exit(0)
		return
	}
	command, err := parseCommand(raw, state != nil, application != nil)
	if err != nil {
		finishSession(channel, 2, err)
		return
	}
	runSession(channel, application, command, state)
}

func parseCommand(raw string, hasPTY, installed bool) (Command, error) {
	if !installed {
		return Command{}, errors.New("SSH sessions are not configured")
	}
	if raw == "ls" {
		return Command{Kind: CommandList}, nil
	}
	if !hasPTY {
		return Command{}, errors.New("interactive SSH sessions require a terminal; use ssh -t host [session-id], or ssh host ls")
	}
	if raw == "" {
		return Command{Kind: CommandBrowse}, nil
	}
	id, err := meshsession.ParseID(raw)
	if err != nil {
		return Command{}, errors.New("unsupported SSH command; use a session ID or ls")
	}
	return Command{Kind: CommandAttach, SessionID: id}, nil
}

func runSession(channel charmssh.Session, application SessionHandler, command Command, state *windowState) {
	raw := channel.Context().Value(channelKey{}).(gossh.Channel)
	session := Session{Out: raw, Err: raw.Stderr(), Command: command, Size: func() terminal.Size {
		return terminal.Size{Cols: 80, Rows: 24}
	}}
	if state != nil {
		session.Terminal, session.Size, session.WindowChanges = state.term, state.size, state.changes
	}
	if command.Kind == CommandList {
		status, err := application(channel.Context(), session)
		finishSession(channel, status, err)
		return
	}
	runInteractiveSession(channel, application, session)
}

func runInteractiveSession(channel charmssh.Session, application SessionHandler, session Session) {
	input, writer, err := os.Pipe()
	if err != nil {
		finishSession(channel, 1, fmt.Errorf("sshd: create terminal input: %w", err))
		return
	}
	ctx, cancel := context.WithCancel(channel.Context())
	stopCancellation := closeSessionOnCancel(ctx, channel)
	done := make(chan struct{})
	session.In = input
	go func() {
		defer close(done)
		_, _ = io.Copy(writer, channel)
		_ = writer.Close()
		cancel()
	}()
	defer func() {
		stopCancellation()
		cancel()
		_ = input.Close()
		_ = writer.Close()
		_ = channel.Close()
		<-done
	}()
	status, err := application(ctx, session)
	// Exit sends the status before closing the channel to release its reader.
	finishSession(channel, status, err)
}

func closeSessionOnCancel(ctx context.Context, channel charmssh.Session) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		// EOF ends this SSH terminal even when its output window is full.
		// Sending the status before closing also keeps ordinary EOF successful.
		_ = channel.Exit(0)
		_ = channel.Close()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func finishSession(channel charmssh.Session, status int, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(channel.Stderr(), "mesh: %v\n", err)
	}
	if err != nil && status == 0 {
		status = 1
	}
	_ = channel.Exit(status)
}

func waitForWindows(state *windowState) {
	if state != nil {
		<-state.done
	}
}

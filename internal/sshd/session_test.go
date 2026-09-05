package sshd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	charmssh "github.com/charmbracelet/ssh"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/wish/testsession"
	gossh "golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/terminal"
)

func sessionTestServer(t *testing.T, handler SessionHandler) (*charmssh.Server, *gossh.ClientConfig) {
	t.Helper()
	key := generatePrivateKey(t)
	server := mustServer(t, Config{
		HostKey: generatePrivateKey(t), AuthorizedKeys: writeAuthorizedKeys(t, key, 0o600),
		Addr: "127.0.0.1:2222", Handler: handler,
	})
	return server, clientConfig(t, key, nil)
}

func TestSessionCommandGrammar(t *testing.T) {
	tests := []struct {
		raw       string
		pty       bool
		installed bool
		want      Command
		invalid   bool
	}{
		{raw: "", pty: true, installed: true, want: Command{Kind: CommandBrowse}},
		{raw: "7k3d", pty: true, installed: true, want: Command{Kind: CommandAttach, SessionID: "7K3D"}},
		{raw: "ls", installed: true, want: Command{Kind: CommandList}},
		{raw: "ls", pty: true, installed: true, want: Command{Kind: CommandList}},
		{raw: "", installed: true, invalid: true},
		{raw: "7K3D", installed: true, invalid: true},
		{raw: "ls", invalid: true},
		{raw: "ls --all", installed: true, invalid: true},
		{raw: "7K3D; id", pty: true, installed: true, invalid: true},
		{raw: "$(id)", pty: true, installed: true, invalid: true},
		{raw: "'7K3D'", pty: true, installed: true, invalid: true},
		{raw: "uname", pty: true, installed: true, invalid: true},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%q/pty=%t/installed=%t", test.raw, test.pty, test.installed), func(t *testing.T) {
			got, err := parseCommand(test.raw, test.pty, test.installed)
			if (err != nil) != test.invalid || !test.invalid && got != test.want {
				t.Fatalf("parseCommand() = %+v, %v; want %+v, invalid=%t", got, err, test.want, test.invalid)
			}
		})
	}
}

func TestSessionRejectsUnsafePTYRequests(t *testing.T) {
	server, config := sessionTestServer(t, func(context.Context, Session) (int, error) { return 0, nil })
	address := testsession.Listen(t, server)
	tests := []struct {
		name string
		term string
		cols int
		rows int
	}{
		{name: "zero columns", term: "xterm", cols: 0, rows: 24},
		{name: "oversized dimension", term: "xterm", cols: maximumTerminalDimension + 1, rows: 24},
		{name: "oversized screen", term: "xterm", cols: maximumTerminalDimension, rows: maximumTerminalDimension},
		{name: "empty terminal", cols: 80, rows: 24},
		{name: "terminal escape", term: "xterm\x1b[2J", cols: 80, rows: 24},
		{name: "terminal newline", term: "xterm\nPATH=/tmp", cols: 80, rows: 24},
		{name: "long terminal", term: strings.Repeat("x", maximumTerminalName+1), cols: 80, rows: 24},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertPTYRefused(t, address, config, test.term, test.rows, test.cols)
		})
	}
}

func assertPTYRefused(t *testing.T, address string, config *gossh.ClientConfig, term string, rows, cols int) {
	t.Helper()
	client, err := testsession.NewClientSession(t, address, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RequestPty(term, rows, cols, nil); err == nil {
		t.Fatalf("server accepted terminal %q at %dx%d", term, cols, rows)
	}
}

func TestSessionIgnoresUnsafeWindowChanges(t *testing.T) {
	state := &windowState{changes: make(chan terminal.Size, 1)}
	state.update(charmssh.Window{Width: 80, Height: 24})
	<-state.changes
	for _, window := range []charmssh.Window{
		{Width: 0, Height: 24},
		{Width: 80, Height: -1},
		{Width: maximumTerminalDimension + 1, Height: 24},
		{Width: maximumTerminalDimension, Height: maximumTerminalDimension},
	} {
		state.update(window)
	}
	if got := state.size(); got != (terminal.Size{Cols: 80, Rows: 24}) {
		t.Fatalf("invalid resize replaced terminal dimensions: %+v", got)
	}
	select {
	case invalid := <-state.changes:
		t.Fatalf("invalid resize reached the application: %+v", invalid)
	default:
	}
	state.update(charmssh.Window{Width: 120, Height: 40})
	if got := <-state.changes; got != (terminal.Size{Cols: 120, Rows: 40}) {
		t.Fatalf("valid resize after invalid requests = %+v", got)
	}
}

func TestSessionListAndFailureExitStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   int
	}{
		{name: "list"},
		{name: "process exit", status: 7, want: 7},
		{name: "error", err: errors.New("catalog unavailable"), want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkListExit(t, test.status, test.err, test.want)
		})
	}
}

func checkListExit(t *testing.T, status int, applicationErr error, want int) {
	t.Helper()
	server, config := sessionTestServer(t, func(_ context.Context, session Session) (int, error) {
		if session.Command.Kind != CommandList || session.In != nil {
			return 1, errors.New("one-shot list got an interactive input reader")
		}
		_, _ = io.WriteString(session.Out, "7K3D\trunning\n")
		return status, applicationErr
	})
	client := testsession.New(t, server, config)
	var stdout, stderr bytes.Buffer
	client.Stdout, client.Stderr = &stdout, &stderr
	err := client.Run("ls")
	assertExitStatus(t, err, want)
	if stdout.String() != "7K3D\trunning\n" {
		t.Fatalf("list output = %q", stdout.String())
	}
	if applicationErr != nil && !strings.Contains(stderr.String(), applicationErr.Error()) {
		t.Fatalf("stderr = %q, want %v", stderr.String(), applicationErr)
	}
}

func TestSessionRequiresPTYAndRejectsUnsupportedCommands(t *testing.T) {
	server, config := sessionTestServer(t, func(context.Context, Session) (int, error) {
		return 0, errors.New("invalid command reached the application")
	})
	address := testsession.Listen(t, server)
	for _, command := range []string{"", "7K3D", "ls --all", "hello extra", "sh -c id"} {
		client, err := testsession.NewClientSession(t, address, config)
		if err != nil {
			t.Fatal(err)
		}
		assertExitStatus(t, client.Run(command), 2)
	}
}

func TestSessionDrainsResizesBeforeShellAndPreservesOutputBytes(t *testing.T) {
	ready := make(chan terminal.Size, 1)
	server, config := sessionTestServer(t, func(ctx context.Context, session Session) (int, error) {
		if session.Terminal != "xterm-256color" || session.Command != (Command{Kind: CommandAttach, SessionID: "7K3D"}) {
			return 1, fmt.Errorf("unexpected terminal or command: %q %+v", session.Terminal, session.Command)
		}
		if err := awaitSessionWindow(ctx, session, terminal.Size{Cols: 199, Rows: 129}); err != nil {
			return 1, err
		}
		ready <- session.Size()
		return awaitSessionResize(ctx, session, terminal.Size{Cols: 151, Rows: 53})
	})
	client := testsession.New(t, server, config)
	keepInputOpen(t, client)
	if err := client.RequestPty("xterm-256color", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	for n := range 100 {
		if err := client.WindowChange(30+n, 100+n); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	client.Stdout = &output
	if err := client.Start("7k3d"); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-ready:
		if size != (terminal.Size{Cols: 199, Rows: 129}) {
			t.Fatalf("initial size after queued resizes = %+v", size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("window-change requests blocked the shell request")
	}
	if err := client.WindowChange(53, 151); err != nil {
		t.Fatal(err)
	}
	assertExitStatus(t, client.Wait(), 7)
	if output.String() != "raw\n\r\n\x1b[2J" {
		t.Fatalf("SSH changed terminal output bytes: %q", output.String())
	}
}

func awaitSessionResize(ctx context.Context, session Session, want terminal.Size) (int, error) {
	if err := awaitSessionWindow(ctx, session, want); err != nil {
		return 1, err
	}
	_, err := io.WriteString(session.Out, "raw\n\r\n\x1b[2J")
	return 7, err
}

func awaitSessionWindow(ctx context.Context, session Session, want terminal.Size) error {
	for session.Size() != want {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.WindowChanges:
		}
	}
	return nil
}

func TestSessionInputCanBeCancelledAndReused(t *testing.T) {
	stage := make(chan int, 2)
	server, config := sessionTestServer(t, func(_ context.Context, session Session) (int, error) {
		if err := cancelIdleReader(session); err != nil {
			return 1, err
		}
		stage <- 1
		reader, err := uv.NewCancelReader(session.In)
		if err != nil {
			return 1, err
		}
		defer reader.Close() //nolint:errcheck // the handler's result takes precedence
		var buffer [6]byte
		_, err = io.ReadFull(reader, buffer[:])
		if err != nil {
			return 1, err
		}
		_, err = session.Out.Write(buffer[:])
		return 0, err
	})
	client := testsession.New(t, server, config)
	input := keepInputOpen(t, client)
	var output bytes.Buffer
	client.Stdout = &output
	startInteractive(t, client)
	select {
	case <-stage:
	case <-time.After(2 * time.Second):
		t.Fatal("first input reader did not cancel")
	}
	if _, err := io.WriteString(input, "second"); err != nil {
		t.Fatal(err)
	}
	assertExitStatus(t, client.Wait(), 0)
	if output.String() != "second" {
		t.Fatalf("input after cancellation = %q", output.String())
	}
}

func cancelIdleReader(session Session) error {
	reader, err := uv.NewCancelReader(session.In)
	if err != nil {
		return err
	}
	defer reader.Close() //nolint:errcheck // preserve the cancellation result
	read := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		read <- err
	}()
	if !reader.Cancel() {
		return errors.New("SSH pipe reader cannot cancel")
	}
	select {
	case err := <-read:
		if err == nil {
			return errors.New("cancelled input read succeeded")
		}
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("cancelled input reader did not return")
	}
}

func TestClosingOneSessionCancelsOnlyThatChannel(t *testing.T) {
	started := make(chan context.Context, 2)
	finished := make(chan struct{}, 2)
	server, config := sessionTestServer(t, func(ctx context.Context, _ Session) (int, error) {
		started <- ctx
		<-ctx.Done()
		finished <- struct{}{}
		return 0, nil
	})
	client, err := gossh.Dial("tcp", testsession.Listen(t, server), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // test cleanup
	first := newInteractiveChannel(t, client)
	firstContext := <-started
	second := newInteractiveChannel(t, client)
	secondContext := <-started
	_ = first.Close()
	waitForSessionEnd(t, finished)
	if firstContext.Err() == nil || secondContext.Err() != nil {
		t.Fatalf("channel contexts after first close: first=%v second=%v", firstContext.Err(), secondContext.Err())
	}
	_ = second.Close()
	waitForSessionEnd(t, finished)
}

func TestSessionInputEOFCancelsApplication(t *testing.T) {
	finished := make(chan struct{}, 1)
	server, config := sessionTestServer(t, func(ctx context.Context, _ Session) (int, error) {
		<-ctx.Done()
		finished <- struct{}{}
		return 0, nil
	})
	client := testsession.New(t, server, config)
	input := keepInputOpen(t, client)
	startInteractive(t, client)
	_ = input.Close()
	waitForSessionEnd(t, finished)
	assertExitStatus(t, client.Wait(), 0)
}

func TestSessionInputEOFReleasesBlockedOutputWithoutClosingSibling(t *testing.T) {
	started := make(chan context.Context, 2)
	finished := make(chan struct{}, 2)
	server, config := sessionTestServer(t, func(ctx context.Context, session Session) (int, error) {
		defer func() { finished <- struct{}{} }()
		started <- ctx
		if session.Command.Kind == CommandAttach {
			_, err := session.Out.Write(bytes.Repeat([]byte("x"), 8<<20))
			return 0, err
		}
		<-ctx.Done()
		return 0, nil
	})
	client, err := gossh.Dial("tcp", testsession.Listen(t, server), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // test cleanup
	first, input, output := newBackpressuredSession(t, client)
	firstContext := <-started
	second := newInteractiveChannel(t, client)
	secondContext := <-started
	if _, err := io.ReadFull(output, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	waitForSessionEnd(t, finished)
	assertExitStatus(t, first.Wait(), 0)
	if firstContext.Err() == nil || secondContext.Err() != nil {
		t.Fatalf("channel contexts after input EOF: first=%v second=%v", firstContext.Err(), secondContext.Err())
	}
	if err := second.WindowChange(30, 100); err != nil {
		t.Fatalf("sibling resize after input EOF: %v", err)
	}
	_ = second.Close()
	waitForSessionEnd(t, finished)
}

func newBackpressuredSession(t *testing.T, client *gossh.Client) (*gossh.Session, io.WriteCloser, io.Reader) {
	t.Helper()
	channel, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	input := keepInputOpen(t, channel)
	output, err := channel.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.RequestPty("xterm-256color", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	if err := channel.Start("7K3D"); err != nil {
		t.Fatal(err)
	}
	return channel, input, output
}

func TestSessionPumpsStopAcross100ConnectDisconnectCycles(t *testing.T) {
	started := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	server, config := sessionTestServer(t, func(ctx context.Context, _ Session) (int, error) {
		started <- struct{}{}
		<-ctx.Done()
		finished <- struct{}{}
		return 0, nil
	})
	address := testsession.Listen(t, server)
	baseline := sessionGoroutines()
	// Pace real connections within the public listener's existing rate limit.
	ticker := time.NewTicker(105 * time.Millisecond)
	defer ticker.Stop()
	for range 100 {
		<-ticker.C
		disconnectSession(t, address, config, started, finished)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := sessionGoroutines(); got <= baseline {
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("SSH goroutines after 100 disconnects = %d, before = %d", sessionGoroutines(), baseline)
}

func disconnectSession(t *testing.T, address string, config *gossh.ClientConfig, started, finished <-chan struct{}) {
	t.Helper()
	client, err := gossh.Dial("tcp", address, config)
	if err != nil {
		t.Fatal(err)
	}
	newInteractiveChannel(t, client)
	waitForSessionEnd(t, started)
	_ = client.Close()
	waitForSessionEnd(t, finished)
}

func sessionGoroutines() int {
	stack := make([]byte, 1<<20)
	stack = stack[:runtime.Stack(stack, true)]
	count := 0
	for _, goroutine := range strings.Split(string(stack), "\n\n") {
		if strings.Contains(goroutine, "sshd.runInteractiveSession") || strings.Contains(goroutine, "sshd.(*windowState).pump") || strings.Contains(goroutine, "sshd.handleSessionChannel") {
			count++
		}
	}
	return count
}

func newInteractiveChannel(t *testing.T, client *gossh.Client) *gossh.Session {
	t.Helper()
	channel, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	keepInputOpen(t, channel)
	startInteractive(t, channel)
	return channel
}

func startInteractive(t *testing.T, client *gossh.Session) {
	t.Helper()
	if err := client.RequestPty("xterm-256color", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Shell(); err != nil {
		t.Fatal(err)
	}
}

func keepInputOpen(t *testing.T, client *gossh.Session) io.WriteCloser {
	t.Helper()
	input, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func waitForSessionEnd(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSH session did not complete its lifecycle step")
	}
}

func assertExitStatus(t *testing.T, err error, want int) {
	t.Helper()
	if want == 0 && err == nil {
		return
	}
	var exit *gossh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != want {
		t.Fatalf("SSH exit = %v, want status %d", err, want)
	}
}

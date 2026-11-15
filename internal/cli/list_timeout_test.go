package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestListReplyTimeout(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "local default", args: []string{"--daemon"}, want: 500 * time.Millisecond},
		{name: "remote default", want: 750 * time.Millisecond},
		{name: "local explicit default value", args: []string{"--daemon", "--timeout=750ms"}, want: 750 * time.Millisecond},
		{name: "local generous cap", args: []string{"--daemon", "--timeout=1m"}, want: time.Minute},
		{name: "remote explicit", args: []string{"--timeout=1m"}, want: time.Minute},
		{name: "local expired", args: []string{"--daemon", "--timeout=0"}, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertListReplyTimeout(t, test.args, test.want)
		})
	}
}

func assertListReplyTimeout(t *testing.T, args []string, want time.Duration) {
	t.Helper()
	command := (&application{}).listCommand()
	if err := command.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	viaDaemon, err := command.Flags().GetBool("daemon")
	if err != nil {
		t.Fatal(err)
	}
	timeout, err := command.Flags().GetDuration("timeout")
	if err != nil {
		t.Fatal(err)
	}
	if got := listReplyTimeout(command, viaDaemon, timeout); got != want {
		t.Fatalf("reply timeout = %s, want %s", got, want)
	}
}

func TestListDaemonHonorsExpiredExplicitTimeout(t *testing.T) {
	setupLocalCommandTest(t)
	_, _, err := executeCommand(t, Dependencies{}, "ls", "--daemon", "--timeout=0")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded before dialing", err)
	}
}

func TestListDaemonGenerousTimeoutStillCancelsBlockedRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeList {
			return errors.New("expected session.list request")
		}
		cancel()
		if _, err := conn.ReadFrame(); err == nil {
			return errors.New("list connection remained open after cancellation")
		}
		return nil
	})
	t.Setenv("MESH_STATE_DIR", filepath.Dir(socket))
	command := (&application{}).listCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--daemon", "--timeout=1m"})
	if err := command.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	awaitDaemonServer(t, done)
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/transport"
	gossh "golang.org/x/crypto/ssh"
)

func TestSSHAttachmentCloseDiagnosticUsesDaemonLog(t *testing.T) {
	checkSSHAttachmentLogRoute(t, false)
}

func TestSSHAttachmentCommandErrorStillUsesClientStderr(t *testing.T) {
	checkSSHAttachmentLogRoute(t, true)
}

func checkSSHAttachmentLogRoute(t *testing.T, commandError bool) {
	t.Helper()
	setupCommandTestHost(t)
	file, err := os.CreateTemp(t.TempDir(), "daemon.log")
	if err != nil {
		t.Fatal(err)
	}
	previous := log.Writer()
	log.SetOutput(file)
	t.Cleanup(func() { log.SetOutput(previous); _ = file.Close() })
	socket, daemonDone := startDaemonControlServer(t, 1, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeAttach || request.SessionID != "7K3D" {
			return fmt.Errorf("unexpected SSH attachment: %+v", request)
		}
		response := protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"}
		if commandError {
			response = protocol.Control{Type: protocol.TypeError, Message: "fixture denied attachment"}
		}
		return writeDaemonControl(conn, response)
	})
	app := sshApplication{socket: socket}
	client := appSSHFixture(t, func(ctx context.Context, session sshd.Session) (int, error) {
		if session.Terminal != "xterm-256color" {
			return 1, fmt.Errorf("SSH fixture did not retain its terminal: %q", session.Terminal)
		}
		_, err := app.attach(ctx, session, sshAttachment{id: "7K3D"})
		if err != nil {
			return 1, err
		}
		return 0, nil
	})
	channel, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	input, err := channel.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	if err := channel.RequestPty("xterm-256color", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	channel.Stdout, channel.Stderr = &stdout, &stderr
	err = channel.Run("7K3D")
	checkSSHAttachmentResult(t, commandError, err, stderr.String())
	select {
	case err := <-daemonDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("SSH fixture daemon did not stop")
	}
	diagnostics, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !commandError && !strings.Contains(string(diagnostics), `stage=established-stream classification=terminal failed=false error="transport: read stream frame: EOF"`) {
		t.Fatalf("daemon log lost the SSH close cause: %q", diagnostics)
	}
	if strings.Contains(stdout.String(), "stage=") || strings.Contains(stderr.String(), "stage=") {
		t.Fatalf("close diagnostic reached SSH client: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func checkSSHAttachmentResult(t *testing.T, commandError bool, err error, stderr string) {
	t.Helper()
	if !commandError {
		if err != nil || stderr != "" {
			t.Fatalf("ordinary SSH close: error=%v stderr=%q", err, stderr)
		}
		return
	}
	var exit *gossh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 || !strings.Contains(stderr, "fixture denied attachment") {
		t.Fatalf("SSH command error lost: error=%v stderr=%q", err, stderr)
	}
}

package cli

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

func TestSSHListUsesConfiguredDaemonWithoutTerminal(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeList {
			return fmt.Errorf("unexpected request %s", request.Type)
		}
		return writeDaemonControl(conn, protocol.Control{
			Type: protocol.TypeListed, RequestID: request.RequestID,
			Sessions: []protocol.SessionInfo{{
				ID: "7K3D", HostID: "ssh-host", Command: []string{"sh", "unsafe\x1b[2J"},
				Cwd: "/tmp/work", State: "detached", CreatedAt: time.Now(),
			}},
		})
	})
	var output bytes.Buffer
	handler := NewSSHSessionHandler(filepath.Dir(socket), nil)
	status, err := handler(t.Context(), sshd.Session{Out: &output, Command: sshd.Command{Kind: sshd.CommandList}})
	if err != nil || status != 0 {
		t.Fatalf("list returned %d, %v", status, err)
	}
	if !strings.Contains(output.String(), "7K3D") || !strings.Contains(output.String(), "detached") || strings.Contains(output.String(), "\x1b") {
		t.Fatalf("list output = %q", output.String())
	}
	awaitDaemonServer(t, done)
}

func TestSSHCreateUsesClientTerminalAndFreshNesting(t *testing.T) {
	t.Setenv("TERM", "dumb")
	t.Setenv("MESH_DEPTH", "27")
	command := []string{"/bin/sh", "retained"}
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeCreate || request.Depth != 1 || request.Term != "xterm-256color" {
			return fmt.Errorf("wrong SSH creation metadata: %#v", request)
		}
		if request.Cols != 101 || request.Rows != 37 || request.Cwd != "/tmp/original" || !slices.Equal(request.Command, command) {
			return fmt.Errorf("wrong SSH creation arguments: %#v", request)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeCreated, RequestID: request.RequestID, SessionID: "7K3D"})
	})
	app := sshApplication{socket: socket}
	target, err := app.create(t.Context(), sshd.Session{
		Terminal: "xterm-256color", Size: func() terminal.Size { return terminal.Size{Cols: 101, Rows: 37} },
	}, command, "/tmp/original")
	if err != nil {
		t.Fatal(err)
	}
	if target.id != "7K3D" || !target.ifDetached || target.lastSeq == nil || *target.lastSeq != 0 {
		t.Fatalf("fresh session attachment = %#v", target)
	}
	awaitDaemonServer(t, done)
}

func TestSSHRejectsRemotePickerOperations(t *testing.T) {
	app := sshApplication{socket: filepath.Join(t.TempDir(), "missing.sock")}
	_, err := app.inspect(t.Context(), PickerInspectRequest{HostAlias: "pc", SessionID: "7K3D"})
	if err == nil || !strings.Contains(err.Error(), "only this host") {
		t.Fatalf("remote inspection = %v", err)
	}
	err = app.action(t.Context(), PickerSessionActionRequest{HostAlias: "pc", SessionID: "7K3D", Action: PickerKillSession})
	if err == nil || !strings.Contains(err.Error(), "only sessions on this host") {
		t.Fatalf("remote action = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = app.catalog(ctx)
	if err == nil {
		t.Fatal("cancelled catalog request succeeded")
	}
}

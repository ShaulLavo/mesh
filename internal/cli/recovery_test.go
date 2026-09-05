package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestPreviousOutputReadsBeyondCatalogPreviewWithoutNetwork(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "7K3D", worker.StateInterrupted)
	config, err := localRecoveryConfig()
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{"earlier-than-visible-preview"}
	for range protocol.MaxInspectionPreviewRows + 5 {
		lines = append(lines, "later output")
	}
	record := recovery.Record{Version: recovery.Version, HostID: config.HostID, SessionID: "7K3D",
		CheckpointAt: commandTestTime, Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryLaunch,
		Command: []string{"/bin/sh"}, Lines: lines}
	if err := recovery.Write(filepath.Join(config.SessionsDir, "7K3D"), record); err != nil {
		t.Fatal(err)
	}
	output, _, err := executeCommand(t, Dependencies{DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
		t.Fatal("local previous output queried a remote host")
		return nil, nil
	}}, "logs", "7K3D", "--previous")
	if err != nil || !strings.Contains(output, "Previous output") || !strings.Contains(output, lines[0]) {
		t.Fatalf("saved output = %q, %v", output, err)
	}
}

func TestSSHRecoveryUsesSharedTransactionAndDetachedClaim(t *testing.T) {
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeRecover || request.SessionID != "7K3D" || request.RecoveryAction != "command" {
			return fmt.Errorf("unexpected recovery request: %#v", request)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeRecovered, RequestID: request.RequestID,
			RecoveryResult: &recovery.Result{SessionID: "ABCD", RecoveredFrom: "7K3D", State: worker.StateDetached}})
	})
	app := sshApplication{socket: socket}
	target, err := app.relaunch(t.Context(), sshd.Session{Err: io.Discard, Size: func() terminal.Size {
		return terminal.Size{Cols: 80, Rows: 24}
	}}, "7K3D", recovery.ActionCommand)
	if err != nil || target == nil || target.id != "ABCD" || !target.ifDetached {
		t.Fatalf("SSH recovery = %#v, %v", target, err)
	}
	awaitDaemonServer(t, done)
}

func TestSSHRecoveryReportsExactRemoteTarget(t *testing.T) {
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeRecovered, RequestID: request.RequestID,
			RecoveryResult: &recovery.Result{Remote: &recovery.Target{HostID: "remote-host", SessionID: "ABCD"}}})
	})
	app := sshApplication{socket: socket}
	_, err := app.relaunch(t.Context(), sshd.Session{Err: io.Discard, Size: func() terminal.Size {
		return terminal.Size{Cols: 80, Rows: 24}
	}}, "7K3D", recovery.ActionDefault)
	if err == nil || !strings.Contains(err.Error(), "remote-host/ABCD") || !strings.Contains(err.Error(), "Open shell") {
		t.Fatalf("SSH remote continuation = %v", err)
	}
	awaitDaemonServer(t, done)
}

func TestRecoveryCommandsReserveAliases(t *testing.T) {
	for _, name := range []string{"recover", "recovery-command", "shell-init", "shell-update"} {
		if _, err := ValidateHostAlias(name); err == nil {
			t.Fatalf("accepted command %s as a host alias", name)
		}
	}
}

func TestLegacyDefaultRecoveryDoesNotRunSavedCommand(t *testing.T) {
	host := setupCommandTestHost(t)
	host.sessionState = worker.StateInterrupted
	_, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial,
		Picker: func(context.Context, PickerInput) (PickerSelection, error) {
			return PickerSelection{HostAlias: "pc", SessionID: "7K3D", Relaunch: true}, nil
		},
	}, "--raw")
	if err == nil || host.eventCount(protocol.TypeCreate) != 0 {
		t.Fatalf("legacy default recovery launched a command: %v, events %v", err, host.recorded())
	}
}

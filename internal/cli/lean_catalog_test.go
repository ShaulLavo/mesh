package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestLeanClientAcceptsFullCatalogFromOldDaemon(t *testing.T) {
	saved := &recovery.Record{Lines: []string{"old daemon preview"}}
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeList || !request.Lean {
			return fmt.Errorf("expected additive lean request, got %+v", request)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeListed, RequestID: request.RequestID, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: "host", Command: []string{"sh"}, Cwd: "/work", State: "running", CreatedAt: commandTestTime, Recovery: saved, MemoryBytes: 12345}}})
	})
	rows, err := ListViaDaemon(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	awaitDaemonServer(t, done)
	if len(rows) != 1 || rows[0].RecoveryDetailsOmitted || !slices.Equal(rows[0].Recovery.Lines, saved.Lines) || rows[0].MemoryBytes != 12345 {
		t.Fatalf("old daemon response changed: %+v", rows)
	}
}

func TestSavedInspectionRejectsWrongOwnerAndOversizedPreview(t *testing.T) {
	saved := recovery.Record{Version: recovery.Version, HostID: "host", SessionID: "7K3D", CheckpointAt: commandTestTime, Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryShell, Command: []string{"sh"}, Lines: []string{"same preview"}}
	host := HostRecord{ID: "host", MachineName: "pc"}
	inspected, err := savedInspection(host, "7K3D", saved)
	if err != nil || !slices.Equal(inspected.Recovery.Lines, saved.Lines) {
		t.Fatalf("saved preview = %+v, %v", inspected, err)
	}
	saved.HostID = "another-host"
	if _, err := savedInspection(host, "7K3D", saved); err == nil {
		t.Fatal("accepted wrong saved owner")
	}
	saved.HostID = "host"
	saved.Lines = make([]string, protocol.MaxInspectionPreviewRows+1)
	if _, err := savedInspection(host, "7K3D", saved); err == nil {
		t.Fatal("accepted oversized saved preview")
	}
	saved.Lines = nil
	saved.CheckpointAt = time.Time{}
	if _, err := savedInspection(host, "7K3D", saved); err != nil {
		t.Fatalf("empty saved preview rejected: %v", err)
	}
}

func TestLocalCatalogMemoryComesFromDaemonSample(t *testing.T) {
	var hostID string
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeList || !request.Lean {
			return fmt.Errorf("local memory request = %+v", request)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeListed, RequestID: request.RequestID, Sessions: []protocol.SessionInfo{{ID: "71VE", HostID: hostID, Command: []string{"sh"}, Cwd: "/work", State: "running", CreatedAt: commandTestTime, MemoryBytes: 64 << 20}}})
	})
	stateDir := filepath.Dir(socket)
	t.Setenv("MESH_STATE_DIR", stateDir)
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	hostID = host.ID
	writeLocalSessionDir(t, "71VE", worker.StateRunning)
	dir, err := paths.SessionDir("71VE")
	if err != nil {
		t.Fatal(err)
	}
	controlTestListener(t, paths.Socket(dir))
	rows, err := localSessionRowsWithDaemonMemory(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	awaitDaemonServer(t, done)
	if len(rows) != 1 || rows[0].MemoryBytes != 64<<20 {
		t.Fatalf("local sampler rows = %+v", rows)
	}
}

func TestLocalCatalogSurvivesUnavailableMemorySampler(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "70C7", worker.StateExited)
	stateDir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := localSessionRowsWithDaemonMemory(context.Background(), stateDir)
	if err != nil || len(rows) != 1 || rows[0].ID != "70C7" || rows[0].MemoryBytes != 0 {
		t.Fatalf("daemonless listing = %+v, %v", rows, err)
	}
}

func TestLocalSavedInspectionPreservesKeylessLaunchFallback(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "70C7", worker.StateExited)
	current, err := Find("70C7")
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := inspectSavedLocalSession(current)
	if err != nil || inspection.Recovery == nil || !inspection.Recovery.CheckpointAt.IsZero() || inspection.Recovery.ShellDirectory != current.Cwd {
		t.Fatalf("keyless launch fallback = %+v, %v", inspection, err)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestLeanCatalogKeepsMetadataAndInspectsSavedPreview(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	dir := t.TempDir()
	code := 0
	stored := storage.Session{ID: "7K3D", HostID: "host-a", Command: []string{"/bin/sh"}, Cwd: "/work", State: storage.StateExited, CreatedAt: now.Add(-time.Hour), ExitCode: &code}
	catalog := &lifecycleTestCatalog{sessions: []storage.Session{stored}}
	l, err := newLifecycle(lifecycleConfig{Catalog: catalog, Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
		return nil, errors.New("ended worker socket is unavailable")
	}), Host: storage.Host{ID: "host-a", MeshIdentity: "key"}, SessionsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(dir, "7K3D")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(sessionDir, worker.Meta{ID: "7K3D", Command: stored.Command, Cwd: stored.Cwd, State: worker.StateExited, CreatedAt: stored.CreatedAt, ExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	saved := recovery.Record{Version: recovery.Version, HostID: "host-a", SessionID: "7K3D", CheckpointAt: now, Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryShell, Title: "saved title", LastOutputAt: now, Command: stored.Command, Lines: []string{strings.Repeat("x", 160), "same saved preview"}}
	if err := recovery.Write(sessionDir, saved); err != nil {
		t.Fatal(err)
	}
	var request protocol.Control
	if err := json.Unmarshal([]byte(`{"type":"session.list","requestId":"lean","lean":true}`), &request); err != nil {
		t.Fatal(err)
	}
	lean, _, err := l.HandleControl(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := l.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeList, RequestID: "old-client"})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Sessions[0].Recovery.Lines) != 2 {
		t.Fatal("old client lost its preview")
	}
	if len(lean.Sessions[0].Recovery.Lines) != 0 || len(lean.Sessions[0].Recovery.Command) != 0 {
		t.Fatal("lean catalog includes heavy recovery fields")
	}
	if !lean.Sessions[0].LastActiveAt().Equal(full.Sessions[0].LastActiveAt()) {
		t.Fatal("lean catalog changes ordering")
	}
	inspected, _, err := l.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeInspect, RequestID: "saved-preview", SessionID: "7K3D", PreviewCols: 160, PreviewRows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Recovery == nil || !reflect.DeepEqual(inspected.Recovery.Lines, full.Sessions[0].Recovery.Lines) {
		t.Fatal("explicit inspection changes saved preview")
	}
}

package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

// A saved terminal screen belongs to one reservation. After the job is
// removed and its session ID reused, the old receipt reads nothing.
func TestSavedTerminalScreenStaysWithItsReservation(t *testing.T) {
	const host = "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	sessions := filepath.Join(compactSocketTempDir(t), "s")
	l := mustLifecycle(t, lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: host, MeshIdentity: host}, SessionsDir: sessions})
	ref, err := worker.Reserve(sessions, session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := time.Date(2026, 11, 22, 1, 2, 3, 0, time.UTC)
	record := recovery.Record{Version: 1, HostID: host, SessionID: ref.ID, CheckpointAt: checkpoint, Shell: "/bin/sh", ShellDirectory: "/", DirectorySource: recovery.DirectoryShell, Command: []string{"/bin/sh"}, Lines: []string{"OWN-SCREEN"}}
	if err := recovery.Write(filepath.Join(sessions, ref.ID), record); err != nil {
		t.Fatal(err)
	}
	own := execution.Binding{Host: host, Reservation: ref}
	screen, err := l.savedTerminalScreen(t.Context(), own, 80, 24)
	if err != nil || len(screen.Preview) == 0 || screen.Preview[0] != "OWN-SCREEN" || !screen.ObservedAt.Equal(checkpoint) {
		t.Fatalf("own saved screen = %+v, %v", screen, err)
	}
	// Another reservation of the same ID (a reused session) is not this job.
	stale := own
	stale.Reservation.Instance = "0123456789abcdef0123456789abcdef"
	if _, err := l.savedTerminalScreen(t.Context(), stale, 80, 24); !errors.Is(err, execution.ErrResultExpired) {
		t.Fatalf("screen of a replaced reservation = %v, want result expired", err)
	}
}

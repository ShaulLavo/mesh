package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

// mesh mcp's human-session tools (list, inspect, hibernate) see only
// sessions a person started. A keyed terminal job, of any owner, and a
// service worker stay on their own controls, and their worker is never
// contacted through these tools.
func TestHumanSessionOnlyControlsSkipJobsAndServiceWorkers(t *testing.T) {
	sessions := filepath.Join(compactSocketTempDir(t), "s")
	ids := map[session.Purpose]string{}
	for _, purpose := range []session.Purpose{session.Generic, session.Execution, session.Managed} {
		ref, err := worker.Reserve(sessions, purpose)
		if err != nil {
			t.Fatal(err)
		}
		ids[purpose] = ref.ID
	}
	var contacted atomic.Int32
	connector := lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
		contacted.Add(1)
		return nil, errors.New("fixture worker is offline")
	})
	rows := make([]storage.Session, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, storage.Session{ID: storage.SessionID(id), State: storage.StateDetached, CreatedAt: time.Now().UTC()})
	}
	host := storage.Host{ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"}
	l := mustLifecycle(t, lifecycleConfig{Catalog: &lifecycleTestCatalog{sessions: rows}, Connector: connector, Host: host, SessionsDir: sessions})

	listed, _, err := l.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeList, RequestID: "list", HumanSessionOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != ids[session.Generic] {
		t.Fatalf("human-only list = %+v, want only %s", listed.Sessions, ids[session.Generic])
	}
	full, _, err := l.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeList, RequestID: "list-all"})
	if err != nil || len(full.Sessions) != 3 {
		t.Fatalf("account list = %d sessions, %v; want all 3", len(full.Sessions), err)
	}

	for _, purpose := range []session.Purpose{session.Execution, session.Managed} {
		for _, kind := range []string{protocol.TypeInspect, protocol.TypeHibernate} {
			request := protocol.Control{Type: kind, RequestID: "x", SessionID: ids[purpose], PreviewCols: 80, PreviewRows: 24, HumanSessionOnly: true}
			_, _, err := l.HandleControl(t.Context(), request)
			if !errors.Is(err, errAccessNotFound) {
				t.Fatalf("%s of a %s worker = %v, want not found", kind, purpose, err)
			}
			if code := clientErrorCode(err); code != "session.not_found" {
				t.Fatalf("%s refusal code = %q", kind, code)
			}
		}
	}
	if contacted.Load() != 0 {
		t.Fatal("a refused human-session control contacted a private worker")
	}
	// A person's session passes the limit and reaches the usual hibernation
	// checks (here, its missing fixture metadata).
	_, _, err = l.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeHibernate, RequestID: "h", SessionID: ids[session.Generic], HumanSessionOnly: true})
	if err == nil || errors.Is(err, errAccessNotFound) {
		t.Fatalf("person session hibernate = %v; want it past the human-session limit", err)
	}
}

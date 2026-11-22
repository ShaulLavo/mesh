package daemon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func TestScopedOutputExcludesPipeAndLineAllocations(t *testing.T) {
	for _, purpose := range []session.Purpose{session.PipeExecution, session.LineExecution} {
		t.Run(string(purpose), func(t *testing.T) {
			f := newScopedFixture(t.Context(), t, false)
			enableScopedOutput(t, f)
			sessions := compactSocketTempDir(t)
			f.server.lifecycle.sessionsDir = sessions
			ref, err := worker.Reserve(sessions, purpose)
			if err != nil {
				t.Fatal(err)
			}
			f.catalog.rows[0].ID = storage.SessionID(ref.ID)
			f.catalog.rows[0].State = storage.StateExited
			if err := os.WriteFile(paths.Log(filepath.Join(sessions, ref.ID)), []byte(disclosureSentinel), 0600); err != nil {
				t.Fatal(err)
			}
			assertScopedOutputHidden(t, f, ref.ID)
		})
	}
}

// Application-owned terminal jobs are private to their caller even though
// they run in a PTY like a public session. Account-owned keyed terminal jobs
// keep the documented scoped-output policy for public sessions.
func TestScopedOutputExcludesApplicationTerminalJobs(t *testing.T) {
	cases := []struct {
		name   string
		key    func(host string) execution.Key
		hidden bool
	}{
		{name: "application", hidden: true, key: func(host string) execution.Key {
			return execution.Key{Host: host, Class: execution.ApplicationClass, Principal: strings.Repeat("a", 64), Opaque: "term-1", Context: strings.Repeat("c", 32), Operation: execution.OperationExec}
		}},
		{name: "local-account", hidden: false, key: func(host string) execution.Key {
			return execution.Key{Host: host, Class: executionLocalAccountClass, Principal: "daemon-account", Opaque: "term-1"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFixture(t.Context(), t, false)
			enableScopedOutput(t, f)
			sessions := filepath.Join(compactSocketTempDir(t), "s")
			if err := os.MkdirAll(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			f.server.lifecycle.sessionsDir = sessions
			id := reserveTerminalJob(t, f.server.lifecycle, tc.key(string(f.server.lifecycle.host.ID)))
			f.catalog.rows[0].ID = storage.SessionID(id)
			f.catalog.rows[0].State = storage.StateExited
			if err := os.WriteFile(paths.Log(filepath.Join(sessions, id)), []byte(disclosureSentinel), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.hidden {
				assertScopedOutputHidden(t, f, id)
				return
			}
			result := scopedOutput(t, f, protocol.AccessOutput{SessionID: id, Format: "tail"})
			if result.Result == nil || result.Result.BytesBase64 == nil {
				t.Fatalf("public terminal job tail = %+v", result)
			}
			raw, err := base64.StdEncoding.DecodeString(*result.Result.BytesBase64)
			if err != nil || string(raw) != disclosureSentinel {
				t.Fatalf("public terminal job tail = %q, %v", raw, err)
			}
		})
	}
}

// A keyed terminal allocation whose receipt cannot be read never becomes
// public: the scoped reader learns nothing.
func TestScopedOutputHidesTerminalJobWithUnreadableOwner(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	sessions := compactSocketTempDir(t)
	f.server.lifecycle.sessionsDir = sessions
	ref, err := worker.Reserve(sessions, session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	f.catalog.rows[0].ID = storage.SessionID(ref.ID)
	f.catalog.rows[0].State = storage.StateExited
	if err := os.WriteFile(paths.Log(filepath.Join(sessions, ref.ID)), []byte(disclosureSentinel), 0600); err != nil {
		t.Fatal(err)
	}
	assertScopedOutputHidden(t, f, ref.ID)
}

// reserveTerminalJob stores a durable receipt for a keyed terminal job
// without launching it. Application keys get a context first, as the daemon
// requires for application jobs.
func reserveTerminalJob(t *testing.T, l *lifecycle, key execution.Key) string {
	t.Helper()
	journal, err := l.executionJournal()
	if err != nil {
		t.Fatal(err)
	}
	intent, err := execution.Normalize(execution.Intent{Kind: execution.KindPTY, Command: []string{"/bin/true"}, Cwd: "/"})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	allocate := func() (execution.Binding, error) {
		ref, err := worker.Reserve(l.sessionsDir, session.Execution)
		if err != nil {
			return execution.Binding{}, fmt.Errorf("reserve terminal worker: %w", err)
		}
		id = ref.ID
		return execution.NewBinding(key, intent, ref, nil), nil
	}
	if key.Class != execution.ApplicationClass {
		if _, owner, err := journal.Reserve(t.Context(), key, intent, allocate); err != nil || !owner {
			t.Fatalf("reserve terminal job: owner=%v err=%v", owner, err)
		}
		return id
	}
	openKey := execution.Key{Host: key.Host, Class: key.Class, Principal: key.Principal, Opaque: "ctx-1", Operation: execution.OperationContextOpen}
	c, _, err := journal.OpenContext(t.Context(), openKey, execution.Digest("/"), func() (execution.ContextSpec, error) {
		return execution.ContextSpec{Cwd: "/", Shell: protocol.ContextShellNone}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	key.Context = c.ID
	resolve := func(execution.Context) (execution.Intent, map[string]string, error) { return intent, nil, nil }
	reserve := func(execution.LaunchSnapshot) (execution.Binding, error) { return allocate() }
	if _, owner, err := journal.ReserveInContext(t.Context(), key, execution.Digest(intent), resolve, reserve); err != nil || !owner {
		t.Fatalf("reserve application terminal job: owner=%v err=%v", owner, err)
	}
	return id
}

func assertScopedOutputHidden(t *testing.T, f *scopedFixture, id string) {
	t.Helper()
	for _, request := range []protocol.AccessOutput{
		{SessionID: id, Format: "tail"},
		{SessionID: id, Format: "preview", Cols: 1, Rows: 1},
	} {
		result := scopedOutput(t, f, request)
		if result.ErrorCode != "session.not_found" || result.Result != nil {
			t.Fatalf("private worker admitted to scoped output: %+v", result)
		}
	}
}

// Managed service and app workers are Mesh's own: a scoped reader gets
// neither their live nor their saved output, and generic recovery never
// restarts one as a detached session.
func TestScopedOutputAndRecoveryExcludeManagedWorkers(t *testing.T) {
	for _, state := range []storage.SessionState{storage.StateDetached, storage.StateExited} {
		t.Run(string(state), func(t *testing.T) {
			f := newScopedFixture(t.Context(), t, false)
			enableScopedOutput(t, f)
			sessions := compactSocketTempDir(t)
			f.server.lifecycle.sessionsDir = sessions
			ref, err := worker.Reserve(sessions, session.Managed)
			if err != nil {
				t.Fatal(err)
			}
			f.catalog.rows[0].ID = storage.SessionID(ref.ID)
			f.catalog.rows[0].State = state
			if err := os.WriteFile(paths.Log(filepath.Join(sessions, ref.ID)), []byte(disclosureSentinel), 0600); err != nil {
				t.Fatal(err)
			}
			assertScopedOutputHidden(t, f, ref.ID)
			assertRecoveryRefused(t, f.server.lifecycle, ref.ID)
		})
	}
}

// Damaged ownership evidence is never read as a person's session: an
// unreadable allocation, or worker metadata naming one that is missing.
func TestScopedOutputHidesDamagedOwnershipEvidence(t *testing.T) {
	damage := map[string]func(t *testing.T, dir string){
		"unreadable allocation": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".reservation"), []byte("{broken"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"metadata names an allocation": func(t *testing.T, dir string) {
			meta := worker.Meta{ID: filepath.Base(dir), Reservation: &session.Reservation{ID: filepath.Base(dir), Purpose: session.Execution}, ExecutionKind: execution.KindPTY}
			if err := worker.WriteMeta(dir, meta); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, apply := range damage {
		t.Run(name, func(t *testing.T) {
			f := newScopedFixture(t.Context(), t, false)
			enableScopedOutput(t, f)
			sessions := compactSocketTempDir(t)
			f.server.lifecycle.sessionsDir = sessions
			dir := filepath.Join(sessions, "4R8X")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			apply(t, dir)
			f.catalog.rows[0].ID = "4R8X"
			f.catalog.rows[0].State = storage.StateExited
			if err := os.WriteFile(paths.Log(dir), []byte(disclosureSentinel), 0600); err != nil {
				t.Fatal(err)
			}
			assertScopedOutputHidden(t, f, "4R8X")
		})
	}
}

// A person's own session keeps the documented scoped-output policy.
func TestScopedOutputServesPersonSessions(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	sessions := compactSocketTempDir(t)
	f.server.lifecycle.sessionsDir = sessions
	ref, err := worker.Reserve(sessions, session.Generic)
	if err != nil {
		t.Fatal(err)
	}
	f.catalog.rows[0].ID = storage.SessionID(ref.ID)
	f.catalog.rows[0].State = storage.StateExited
	if err := os.WriteFile(paths.Log(filepath.Join(sessions, ref.ID)), []byte(disclosureSentinel), 0600); err != nil {
		t.Fatal(err)
	}
	result := scopedOutput(t, f, protocol.AccessOutput{SessionID: ref.ID, Format: "tail"})
	if result.Result == nil || result.Result.BytesBase64 == nil {
		t.Fatalf("person session tail = %+v", result)
	}
}

func assertRecoveryRefused(t *testing.T, l *lifecycle, id string) {
	t.Helper()
	for _, kind := range []string{protocol.TypeRecover, protocol.TypeRecoveryRead} {
		_, err := l.recoveryControl(t.Context(), protocol.Control{Type: kind, RequestID: "recover", SessionID: id, RecoveryAction: "command"})
		if !errors.Is(err, errAccessNotFound) {
			t.Fatalf("%s of a private worker = %v, want not found", kind, err)
		}
	}
}

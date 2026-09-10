package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func bindingStateDir(t *testing.T) {
	t.Helper()
	t.Setenv("MESH_STATE_DIR", t.TempDir())
}

func TestTerminalBindingRoundTripsAndForgets(t *testing.T) {
	bindingStateDir(t)
	created := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	want := TerminalBinding{
		Source: "cmux-pane", HostID: "host-id", SessionID: "7K3D",
		OriginID: "2QW9", CreatedAt: created, BoundAt: created.Add(time.Minute),
	}
	if err := saveTerminalBinding("key", want); err != nil {
		t.Fatal(err)
	}
	got, found := loadTerminalBinding("key")
	if !found {
		t.Fatal("binding was not found after saving it")
	}
	if got.SessionID != want.SessionID || got.HostID != want.HostID || got.OriginID != want.OriginID {
		t.Fatalf("binding = %#v, want %#v", got, want)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("createdAt = %s, want %s", got.CreatedAt, created)
	}
	if got.Version != terminalBindingVersion {
		t.Fatalf("version = %d, want %d", got.Version, terminalBindingVersion)
	}
	if err := forgetTerminalBinding("key"); err != nil {
		t.Fatal(err)
	}
	if _, found := loadTerminalBinding("key"); found {
		t.Fatal("binding survived being forgotten")
	}
	if err := forgetTerminalBinding("key"); err != nil {
		t.Fatalf("forgetting an absent binding must be a no-op: %v", err)
	}
}

// A tab losing its memory is a small annoyance; a command that fails because of
// a file it owns is worse. Every unreadable shape must read as "not bound".
func TestUnreadableTerminalBindingReadsAsAbsent(t *testing.T) {
	bindingStateDir(t)
	dir, err := bindingDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name     string
		contents string
	}{
		{"not json", "{{{"},
		{"unsupported version", `{"version":99,"sessionId":"7K3D","boundAt":"2026-09-10T12:00:00Z"}`},
		{"unknown field", `{"version":1,"sessionId":"7K3D","boundAt":"2026-09-10T12:00:00Z","surprise":1}`},
		{"trailing data", `{"version":1,"sessionId":"7K3D","boundAt":"2026-09-10T12:00:00Z"} {}`},
		{"invalid session id", `{"version":1,"sessionId":"not-an-id","boundAt":"2026-09-10T12:00:00Z"}`},
		{"oversized", `{"version":1,"sessionId":"7K3D","source":"` + strings.Repeat("x", maximumBindingBytes) + `"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "key.json"), []byte(testCase.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if binding, found := loadTerminalBinding("key"); found {
				t.Fatalf("loaded %#v from %s, want it treated as absent", binding, testCase.name)
			}
		})
	}
}

func TestSavingAnInvalidTerminalBindingIsRefused(t *testing.T) {
	bindingStateDir(t)
	if err := saveTerminalBinding("key", TerminalBinding{SessionID: "not-an-id"}); err == nil {
		t.Fatal("saved a binding whose session id does not parse")
	}
	if _, found := loadTerminalBinding("key"); found {
		t.Fatal("a refused save left a binding behind")
	}
}

// Session ids are four characters and are freed with their directory, so the
// same id comes back on an unrelated session. Creation time is what separates
// the session this terminal opened from a later one wearing its id.
func TestBindingStillDescribesRejectsARecycledSessionID(t *testing.T) {
	created := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	binding := TerminalBinding{SessionID: "7K3D", CreatedAt: created}
	same := resolvedSession{local: &Session{}}
	same.local.ID, same.local.CreatedAt = "7K3D", created
	if err := bindingStillDescribes(binding, same); err != nil {
		t.Fatalf("the same session was rejected: %v", err)
	}
	later := resolvedSession{local: &Session{}}
	later.local.ID, later.local.CreatedAt = "7K3D", created.Add(time.Hour)
	if err := bindingStillDescribes(binding, later); err == nil {
		t.Fatal("a different session reusing the id was accepted")
	}
	// A binding made before creation times were known, or from a stale picker
	// row, carries no timestamp and must not be rejected for it.
	if err := bindingStillDescribes(TerminalBinding{SessionID: "7K3D"}, later); err != nil {
		t.Fatalf("a binding with no recorded creation time was rejected: %v", err)
	}
}

// A stale picker row's timestamps come out of the cache, and a wrong creation
// time would later read as "a different session reusing the id".
func TestBindingForDropsTheTimestampOfAStaleRow(t *testing.T) {
	created := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	host := HostRecord{ID: "host-id", Alias: "pc"}
	fresh := bindingFor(resolvedSession{host: &host, remote: sessionInfoFor("7K3D", created)})
	if fresh.HostID != "host-id" || fresh.SessionID != "7K3D" || !fresh.CreatedAt.Equal(created) {
		t.Fatalf("binding = %#v", fresh)
	}
	stale := bindingFor(resolvedSession{host: &host, remote: sessionInfoFor("7K3D", created), stale: true})
	if !stale.CreatedAt.IsZero() {
		t.Fatalf("a stale row contributed a creation time of %s", stale.CreatedAt)
	}
	if stale.SessionID != "7K3D" {
		t.Fatalf("a stale row lost its session id: %#v", stale)
	}
}

func sessionInfoFor(id string, created time.Time) protocol.SessionInfo {
	return protocol.SessionInfo{ID: id, HostID: "host-id", CreatedAt: created, State: "running"}
}

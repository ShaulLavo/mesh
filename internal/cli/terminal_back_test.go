package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestBackResolvesTheRememberedHostDespiteALocalIDCollision(t *testing.T) {
	host := setupCommandTestHost(t)
	host.sessionID = "7K3D"
	dir, err := paths.SessionDir(host.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(dir, worker.Meta{ID: host.sessionID, State: worker.StateDetached,
		Command: []string{"bash"}, Cwd: "/work", CreatedAt: commandTestTime}); err != nil {
		t.Fatal(err)
	}
	app := application{dependencies: Dependencies{DialHost: host.dial}}
	binding := TerminalBinding{HostID: host.host.ID, SessionID: host.sessionID, CreatedAt: commandTestTime}
	resolved, err := app.resolveBinding(context.Background(), TerminalIdentity{Key: "tab"}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.local != nil || resolved.host == nil || resolved.host.ID != binding.HostID {
		t.Fatalf("binding resolved to the wrong host: %+v", resolved)
	}
}

func TestBackPreservesBindingWhenHostIsUnavailable(t *testing.T) {
	host := setupCommandTestHost(t)
	host.sessionID = "7K3D"
	binding := TerminalBinding{HostID: host.host.ID, SessionID: host.sessionID}
	if err := saveTerminalBinding("tab", binding); err != nil {
		t.Fatal(err)
	}
	app := application{dependencies: Dependencies{DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("host offline")
	}}}
	if _, err := app.resolveBinding(context.Background(), TerminalIdentity{Key: "tab"}, binding); err == nil {
		t.Fatal("offline host resolved successfully")
	}
	if _, found := loadTerminalBinding("tab"); !found {
		t.Fatal("connection failure erased the terminal binding")
	}
}

func TestBackDoesNotResolveALocalBindingOnARemoteHost(t *testing.T) {
	host := setupCommandTestHost(t)
	app := application{dependencies: Dependencies{DialHost: host.dial}}
	binding := TerminalBinding{SessionID: "7K3D"}
	if _, err := app.resolveBinding(context.Background(), TerminalIdentity{Key: "tab"}, binding); !errors.Is(err, ErrNoLocalSession) {
		t.Fatalf("missing local binding = %v, want local session not found", err)
	}
	if len(host.recorded()) != 0 {
		t.Fatal("local binding queried a remote host")
	}
}

func fakeTerminal(key string) TerminalFunc {
	return func() (TerminalIdentity, bool) {
		return TerminalIdentity{Key: key, Source: "test"}, true
	}
}

// A terminal Mesh cannot name must say so plainly. Guessing would send the tab
// to a session that belongs to a different one.
func TestBackExplainsAnUnidentifiableTerminal(t *testing.T) {
	setupCommandTestHost(t)
	_, _, err := executeCommand(t, Dependencies{
		Terminal: func() (TerminalIdentity, bool) { return TerminalIdentity{}, false },
	}, "back")
	if err == nil || !strings.Contains(err.Error(), "cannot identify this terminal") {
		t.Fatalf("back error = %v, want the unidentified-terminal explanation", err)
	}
}

func TestBackExplainsATerminalThatHasOpenedNothing(t *testing.T) {
	setupCommandTestHost(t)
	_, _, err := executeCommand(t, Dependencies{Terminal: fakeTerminal("fresh-tab")}, "back")
	if err == nil || !strings.Contains(err.Error(), "has not opened a Mesh session yet") {
		t.Fatalf("back error = %v, want the unbound-terminal explanation", err)
	}
}

// The binding is this terminal's memory; --forget is how a user drops it
// without needing to know where it is stored.
func TestBackForgetsThisTerminalsBinding(t *testing.T) {
	setupCommandTestHost(t)
	if err := saveTerminalBinding("bound-tab", TerminalBinding{SessionID: "7K3D", BoundAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeCommand(t, Dependencies{Terminal: fakeTerminal("bound-tab")}, "back", "--forget")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "no longer bound") {
		t.Fatalf("back --forget output = %q", stdout)
	}
	if _, found := loadTerminalBinding("bound-tab"); found {
		t.Fatal("the binding survived --forget")
	}
}

// Bare `mesh` has always been total and side-effect free. A returning tab is
// told where it was; it is not attached to anything without being asked.
func TestBareMeshPointsAReturningTerminalAtItsOwnSession(t *testing.T) {
	host := setupCommandTestHost(t)
	if err := saveTerminalBinding("bound-tab", TerminalBinding{
		SessionID: "7K3D", HostID: host.host.ID, BoundAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	opened := 0
	dependencies := Dependencies{
		Terminal: fakeTerminal("bound-tab"),
		Picker: func(context.Context, PickerInput) (PickerSelection, error) {
			opened++
			return PickerSelection{}, nil
		},
	}
	_, stderr, err := executeCommand(t, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 {
		t.Fatalf("picker opened %d times, want exactly 1 — bare mesh must still open it", opened)
	}
	if !strings.Contains(stderr, "7K3D") || !strings.Contains(stderr, "mesh back") {
		t.Fatalf("bare mesh stderr = %q, want it to name 7K3D and mesh back", stderr)
	}
}

// An unbound terminal must see exactly what it saw before this feature existed.
func TestBareMeshSaysNothingExtraForAnUnboundTerminal(t *testing.T) {
	setupCommandTestHost(t)
	_, stderr, err := executeCommand(t, Dependencies{
		Terminal: fakeTerminal("fresh-tab"),
		Picker: func(context.Context, PickerInput) (PickerSelection, error) {
			return PickerSelection{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "mesh back") {
		t.Fatalf("an unbound terminal was told about mesh back: %q", stderr)
	}
}

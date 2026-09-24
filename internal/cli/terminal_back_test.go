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
	clearTerminalEnvironment(t)
	_, _, err := executeCommand(t, Dependencies{
		Terminal: func() (TerminalIdentity, bool) { return TerminalIdentity{}, false },
	}, "back")
	if err == nil || !strings.Contains(err.Error(), "cannot identify this terminal") {
		t.Fatalf("back error = %v, want the unidentified-terminal explanation", err)
	}
}

func TestBackInsideASessionPointsAtTheOuterTerminal(t *testing.T) {
	setupCommandTestHost(t)
	clearTerminalEnvironment(t)
	insideSessionProcess = func() bool { return true }
	_, _, err := executeCommand(t, Dependencies{
		Terminal: func() (TerminalIdentity, bool) { return TerminalIdentity{}, false },
	}, "back")
	if !errors.Is(err, errTerminalInsideSession) {
		t.Fatalf("back error = %v, want the inside-a-session explanation", err)
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
	if !strings.Contains(stderr, "7K3D on pc") || !strings.Contains(stderr, "mesh back") {
		t.Fatalf("bare mesh stderr = %q, want it to name 7K3D on pc and mesh back", stderr)
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

// The binding has to survive the outage it exists for. A host that is down
// answers no query, and a session created by `mesh <host>` was never written to
// the local catalog cache, so a failed lookup is indistinguishable from a dead
// session. Discarding the record there would erase the tab's memory during
// exactly the crash it is meant to carry it through.
func TestBackKeepsTheBindingWhenTheSessionCannotBeReached(t *testing.T) {
	host := setupCommandTestHost(t)
	binding := TerminalBinding{SessionID: "7K3D", HostID: host.host.ID, BoundAt: time.Now()}
	if err := saveTerminalBinding("bound-tab", binding); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeCommand(t, Dependencies{
		Terminal: fakeTerminal("bound-tab"),
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host unavailable")
		},
	}, "back")
	if err == nil {
		t.Fatal("back succeeded against an unreachable host")
	}
	if !strings.Contains(err.Error(), "binding retained") {
		t.Fatalf("back error = %v, want it to say the binding is retained", err)
	}
	kept, found := loadTerminalBinding("bound-tab")
	if !found {
		t.Fatal("an unreachable host erased this terminal's binding")
	}
	if kept.SessionID != "7K3D" || kept.HostID != host.host.ID {
		t.Fatalf("binding = %#v, want it unchanged", kept)
	}
}

// A session reusing a retired id is the one case where forgetting is right: the
// terminal is demonstrably pointed at somebody else's session.
func TestBackForgetsABindingWhoseSessionWasReplacedByAnother(t *testing.T) {
	setupCommandTestHost(t)
	created := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	identity := TerminalIdentity{Key: "bound-tab", Source: "test"}
	binding := TerminalBinding{SessionID: "7K3D", CreatedAt: created, BoundAt: created}
	if err := saveTerminalBinding(identity.Key, binding); err != nil {
		t.Fatal(err)
	}
	later := resolvedSession{local: &Session{}}
	later.local.ID, later.local.CreatedAt = "7K3D", created.Add(time.Hour)
	if err := bindingStillDescribes(binding, later); err == nil {
		t.Fatal("a session reusing the id was accepted")
	}
	// resolveBinding is what actually drops it; prove the two agree.
	if _, err := (&application{dependencies: Dependencies{}}).resolveBinding(context.Background(), identity, binding); err == nil {
		t.Fatal("resolveBinding accepted a session it could not resolve without error")
	}
}

// A session is bound the moment it is created, before the host has reported
// anything about it, so the guard against a recycled id starts blank. It has to
// be filled in by the first lookup that can supply one, or it never guards.
func TestBackRemembersACreationTimeItCouldNotKnowAtBindTime(t *testing.T) {
	setupCommandTestHost(t)
	created := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	identity := TerminalIdentity{Key: "bound-tab", Source: "test"}
	binding := TerminalBinding{SessionID: "7K3D", BoundAt: created}
	if err := saveTerminalBinding(identity.Key, binding); err != nil {
		t.Fatal(err)
	}
	resolved := resolvedSession{local: &Session{}}
	resolved.local.ID, resolved.local.CreatedAt = "7K3D", created
	rememberCreation(identity, binding, resolved)
	upgraded, found := loadTerminalBinding(identity.Key)
	if !found || !upgraded.CreatedAt.Equal(created) {
		t.Fatalf("binding = %#v found=%t, want createdAt %s", upgraded, found, created)
	}
	// A stale catalog row's timestamps came out of the cache and must not be
	// promoted into the guard.
	stale := resolvedSession{remote: sessionInfoFor("9XYZ", created.Add(time.Hour)), stale: true}
	blank := TerminalBinding{SessionID: "9XYZ", BoundAt: created}
	if err := saveTerminalBinding("stale-tab", blank); err != nil {
		t.Fatal(err)
	}
	rememberCreation(TerminalIdentity{Key: "stale-tab"}, blank, stale)
	unchanged, _ := loadTerminalBinding("stale-tab")
	if !unchanged.CreatedAt.IsZero() {
		t.Fatalf("a stale row supplied a creation time of %s", unchanged.CreatedAt)
	}
}

// Telling someone to export a variable is useless advice when the refusal
// happened before any variable was read.
func TestBackInsideASessionNamesNestingRatherThanBlamingTheEnvironment(t *testing.T) {
	setupCommandTestHost(t)
	t.Setenv(worker.MeshSessionIDVariable, "7K3D")
	_, _, err := executeCommand(t, Dependencies{
		Terminal: func() (TerminalIdentity, bool) { return TerminalIdentity{}, false },
	}, "back")
	if err == nil {
		t.Fatal("back succeeded inside a Mesh session")
	}
	if strings.Contains(err.Error(), "MESH_TERMINAL_ID") {
		t.Fatalf("back blamed the environment inside a session: %v", err)
	}
	if !strings.Contains(err.Error(), "Mesh session") {
		t.Fatalf("back error = %v, want it to name nesting as the cause", err)
	}
}

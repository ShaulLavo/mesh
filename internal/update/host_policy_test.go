package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func reserveHostWithoutStage(t *testing.T, f *hostFixture) OwnedRequest {
	t.Helper()
	calls := 0
	f.updater.Engine = func(context.Context) (*updateinstall.Engine, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("daemon replaced before journal acceptance")
		}
		return f.engine, nil
	}
	if err := f.updater.Tick(t.Context()); err == nil {
		t.Fatal("did not interrupt before journal")
	}
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned == nil {
		t.Fatalf("ownership not durable: %+v %v", state, err)
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal already accepted: %v", err)
	}
	f.updater = f.newUpdater()
	f.now = f.now.Add(time.Hour)
	return *state.Owned
}

func TestHostOwnershipBeforeJournalRecoversSameRequest(t *testing.T) {
	f := newHostFixture(t)
	owned := reserveHostWithoutStage(t, f)
	f.tick()
	status := f.installation()
	if status.Request.ID != owned.Request || status.Request.Generation != owned.Generation || status.Phase != updateinstall.Granted {
		t.Fatalf("recovered another request: %+v", status)
	}
}

func TestHostOffClearsUnissuedOwnershipWithoutJournal(t *testing.T) {
	f := newHostFixture(t)
	reserveHostWithoutStage(t, f)
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned != nil {
		t.Fatalf("unissued ownership survived off: %+v %v", state, err)
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("off staged: %v", err)
	}
}

func TestHostAcceptanceMissingAndMalformedOwnershipNeverManual(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "wrong request", "off", "revision"} {
		t.Run(kind, func(t *testing.T) { testHostAcceptanceState(t, kind) })
	}
}

func testHostAcceptanceState(t *testing.T, kind string) {
	f := newHostFixture(t)
	owned := reserveHostWithoutStage(t, f)
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "missing":
		if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-update.json")); err != nil {
			t.Fatal(err)
		}
	case "malformed":
		if err := f.store.writeAuthority("host-update.json", map[string]any{"schema": 1, "phase": HostInstalling}); err != nil {
			t.Fatal(err)
		}
	case "wrong request":
		state.Owned.Request = strings.Repeat("a", 32)
		if err := f.store.writeAuthority("host-update.json", state); err != nil {
			t.Fatal(err)
		}
	case "off":
		if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
			t.Fatal(err)
		}
	case "revision":
		if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic}); err != nil {
			t.Fatal(err)
		}
	}
	engine := f.engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
	if _, err := engine.Stage(t.Context(), owned.Installation); err == nil {
		t.Fatal("host stage fell through to manual")
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected host request wrote journal: %v", err)
	}
}

func TestHostOffFencesRealStageRetryAndGrant(t *testing.T) {
	for _, kind := range []string{"stage", "retry", "grant"} {
		t.Run(kind, func(t *testing.T) { testHostAcceptanceRace(t, kind) })
	}
}

func testHostAcceptanceRace(t *testing.T, kind string) {
	f := newHostFixture(t)
	owned := reserveHostWithoutStage(t, f)
	engine := f.engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
	if kind != "stage" {
		if _, err := engine.Stage(t.Context(), owned.Installation); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "retry" {
		failHostBeforeActivation(t, f, engine, owned)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	acceptance := f.store.AuthorizeHostAcceptance(nil)
	engine = engine.WithAcceptance(func(ctx context.Context, request updateinstall.Request, phase updateinstall.AcceptanceKind, commit func() error) error {
		close(entered)
		<-release
		return acceptance(ctx, request, phase, commit)
	})
	done := make(chan error, 1)
	go func() { done <- callHostEngine(t.Context(), engine, owned, kind) }()
	<-entered
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("off failed to fence acceptance")
	}
	if kind == "stage" {
		if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("off allowed stage")
		}
		return
	}
	if status := f.installation(); status.Phase == updateinstall.Granted || status.Phase == updateinstall.Accepted {
		t.Fatalf("off accepted work: %s", status.Phase)
	}
}

func failHostBeforeActivation(t *testing.T, f *hostFixture, engine *updateinstall.Engine, owned OwnedRequest) {
	t.Helper()
	if _, err := engine.Grant(t.Context(), owned.Request, owned.Generation); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.settings.Executable, []byte("changed installation"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Run(t.Context()); err == nil {
		t.Fatal("changed installation did not fail")
	}
	if f.installation().Phase != updateinstall.Failed {
		t.Fatal("expected preactivation failure")
	}
	if err := os.WriteFile(f.settings.Executable, f.files["v0.1.1/image"], 0600); err != nil {
		t.Fatal(err)
	}
}

func callHostEngine(ctx context.Context, engine *updateinstall.Engine, owned OwnedRequest, kind string) error {
	var err error
	switch kind {
	case "stage":
		_, err = engine.Stage(ctx, owned.Installation)
	case "grant":
		_, err = engine.Grant(ctx, owned.Request, owned.Generation)
	case "retry":
		_, err = engine.Retry(ctx, owned.Request, owned.Generation)
	}
	return hostError(err)
}

func TestHostOffReturnsAfterAcceptanceCommit(t *testing.T) {
	f := newHostFixture(t)
	owned := reserveHostWithoutStage(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	authorize := f.store.AuthorizeHostAcceptance(nil)
	engine := f.engine.WithHostOrigin().WithAcceptance(func(ctx context.Context, request updateinstall.Request, kind updateinstall.AcceptanceKind, commit func() error) error {
		return authorize(ctx, request, kind, func() error { close(entered); <-release; return errors.New("interrupted commit") })
	})
	go func() { _, err := engine.Stage(t.Context(), owned.Installation); finished <- err }()
	<-entered
	off := make(chan error, 1)
	go func() { _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); off <- err }()
	select {
	case err := <-off:
		t.Fatalf("off overtook acceptance commit: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err == nil {
		t.Fatal("expected interrupted commit")
	}
	if err := <-off; err != nil {
		t.Fatal(err)
	}
}

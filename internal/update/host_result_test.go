package update

import (
	"errors"
	"reflect"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHostFinalizationDoesNotOverwriteHelperPublication(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := HostUpdate{Schema: 1, Phase: HostIdle, LastRequest: "completed", LastGeneration: 3, LastPhase: updateinstall.Committed}
	if err := store.writeAuthority("host-update.json", current); err != nil {
		t.Fatal(err)
	}
	stale := HostUpdate{Schema: 1, Phase: HostInstalling, Owned: &OwnedRequest{Request: "completed", Generation: 3}}
	u := &HostUpdater{Store: store}
	state, done, err := u.recordFinal(t.Context(), stale, updateinstall.Status{Phase: updateinstall.Committed})
	if err != nil || !done || !reflect.DeepEqual(state, current) {
		t.Fatalf("stale daemon tick did not yield to helper publication: %+v, %v, %v", state, done, err)
	}
	actual, err := store.HostState(t.Context())
	if err != nil || !reflect.DeepEqual(actual, current) {
		t.Fatalf("stale daemon tick overwrote helper publication: %+v, %v", actual, err)
	}
}

func TestHostFinalizationRejectsStaleDaemonWriters(t *testing.T) {
	for _, writer := range []string{"attention", "wait", "backoff", "save", "retire"} {
		t.Run(writer, func(t *testing.T) { checkStaleHostWriter(t, writer) })
	}
}

func checkStaleHostWriter(t *testing.T, writer string) {
	t.Helper()
	f := newHostFixture(t)
	f.tick()
	stale, err := f.store.HostState(t.Context())
	if err != nil || stale.Owned == nil {
		t.Fatalf("capture owned snapshot: %+v, %v", stale, err)
	}
	f.finish()
	if err := PublishHostResult(t.Context(), f.store.stateDir()); err != nil {
		t.Fatal(err)
	}
	published, err := f.store.HostState(t.Context())
	if err != nil || published.Owned != nil || published.LastPhase != updateinstall.Committed {
		t.Fatalf("helper publication: %+v, %v", published, err)
	}
	cause := errors.New("stale daemon operation failed")
	err = runStaleHostWriter(t, f.updater, stale, writer, cause)
	if err != nil && !errors.Is(err, cause) {
		t.Fatal(err)
	}
	actual, err := f.store.HostState(t.Context())
	if err != nil || !reflect.DeepEqual(actual, published) {
		t.Fatalf("%s overwrote helper publication: %+v, want %+v, error %v", writer, actual, published, err)
	}
}

func runStaleHostWriter(t *testing.T, u *HostUpdater, stale HostUpdate, writer string, cause error) error {
	t.Helper()
	switch writer {
	case "attention":
		return u.attention(t.Context(), stale, "policy", cause)
	case "wait":
		return u.wait(t.Context(), stale, "policy unavailable")
	case "backoff":
		return u.backoff(t.Context(), stale, cause)
	case "retire":
		owned := *stale.Owned
		stale.Owned, stale.Phase, stale.Problem = nil, HostIdle, nil
		return u.retireLocalTarget(t.Context(), stale, owned)
	default:
		return u.save(t.Context(), stale)
	}
}

func TestHostFinalizationStillAllowsCurrentOwnershipRetirement(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned == nil {
		t.Fatalf("capture owned snapshot: %+v, %v", state, err)
	}
	owned := *state.Owned
	state.Owned, state.Phase, state.Problem = nil, HostIdle, nil
	if err := f.updater.retireLocalTarget(t.Context(), state, owned); err != nil {
		t.Fatal(err)
	}
	actual, err := f.store.HostState(t.Context())
	if err != nil || actual.Owned != nil || actual.Phase != HostIdle {
		t.Fatalf("current ownership retirement did not persist: %+v, %v", actual, err)
	}
}

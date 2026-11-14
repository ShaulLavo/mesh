package update

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHostMissingPolicyDefaultsByInstallation(t *testing.T) {
	for _, kind := range []string{updateinstall.InstallationManaged, updateinstall.InstallationPackage, updateinstall.InstallationClient, updateinstall.InstallationDevelopment, updateinstall.InstallationUnknown, ""} {
		t.Run(kind, func(t *testing.T) { checkHostMissingPolicy(t, kind) })
	}
}

func checkHostMissingPolicy(t *testing.T, kind string) {
	t.Helper()
	state := t.TempDir()
	store, err := OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	store = store.WithHostInstallation(kind)
	policy, err := store.HostPolicy(t.Context())
	want := HostManual
	if kind == updateinstall.InstallationManaged {
		want = HostAutomatic
	}
	if err != nil || policy.Mode != want || policy.Revision != 0 || len(policy.After) != 0 {
		t.Fatalf("first-start policy = %+v, %v; want %s", policy, err, want)
	}
	if _, err := os.Stat(filepath.Join(state, "updates", "host-policy.json")); !os.IsNotExist(err) {
		t.Fatalf("reading default persisted a decision: %v", err)
	}
	manual, err := store.SetHostPolicy(t.Context(), 0, HostPolicy{Mode: HostManual})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.WithHostInstallation(updateinstall.InstallationManaged).HostPolicy(t.Context())
	if err != nil || !reflect.DeepEqual(got, manual) {
		t.Fatalf("persisted manual decision changed: %+v, %v", got, err)
	}
}

func TestHostFreshManagedDefaultIssuesFirstHop(t *testing.T) {
	f := newHostFixture(t)
	if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-policy.json")); err != nil {
		t.Fatal(err)
	}
	f.store = f.store.WithHostInstallation(updateinstall.InstallationManaged)
	f.updater = f.newUpdater()
	f.tick()
	status := f.installation()
	if status.Phase != updateinstall.Granted || f.helperCalls == 0 {
		t.Fatalf("first automatic tick did not verify helper and grant hop: %s, helper calls %d", status.Phase, f.helperCalls)
	}
	policy, err := f.store.HostPolicy(t.Context())
	if err != nil || policy.Revision != 0 || policy.Mode != HostAutomatic {
		t.Fatalf("automatic first hop overwrote missing policy: %+v, %v", policy, err)
	}
}

func TestHostFreshDefaultOffFencesUnissuedOwnership(t *testing.T) {
	f := newHostFixture(t)
	if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-policy.json")); err != nil {
		t.Fatal(err)
	}
	f.store = f.store.WithHostInstallation(updateinstall.InstallationManaged)
	f.updater = f.newUpdater()
	owned := reserveHostWithoutStage(t, f)
	if owned.PolicyRevision != 0 {
		t.Fatalf("fresh default ownership revision = %d", owned.PolicyRevision)
	}
	if _, err := f.store.SetHostPolicy(t.Context(), 0, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	engine := f.engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
	if _, err := engine.Stage(t.Context(), owned.Installation); err == nil {
		t.Fatal("off did not fence revision-zero ownership")
	}
	f.tick()
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned != nil {
		t.Fatalf("off retained unissued ownership: %+v, %v", state, err)
	}
}

package update

import (
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

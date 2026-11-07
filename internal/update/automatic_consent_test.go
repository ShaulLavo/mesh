package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestAutomaticConsentRevocationFencesRealAcceptanceAndReplay(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fleet := testFleet(t, 2)
	coordinator := fleet.Members[0].ID
	local := fleet.Members[1].ID
	consent, err := store.SetRecipientConsent(context.Background(), 0, ConsentChange{LocalHostID: local, CoordinatorID: coordinator, Fleet: fleet, Epoch: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	actor := VerifiedActor{id: coordinator}
	request := updateinstall.Request{TargetID: local, Automatic: &updateinstall.AutomaticReference{PolicyID: mustAutomaticID(t), CoordinatorID: coordinator, Membership: fleet.Digest(), Epoch: 7, ConsentRevision: consent.Revision}}
	acceptance := store.AuthorizeAcceptance(actor, func(string) bool { return true })
	commits := 0
	commit := func() error { commits++; return nil }
	if err := acceptance(context.Background(), request, updateinstall.AcceptanceGrant, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecipientConsent(context.Background(), consent.Revision, ConsentChange{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []updateinstall.AcceptanceKind{updateinstall.AcceptanceStage, updateinstall.AcceptanceGrant, updateinstall.AcceptanceRetry} {
		if err := acceptance(context.Background(), request, kind, commit); err == nil {
			t.Fatalf("revoked %s was accepted", kind)
		}
	}
	if commits != 1 {
		t.Fatalf("revocation permitted commits: %d", commits)
	}
	if _, err := store.SetRecipientConsent(context.Background(), 2, ConsentChange{LocalHostID: local, CoordinatorID: coordinator, Fleet: fleet, Epoch: 7, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := acceptance(context.Background(), request, updateinstall.AcceptanceGrant, commit); err == nil {
		t.Fatal("re-allow accepted delayed old consent revision")
	}
}
func TestAutomaticConsentLinearizesRevokeWithAcceptance(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fleet := testFleet(t, 1)
	host := fleet.Members[0].ID
	consent, err := store.SetRecipientConsent(context.Background(), 0, ConsentChange{LocalHostID: host, CoordinatorID: host, Fleet: fleet, Epoch: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := updateinstall.Request{TargetID: host, Automatic: &updateinstall.AutomaticReference{PolicyID: mustAutomaticID(t), CoordinatorID: host, Membership: fleet.Digest(), Epoch: 1, ConsentRevision: 1}}
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	accepted := make(chan error, 1)
	go func() {
		accepted <- store.AuthorizeAcceptance(VerifiedActor{id: host}, func(string) bool { return true })(context.Background(), request, updateinstall.AcceptanceGrant, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	revoked := make(chan error, 1)
	go func() {
		_, err := store.SetRecipientConsent(context.Background(), consent.Revision, ConsentChange{})
		revoked <- err
	}()
	select {
	case err := <-revoked:
		t.Fatalf("revoke passed active acceptance: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
}
func TestAutomaticAuthorityRejectsUnsafeAndDamagedState(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "permissions", "duplicate", "unknown", "oversize"} {
		t.Run(kind, func(t *testing.T) { testAutomaticUnsafe(t, kind) })
	}
}
func testAutomaticUnsafe(t *testing.T, kind string) {
	c, _, _ := automaticFixture(t, 1)
	root, err := os.OpenRoot(filepath.Dir(c.Store.directory))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	path := "automatic.json"
	data, err := root.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "symlink":
		original := path + ".original"
		if err := root.Rename(path, original); err != nil {
			t.Fatal(err)
		}
		if err := root.Symlink(original, path); err != nil {
			t.Fatal(err)
		}
	case "hardlink":
		if err := root.Link(path, path+".link"); err != nil {
			t.Fatal(err)
		}
	case "permissions":
		if err := root.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	case "duplicate":
		data = append([]byte(`{"schema":1,`), data[1:]...)
		writeAutomaticCorrupt(t, root, path, data)
	case "unknown":
		data = append([]byte(`{"owner":"claimed",`), data[1:]...)
		writeAutomaticCorrupt(t, root, path, data)
	case "oversize":
		writeAutomaticCorrupt(t, root, path, make([]byte, maximumMessage+1))
	}
	status, err := c.Store.ReadAutomatic(context.Background())
	if err == nil || status.Revision != nil {
		t.Fatalf("damaged authority became absence: %+v %v", status, err)
	}
	if _, err := root.Lstat(path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("damaged authority was deleted")
	}
}
func writeAutomaticCorrupt(t *testing.T, root *os.Root, path string, data []byte) {
	t.Helper()
	if err := root.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

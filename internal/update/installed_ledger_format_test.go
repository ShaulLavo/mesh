//go:build mesh_installed_fixture

package update

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func installedOwnedState(schema int) HostUpdate {
	current := release.Build{Version: "v0.1.9001", Commit: strings.Repeat("a", 40), Digest: strings.Repeat("a", 64),
		Platform: release.Platform{OS: "linux", Arch: "amd64"}, StateVersion: 16, WorkerProtocol: 1, UpdateProtocol: 1}
	target := current
	target.Version, target.Digest = "v0.1.9003", strings.Repeat("b", 64)
	manifest := readerFixtureManifest(target)
	manifest.Schema = schema
	request := updateinstall.Request{ID: strings.Repeat("e", 32), Generation: 1, Current: current, Manifest: manifest}
	return HostUpdate{Schema: 1, Phase: HostInstalling, Owned: &OwnedRequest{Request: request.ID, Generation: 1,
		Edge: Edge{From: current.Digest, To: target.Digest, Platform: current.Platform}, Installation: request,
		Metadata: strings.Repeat("c", 64), Epoch: 2, Position: release.Position{Anchor: true, Ordinal: 1},
		Settings: updateinstall.Settings{StateDir: "/fixture/state", CacheDir: "/fixture/cache", Executable: "/fixture/mesh"}}}
}

func installedLedgerStore(t *testing.T, state HostUpdate) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeAuthority("host-update.json", state); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestInstalledLedgerManifestCapability(t *testing.T) {
	for _, schema := range []int{1, 2} {
		checkInstalledLedgerSchema(t, schema)
	}
}

func TestInstalledLedgerUnsupportedManifest(t *testing.T) {
	store := installedLedgerStore(t, installedOwnedState(3))
	assertInstalledLedgerError(t, store, "schema 3", "manifest schema 3 is unsupported")
}

func checkInstalledLedgerSchema(t *testing.T, schema int) {
	t.Helper()
	state := installedOwnedState(schema)
	store := installedLedgerStore(t, state)
	value, err := installedReadLedger(t.Context(), filepath.Dir(filepath.Dir(store.directory)))
	future := os.Getenv("MESH_INSTALLED_TEST_FUTURE_MANIFEST") == "1"
	if schema == 2 && !future {
		if err == nil || !strings.Contains(err.Error(), "manifest schema 2 is unsupported") || value != nil {
			t.Fatalf("strict exporter accepted future ownership: %v %v", value, err)
		}
		return
	}
	if err != nil || !reflect.DeepEqual(value["state"], state) || value["observer"] != "fixture-exporter" {
		t.Fatalf("exporter changed or rejected owned schema %d: %v %v", schema, value, err)
	}
}

func TestInstalledLedgerOwnershipValidation(t *testing.T) {
	for _, schema := range []int{1, 2} {
		if schema == 2 && os.Getenv("MESH_INSTALLED_TEST_FUTURE_MANIFEST") != "1" {
			continue
		}
		checkInstalledLedgerOwnership(t, schema)
	}
}

func checkInstalledLedgerOwnership(t *testing.T, schema int) {
	t.Helper()
	cases := []struct {
		name   string
		mutate func(*OwnedRequest)
		want   string
	}{
		{"edge", func(o *OwnedRequest) { o.Edge.To = strings.Repeat("d", 64) }, "request differs"},
		{"artifact", func(o *OwnedRequest) { o.Installation.Manifest.Artifacts[0].BinarySHA256 = "bad" }, "invalid SHA-256"},
		{"platform", func(o *OwnedRequest) { o.Installation.Current.Platform.Arch = "unknown" }, "no artifact"},
		{"request", func(o *OwnedRequest) { o.Request = strings.Repeat("d", 32) }, "request differs"},
		{"settings", func(o *OwnedRequest) { o.Settings.Executable = "" }, "installation settings"},
	}
	for _, item := range cases {
		state := installedOwnedState(schema)
		item.mutate(state.Owned)
		store := installedLedgerStore(t, state)
		assertInstalledLedgerError(t, store, item.name, item.want)
	}
}

func assertInstalledLedgerError(t *testing.T, store *Store, name, want string) {
	t.Helper()
	value, err := installedReadLedger(t.Context(), filepath.Dir(filepath.Dir(store.directory)))
	if err == nil || !strings.Contains(err.Error(), want) || value != nil {
		t.Fatalf("%s did not fail closed with %q: %v %v", name, want, value, err)
	}
}

func TestInstalledLedgerAuthorityValidation(t *testing.T) {
	valid := mustHostJSON(installedOwnedState(1))
	cases := []struct {
		name string
		data []byte
		mode os.FileMode
		want string
	}{
		{"unknown", bytes.Replace(valid, []byte(`"schema":1`), []byte(`"unknown":true,"schema":1`), 1), 0600, "unknown field"},
		{"nested unknown", bytes.Replace(valid, []byte(`"generation":1`), []byte(`"unknown":true,"generation":1`), 1), 0600, "unknown field"},
		{"oversize", bytes.Repeat([]byte(" "), maximumMessage+1), 0600, "size limit"},
		{"malformed", []byte(`{"schema":`), 0600, "JSON"},
		{"trailing", append(bytes.Clone(valid), []byte(` {}`)...), 0600, "JSON"},
		{"private", valid, 0644, "private, owned and regular"},
	}
	for _, item := range cases {
		store := installedLedgerStore(t, installedOwnedState(1))
		path := filepath.Join(filepath.Dir(store.directory), "host-update.json")
		if err := os.WriteFile(path, item.data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, item.mode); err != nil {
			t.Fatal(err)
		}
		assertInstalledLedgerError(t, store, item.name, item.want)
	}
}

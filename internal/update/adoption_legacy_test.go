package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
)

// The installed-service scenarios live in the native catch-up gate. This
// control runs the original published image and exercises its real authority
// and source probe without creating a journal for an installation that has none.
func TestAdoptionLegacyOwnIdentity(t *testing.T) {
	if os.Getenv("MESH_ADOPTION_NATIVE") != "1" || runtime.GOOS != "linux" {
		t.Skip("set MESH_ADOPTION_NATIVE=1 on Linux for the published daemon control")
	}
	client := release.Client{}
	manifest, err := client.Manifest(t.Context(), "v0.1.249")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := client.Download(t.Context(), manifest, release.CurrentPlatform(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := startAdoptionLegacy(t, binary)
	observed, err := updatebootstrap.Inspect(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := manifest.Artifact(release.CurrentPlatform())
	if err != nil || observed.Health.Build.Digest != artifact.BinarySHA256 || observed.Health.Build.Version != manifest.Version {
		t.Fatalf("actual source differs from published image: %+v %v", observed.Health.Build, err)
	}
	host, key, err := identity.LoadPrivate(state)
	if err != nil {
		t.Fatal(err)
	}
	local := LocalHost(state, host.ID)
	var info Info
	if err := (Client{ID: host.ID, Key: key}).Call(t.Context(), local, "info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Health.Build.Digest != artifact.BinarySHA256 || info.Installation != nil {
		t.Fatal("old signed info invented an installation journal or lost its exact source")
	}
	other, otherKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := (Client{ID: other.ID, Key: otherKey}).Call(t.Context(), local, "info", nil, &Info{}); err == nil {
		t.Fatal("old daemon accepted another local identity")
	}
	checkLegacyAdoptionJournalHold(t, state, observed.Health.Build)
}

func startAdoptionLegacy(t *testing.T, binary string) string {
	t.Helper()
	state, err := os.MkdirTemp("", "a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(state); err != nil {
			t.Error(err)
		}
	})
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	log, err := root.Create("daemon.log")
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Path: binary, Args: []string{binary, "daemon", "--tailnet-port=0", "--ssh-port=0"}}
	command.Env = append(os.Environ(), "MESH_STATE_DIR="+state, "MESH_CONFIG_DIR="+filepath.Join(state, "config"))
	command.Stdout, command.Stderr = log, log
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(state, "daemon.sock")); err == nil {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("original published daemon did not open its isolated socket")
	return ""
}

func checkLegacyAdoptionJournalHold(t *testing.T, state string, current release.Build) {
	t.Helper()
	signer := newSigningFixture(t)
	side := buildOriginalHostReader(t, signer, HostReaderEpoch, "v0.1.400")
	build := originalReaderCapabilities(t, side).Build
	packet := originalReaderPacket(t, signer, HostReaderCapabilities{Epoch: HostReaderEpoch, Build: current}, build, false, nil)
	var target release.Manifest
	if err := decodeAuthority(packet.Target.Manifest, &target); err != nil {
		t.Fatal(err)
	}
	target.Compatibility.StateReadMin = current.StateVersion
	var metadata release.UpdateMetadata
	if err := decodeAuthority(packet.Target.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	documents := signedReaderDocuments(t, signer, target, HostReaderEpoch, metadata.Position, metadata.Updater, metadata.Rollback)
	server := adoptionLegacyOrigin(t, documents, target.Version, packet)
	supervisor, err := NewLocalAdoptionSupervisor(state, side, HostDocuments{Trust: signer.trust, Client: release.Client{BaseURL: server.URL, HTTPClient: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Build = build
	err = supervisor.Start(t.Context(), AdoptionStart{Version: build.Version, Policy: HostPolicy{Mode: HostAutomatic}})
	if err == nil || !strings.Contains(err.Error(), "installation settings") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy missing journal did not block explicitly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "updates", "host-adoption.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing legacy journal produced a fabricated adoption record")
	}
	observed, err := updatebootstrap.Inspect(t.Context(), state)
	if err != nil || observed.Health.Build.Digest != current.Digest {
		t.Fatal("journal refusal changed the running legacy daemon")
	}
}

func adoptionLegacyOrigin(t *testing.T, documents HostReaderDocuments, version string, packet HostReaderInput) *httptest.Server {
	t.Helper()
	var metadata release.UpdateMetadata
	if err := decodeAuthority(documents.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{release.UpdateMetadataAssetName: documents.Metadata, "mesh-update.sig.json": documents.Envelope, release.ManifestAssetName: documents.Manifest,
		metadata.Page.Name: documents.Page, discoveryDigest(packet.Forward) + ".json": packet.Forward, discoveryDigest(packet.Rollback) + ".rollback.json": packet.Rollback}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/"+version+"/")
		data, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return server
}

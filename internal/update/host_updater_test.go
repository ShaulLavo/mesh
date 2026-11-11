package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type hostFixture struct {
	t            *testing.T
	store        *Store
	updater      *HostUpdater
	engine       *updateinstall.Engine
	settings     updateinstall.Config
	documents    HostDocuments
	files        map[string][]byte
	builds       map[string]release.Build
	manifests    map[string]release.Manifest
	metadata     map[string]release.UpdateMetadata
	signer       *signingFixture
	current      release.Build
	workers      []updateinstall.Worker
	fail         string
	failRollback bool
	helperErr    error
	helperCalls  int
	stops        int
	starts       int
	now          time.Time
	worker       *exec.Cmd
	input        io.WriteCloser
	output       io.ReadCloser
}

type blockedHostArchive struct {
	base    http.RoundTripper
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (b *blockedHostArchive) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, ".tar.gz") {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.resume:
		case <-request.Context().Done():
			return nil, hostError(request.Context().Err())
		}
	}
	response, err := b.base.RoundTrip(request)
	return response, hostError(err)
}

func TestHostOffDuringArchiveDownloadRefusesStagedCommit(t *testing.T) {
	f := newHostFixture(t)
	owned := reserveHostWithoutStage(t, f)
	client := *f.documents.Client.HTTPClient
	blocked := &blockedHostArchive{base: client.Transport, entered: make(chan struct{}), resume: make(chan struct{})}
	release := sync.OnceFunc(func() { close(blocked.resume) })
	t.Cleanup(release)
	client.Transport = blocked
	f.settings.Client.HTTPClient = &client
	engine, err := updateinstall.New(f.settings)
	if err != nil {
		t.Fatal(err)
	}
	engine = engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
	done := make(chan error, 1)
	go func() { _, err := engine.Stage(t.Context(), owned.Installation); done <- err }()
	<-blocked.entered
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err == nil {
		t.Fatal("off during archive download accepted staged commit")
	}
	if got := f.installation(); got.Phase != updateinstall.Accepted {
		t.Fatalf("download crossed off fence: %s", got.Phase)
	}
	f.tick()
	if got := f.installation(); got.Phase != updateinstall.Cancelled {
		t.Fatalf("off did not retire ungranted journal: %s", got.Phase)
	}
}

func hostImage(build release.Build) []byte {
	return fmt.Appendf(nil, "#!/bin/sh\nif [ \"$1\" = version ]; then\nprintf '%%s\\n' '%s'\nexit 0\nfi\nexit 0\n", mustHostJSON(build))
}

func mustHostJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func hostArchive(t *testing.T, image []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "mesh", Mode: 0755, Size: int64(len(image))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	f := &hostFixture{t: t, files: make(map[string][]byte), builds: make(map[string]release.Build), manifests: make(map[string]release.Manifest), metadata: make(map[string]release.UpdateMetadata), signer: newSigningFixture(t), now: time.Now().UTC()}
	root := t.TempDir()
	var err error
	f.store, err = OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	f.addRelease(1, nil)
	f.addRelease(2, []uint64{1})
	f.addRelease(3, []uint64{2})
	f.current = f.builds["v0.1.1"]
	f.startWorker()
	executable := filepath.Join(root, "mesh")
	if err := os.WriteFile(executable, f.files["v0.1.1/image"], 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/download/")
		if r.URL.Path == "/latest/download/"+release.DiscoveryRootAssetName {
			key = "latest/" + release.DiscoveryRootAssetName
		}
		data, ok := f.files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	f.documents = HostDocuments{Client: release.Client{BaseURL: server.URL, HTTPClient: server.Client()}, Trust: f.signer.trust}
	f.settings = updateinstall.Config{StateDir: f.store.stateDir(), Executable: executable, CacheDir: filepath.Join(root, "cache"), Client: f.documents.Client, Probe: f.probe, Service: f,
		ServiceSpec: updateinstall.ServiceSpec{Kind: "systemd", Name: "fixture-daemon.service"}, HealthTimeout: 100 * time.Millisecond}
	f.engine, err = updateinstall.New(f.settings)
	if err != nil {
		t.Fatal(err)
	}
	f.updater = f.newUpdater()
	if _, err := f.store.SetHostPolicy(t.Context(), 0, HostPolicy{Mode: HostAutomatic}); err != nil {
		t.Fatal(err)
	}
	f.head(3)
	return f
}

func (f *hostFixture) startWorker() {
	f.worker = exec.Command("cat")
	f.t.Cleanup(func() {
		if f.worker.Process != nil {
			_ = f.worker.Process.Kill()
			_ = f.worker.Wait()
		}
	})
	var err error
	f.input, err = f.worker.StdinPipe()
	if err != nil {
		f.t.Fatal(err)
	}
	f.output, err = f.worker.StdoutPipe()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.worker.Start(); err != nil {
		f.t.Fatal(err)
	}
	build := f.current
	f.workers = []updateinstall.Worker{{ID: "worker", PID: f.worker.Process.Pid, ShellPID: f.worker.Process.Pid, Protocol: 1, Build: &build}}
}

func (f *hostFixture) newUpdater() *HostUpdater {
	return &HostUpdater{Store: f.store, ID: "host", Current: func() release.Build { return f.current }, Probe: f.probe, Now: func() time.Time { return f.now },
		Engine: func(context.Context) (*updateinstall.Engine, error) { return f.engine, nil },
		Source: func(ctx context.Context, current release.Build) (HostSource, release.DocumentSource, error) {
			source, err := f.documents.Current(ctx, current)
			return source, f.documents.Source(), err
		}, Helper: func(context.Context, release.Manifest) error { f.helperCalls++; return f.helperErr }}
}

func (f *hostFixture) Stop(context.Context) error {
	f.stops++
	if f.failRollback && f.stops > 1 {
		return errors.New("rollback service failed")
	}
	return nil
}

func (f *hostFixture) Start(context.Context) error { f.starts++; return nil }

func (f *hostFixture) probe(context.Context) (updateinstall.Health, error) {
	image, err := os.ReadFile(f.settings.Executable)
	if err != nil {
		return updateinstall.Health{}, hostError(err)
	}
	build, ok := f.builds[discoveryDigest(image)]
	if !ok {
		return updateinstall.Health{}, errors.New("unknown installed image")
	}
	if build.Version == f.fail {
		return updateinstall.Health{}, errors.New("unhealthy candidate")
	}
	return updateinstall.Health{HostID: "host", Build: build, Workers: f.workers}, nil
}

func (f *hostFixture) addRelease(number uint64, sources []uint64) {
	if number < 1 || number > 64 {
		f.t.Fatal("fixture release number outside first page")
	}
	version := fmt.Sprintf("v0.1.%d", number)
	build := release.Build{Version: version, Commit: strings.Repeat("a", 40), Platform: release.CurrentPlatform(), StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1}
	image := hostImage(build)
	build.Digest = discoveryDigest(image)
	f.builds[version], f.builds[build.Digest] = build, build
	f.files[version+"/image"] = image
	archive := hostArchive(f.t, image)
	manifest := release.Manifest{Schema: 1, Version: version, Commit: build.Commit, Compatibility: release.Compatibility{StateReadMin: 7, StateReadMax: 7, StateWrite: 7, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: discoveryDigest(archive), BinarySHA256: build.Digest})
		f.files[version+"/mesh_"+platform.OS+"_"+platform.Arch+".tar.gz"] = archive
	}
	metadata := release.UpdateMetadata{Schema: 1, Epoch: 1, Trust: 1, Version: version, PublishedAt: f.now.Add(time.Duration(len(f.metadata)+1) * time.Minute), Position: release.Position{Anchor: true, Ordinal: number - 1}}
	for _, source := range sources {
		f.addProofs(&manifest, &metadata, f.builds[fmt.Sprintf("v0.1.%d", source)])
	}
	f.files[version+"/"+release.ManifestAssetName] = mustHostJSON(manifest)
	metadata.Manifest = discoveryDigest(f.files[version+"/"+release.ManifestAssetName])
	entries := []release.AnchorEntry{}
	for previous := uint64(1); previous < number; previous++ {
		entries = append(entries, f.anchor(previous))
	}
	page, err := release.BuildAnchorPage(1, 0, entries, nil, version)
	if err != nil {
		f.t.Fatal(err)
	}
	pageData, err := release.EncodeAnchorPage(page, version)
	if err != nil {
		f.t.Fatal(err)
	}
	pageName := discoveryDigest(pageData) + ".anchors.json"
	metadata.Page = release.AssetRef{Tag: version, Name: pageName, SHA256: discoveryDigest(pageData)}
	f.files[version+"/"+pageName] = pageData
	metadata.Direct.AnchorsFrom = 0
	if len(sources) > 0 {
		metadata.Direct.AnchorsFrom = sources[0] - 1
	}
	f.manifests[version], f.metadata[version] = manifest, metadata
	f.signMetadata(version)
}

func (f *hostFixture) addProofs(manifest *release.Manifest, metadata *release.UpdateMetadata, source release.Build) {
	for _, artifact := range manifest.Artifacts {
		proof := release.TransitionReceipt{Schema: 1, Platform: artifact.Platform, FromDigest: source.Digest, ToDigest: artifact.BinarySHA256, StateReadMin: 7, StateReadMax: 7, StateWrite: 7,
			WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1, CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true}
		forward := mustHostJSON(proof)
		transition := release.Transition{FromDigest: source.Digest, ToDigest: artifact.BinarySHA256, Platform: artifact.Platform, Proof: discoveryDigest(forward)}
		manifest.Compatibility.Transitions = append(manifest.Compatibility.Transitions, transition)
		f.files[manifest.Version+"/"+transition.Proof+".json"] = forward
		rollback := release.RollbackProof{Schema: 1, Platform: artifact.Platform, FromDigest: source.Digest, ToDigest: artifact.BinarySHA256, Forward: transition.Proof,
			Boundaries: []release.RollbackBoundary{{State: 7, CandidateWrote: true, SourceStarted: true, SessionIO: true, RecoveryPreserved: true}}, Workers: []string{f.builds["v0.1.1"].Digest}}
		if rollback.Workers[0] != source.Digest {
			rollback.Workers = append(rollback.Workers, source.Digest)
		}
		data := mustHostJSON(rollback)
		metadata.Rollback = append(metadata.Rollback, release.RollbackRef{Forward: transition.Proof, Rollback: discoveryDigest(data)})
		f.files[manifest.Version+"/"+discoveryDigest(data)+".rollback.json"] = data
	}
}

func (f *hostFixture) anchor(number uint64) release.AnchorEntry {
	if number < 1 || number > 64 {
		f.t.Fatal("fixture anchor number outside first page")
	}
	version := fmt.Sprintf("v0.1.%d", number)
	metadata := f.metadata[version]
	return release.AnchorEntry{Ordinal: number - 1, Version: version, Manifest: metadata.Manifest, Metadata: discoveryDigest(f.files[version+"/"+release.UpdateMetadataAssetName]), AnchorsFrom: metadata.Direct.AnchorsFrom, PublishedAt: metadata.PublishedAt}
}

func (f *hostFixture) signMetadata(version string) {
	metadata := f.metadata[version]
	metadata.Updater = &release.UpdaterReader{Schema: 1, Epoch: 1, Position: metadata.Position}
	data := mustHostJSON(metadata)
	f.files[version+"/"+release.UpdateMetadataAssetName] = data
	f.files[version+"/mesh-update.sig.json"] = f.signer.envelope(f.t, data)
}

func (f *hostFixture) head(number uint64) {
	version := fmt.Sprintf("v0.1.%d", number)
	root := release.DiscoveryRoot{Format: 1, Epoch: 1, Head: release.AssetRef{Tag: version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(f.files[version+"/"+release.UpdateMetadataAssetName])}}
	data, err := release.EncodeDiscoveryRoot(root)
	if err != nil {
		f.t.Fatal(err)
	}
	f.files["latest/"+release.DiscoveryRootAssetName] = data
}

func (f *hostFixture) tick() {
	f.t.Helper()
	if err := f.updater.Tick(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hostFixture) installation() updateinstall.Status {
	f.t.Helper()
	status, err := f.engine.Read()
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *hostFixture) finish() {
	f.t.Helper()
	status, err := f.engine.Run(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	if status.Phase == updateinstall.Committed {
		f.current = status.Verified.Build
	}
}

func (f *hostFixture) roundTrip() {
	f.t.Helper()
	const message = "worker survives\n"
	if _, err := io.WriteString(f.input, message); err != nil {
		f.t.Fatal(err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(f.output, got); err != nil || string(got) != message {
		f.t.Fatalf("worker I/O: %q %v", got, err)
	}
}

func TestHostUpdaterTwoHopsAndRestartPreserveWorker(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	if status := f.installation(); status.Phase != updateinstall.Granted || status.Request.Manifest.Version != "v0.1.2" {
		t.Fatalf("first hop: %+v", status)
	}
	f.finish()
	f.roundTrip()
	f.updater = f.newUpdater()
	f.tick()
	if status := f.installation(); status.Phase != updateinstall.Granted || status.Request.Manifest.Version != "v0.1.3" {
		t.Fatalf("second hop: %+v", status)
	}
	f.finish()
	f.tick()
	f.roundTrip()
	entries, more, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || more || len(entries) != 2 || entries[0].WorkersPreserved != 1 || entries[1].WorkersPreserved != 1 {
		t.Fatalf("ledger: %+v %v %v", entries, more, err)
	}
}

func TestHostUpdaterDefaultManualAndForeignJournalYield(t *testing.T) {
	f := newHostFixture(t)
	if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-policy.json")); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default staged: %v", err)
	}
	if f.helperCalls != 0 {
		t.Fatal("manual touched helper")
	}
	if _, err := f.store.SetHostPolicy(t.Context(), 0, HostPolicy{Mode: HostAutomatic}); err != nil {
		t.Fatal(err)
	}
	request := updateinstall.Request{ID: "foreign", TargetID: "host", Generation: 1, Current: f.current, Manifest: f.manifests["v0.1.2"]}
	if _, err := f.engine.Stage(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if status := f.installation(); status.Request.ID != "foreign" || status.Phase != updateinstall.Staged {
		t.Fatalf("foreign journal changed: %+v", status)
	}
}

func TestHostUpdaterManualReconcilesGrantedResultIdempotently(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	f.finish()
	f.updater = f.newUpdater()
	f.tick()
	f.tick()
	entries, _, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 1 || entries[0].Origin != "host" {
		t.Fatalf("manual reconciliation: %+v %v", entries, err)
	}
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned != nil {
		t.Fatalf("ownership not cleared: %+v %v", state, err)
	}
}

func TestHostUpdaterRejectsUnsignedAndIncompleteRollback(t *testing.T) {
	for _, kind := range []string{"unsigned", "wrong signer", "missing rollback", "missing boundary", "old worker", "unproved worker"} {
		t.Run(kind, func(t *testing.T) { testHostInvalidProof(t, kind) })
	}
}

func testHostInvalidProof(t *testing.T, kind string) {
	f := newHostFixture(t)
	f.head(2)
	version := "v0.1.2"
	switch kind {
	case "unsigned":
		delete(f.files, version+"/mesh-update.sig.json")
	case "wrong signer":
		f.documents.Trust = release.EmbeddedTrustRoot()
	case "missing rollback":
		metadata := f.metadata[version]
		metadata.Rollback = nil
		f.metadata[version] = metadata
		f.signMetadata(version)
		f.head(2)
	case "missing boundary":
		removeHostBoundary(t, f, version)
	case "old worker":
		f.workers[0].Build = nil
	case "unproved worker":
		f.workers[0].Build.Digest = strings.Repeat("c", 64)
	}
	if err := f.updater.Tick(t.Context()); err == nil {
		t.Fatal("invalid proof allowed")
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid proof staged: %v", err)
	}
	if f.stops != 0 {
		t.Fatal("preactivation failure stopped daemon")
	}
}

func removeHostBoundary(t *testing.T, f *hostFixture, version string) {
	t.Helper()
	metadata := f.metadata[version]
	for i, ref := range metadata.Rollback {
		data := f.files[version+"/"+ref.Rollback+".rollback.json"]
		var proof release.RollbackProof
		if err := json.Unmarshal(data, &proof); err != nil {
			t.Fatal(err)
		}
		proof.Boundaries = nil
		data = mustHostJSON(proof)
		metadata.Rollback[i].Rollback = discoveryDigest(data)
		f.files[version+"/"+discoveryDigest(data)+".rollback.json"] = data
	}
	f.metadata[version] = metadata
	f.signMetadata(version)
	f.head(2)
}

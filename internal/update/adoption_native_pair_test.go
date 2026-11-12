package update

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestAdoptionNativePair(t *testing.T) {
	if os.Getenv("MESH_ADOPTION_NATIVE") != "1" || runtime.GOOS != "linux" {
		t.Skip("set MESH_ADOPTION_NATIVE=1 for published-daemon adoption fixtures")
	}
	f := newAdoptionNativeFixture(t, "v0.1.248", true)
	f.seedGenuinePrior()
	side, pin := f.publishCandidate()
	f.startOnce(side, pin)
	envelope := f.awaitAdoption(AdoptionComplete)
	if !envelope.Confirmed || envelope.Adopted == nil || envelope.Adopted.Metadata != pin || envelope.Adopted.WorkersPreserved != len(f.workers) {
		t.Fatalf("native adoption lacked its actual pinned commit: %+v", envelope)
	}
	observed := f.awaitDaemon()
	if observed.Health.Build.Version != "v0.1.400" {
		t.Fatal("helper did not install the candidate")
	}
	for _, worker := range f.workers {
		if !hostWorkerPreserved(worker, observed.Health.Workers) {
			t.Fatal("native adoption changed retained worker identity")
		}
	}
	f.roundTrip()
}

func TestAdoptionNativeInitialHelper(t *testing.T) {
	if os.Getenv("MESH_ADOPTION_NATIVE") != "1" || runtime.GOOS != "linux" {
		t.Skip("set MESH_ADOPTION_NATIVE=1 for unjournaled helper fixtures")
	}
	for _, helper := range []bool{false, true} {
		t.Run(strconv.FormatBool(helper), func(t *testing.T) {
			f := newAdoptionNativeFixture(t, "v0.1.249", helper)
			if _, err := updateinstall.Read(f.state); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fixture unexpectedly has a journal before adoption")
			}
			side, pin := f.publishCandidate()
			f.startOnce(side, pin)
			envelope := f.awaitAdoption(AdoptionComplete)
			if !envelope.Confirmed || envelope.Adopted.Generation != 1 {
				t.Fatal("initial helper did not create a genuine first adoption journal")
			}
			f.roundTrip()
		})
	}
}

func (f *adoptionNativeFixture) fixtureClient() release.Client {
	pool := x509.NewCertPool()
	pool.AddCert(f.signer.issuer)
	proxy, err := url.Parse(f.proxy.server.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxy)
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	f.t.Cleanup(transport.CloseIdleConnections)
	return release.Client{HTTPClient: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (f *adoptionNativeFixture) seedGenuinePrior() {
	f.t.Helper()
	f.write("hold-old-helper", nil, false)
	client, host := f.localClient()
	observed := f.awaitDaemon()
	request := updateinstall.Request{ID: strings.Repeat("e", 32), TargetID: host.ID, Generation: 1, Current: observed.Health.Build, Manifest: f.manifests["v0.1.249"]}
	var status updateinstall.Status
	if err := client.Call(f.t.Context(), host, "stage", request, &status); err != nil {
		f.t.Fatal(err)
	}
	if err := client.Call(f.t.Context(), host, "grant", Operation{ID: request.ID, Generation: 1}, &status); err != nil {
		f.t.Fatal(err)
	}
	config := status.Settings.Config()
	config.Client, config.Probe = f.fixtureClient(), updatebootstrap.Probe(f.state)
	engine, err := updateinstall.New(config)
	if err != nil {
		f.t.Fatal(err)
	}
	status, err = engine.Run(f.t.Context())
	if err != nil || status.Phase != updateinstall.Committed {
		f.t.Fatalf("genuine published248→249 transaction: %s %v", status.Phase, err)
	}
	if err := os.Remove(filepath.Join(f.root, "hold-old-helper")); err != nil {
		f.t.Fatal(err)
	}
	observed = f.awaitDaemon()
	if observed.Health.Build.Version != "v0.1.249" {
		f.t.Fatal("prior transaction did not run its published target")
	}
	f.roundTrip()
}

func (f *adoptionNativeFixture) publishCandidate() (string, string) {
	f.t.Helper()
	side := buildOriginalHostReader(f.t, f.signer, HostReaderEpoch, "v0.1.400")
	build := originalReaderCapabilities(f.t, side).Build
	if build.Modified {
		f.t.Fatal("native candidate must be built from the clean committed fixture source")
	}
	image, err := readNativeFixtureFile(side)
	if err != nil {
		f.t.Fatal(err)
	}
	archive := hostArchive(f.t, image)
	current := f.awaitDaemon().Health
	manifest := readerFixtureManifest(build)
	manifest.Compatibility.StateReadMin = current.Build.StateVersion
	for index := range manifest.Artifacts {
		manifest.Artifacts[index].SHA256 = discoveryDigest(archive)
	}
	forward := mustHostJSON(release.TransitionReceipt{Schema: 1, Platform: build.Platform, FromDigest: current.Build.Digest, ToDigest: build.Digest, StateReadMin: current.Build.StateVersion, StateReadMax: build.StateVersion, StateWrite: build.StateVersion, WorkerMin: 1, WorkerMax: build.WorkerProtocol, WorkerWrite: build.WorkerProtocol, JournalVersion: 1, CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true})
	transition := release.Transition{Platform: build.Platform, FromDigest: current.Build.Digest, ToDigest: build.Digest, Proof: discoveryDigest(forward)}
	manifest.Compatibility.Transitions = []release.Transition{transition}
	rollback := release.RollbackProof{Schema: 1, Platform: build.Platform, FromDigest: current.Build.Digest, ToDigest: build.Digest, Forward: transition.Proof, Workers: []string{current.Build.Digest}}
	for state := current.Build.StateVersion; state <= build.StateVersion; state++ {
		rollback.Boundaries = append(rollback.Boundaries, release.RollbackBoundary{State: state, CandidateWrote: true, SourceStarted: true, SessionIO: true, RecoveryPreserved: true})
	}
	for _, worker := range current.Workers {
		if worker.Build != nil && !slices.Contains(rollback.Workers, worker.Build.Digest) {
			rollback.Workers = append(rollback.Workers, worker.Build.Digest)
		}
	}
	proof := mustHostJSON(rollback)
	position := release.Position{Anchor: true, Ordinal: 1}
	documents := signedReaderDocuments(f.t, f.signer, manifest, HostReaderEpoch, position, &release.UpdaterReader{Schema: 1, Epoch: HostReaderEpoch, Position: position}, []release.RollbackRef{{Forward: transition.Proof, Rollback: discoveryDigest(proof)}})
	var metadata release.UpdateMetadata
	if err := decodeAuthority(documents.Metadata, &metadata); err != nil {
		f.t.Fatal(err)
	}
	for name, data := range map[string][]byte{release.UpdateMetadataAssetName: documents.Metadata, "mesh-update.sig.json": documents.Envelope, release.ManifestAssetName: documents.Manifest, metadata.Page.Name: documents.Page, transition.Proof + ".json": forward, discoveryDigest(proof) + ".rollback.json": proof, release.ArchiveName(build.Platform, release.ArchiveGzip): archive} {
		f.proxy.put(build.Version+"/"+name, data)
	}
	f.write("candidate.sha", []byte(build.Digest), false)
	return side, discoveryDigest(documents.Metadata)
}

func (f *adoptionNativeFixture) startOnce(side, pin string) {
	f.t.Helper()
	_, host := f.localClient()
	input := AdoptionLocalInput{Schema: 1, ID: strings.Repeat("a", 32), Pin: pin, Version: "v0.1.400", Expected: host.ID, Policy: HostPolicy{Mode: HostAutomatic}}
	command := &exec.Cmd{Path: side, Args: []string{side, "update", "adopt", "start", "--local", "--release", "v0.1.400", "--json"}, Env: os.Environ(), Stdin: strings.NewReader(string(mustHostJSON(input)))}
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("one side adoption start: %s %v", output, err)
	}
}

func (f *adoptionNativeFixture) awaitAdoption(want AdoptionPhase) HostAdoption {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 90*time.Second)
	defer cancel()
	store, err := OpenStore(f.state)
	if err != nil {
		f.t.Fatal(err)
	}
	for ctx.Err() == nil {
		envelope, operation, err := store.AdoptionStatus(ctx)
		if err == nil && operation.Phase == want {
			return envelope
		}
		if err == nil && (operation.Phase == AdoptionAttention || operation.Phase == AdoptionRolledBack) && operation.Phase != want {
			f.t.Fatalf("native adoption stopped: %s %s", operation.Phase, operation.Problem)
		}
		time.Sleep(50 * time.Millisecond)
	}
	root, _ := os.OpenRoot(f.root)
	if root != nil {
		defer func() { _ = root.Close() }()
		data, _ := root.ReadFile("helper.log")
		f.t.Logf("isolated helper diagnostics: %s", data)
	}
	f.t.Fatal("durable helper did not complete the native adoption within its fixture bound")
	return HostAdoption{}
}

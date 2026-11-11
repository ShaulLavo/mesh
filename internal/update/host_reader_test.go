package update

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func (f *hostFixture) readerDocuments(version string) HostReaderDocuments {
	metadata, err := release.DecodeUpdateMetadata(f.files[version+"/"+release.UpdateMetadataAssetName])
	if err != nil {
		f.t.Fatal(err)
	}
	return HostReaderDocuments{Metadata: f.files[version+"/"+release.UpdateMetadataAssetName], Envelope: f.files[version+"/mesh-update.sig.json"], Manifest: f.files[version+"/"+release.ManifestAssetName], Page: f.files[metadata.Page.Tag+"/"+metadata.Page.Name]}
}

func (f *hostFixture) readerInput(source, target string) HostReaderInput {
	input := HostReaderInput{Schema: 1, Source: f.readerDocuments(source), Target: f.readerDocuments(target), Workers: []HostReaderWorker{{Digest: f.builds[source].Digest, Protocol: 1}}}
	for _, transition := range f.manifests[target].Compatibility.Transitions {
		if transition.FromDigest == f.builds[source].Digest && transition.Platform == release.CurrentPlatform() {
			input.Forward = f.files[target+"/"+transition.Proof+".json"]
		}
	}
	forward := discoveryDigest(input.Forward)
	for _, proof := range f.metadata[target].Rollback {
		if proof.Forward == forward {
			input.Rollback = f.files[target+"/"+proof.Rollback+".rollback.json"]
		}
	}
	return input
}

func TestHostReaderInputChecksExactSourceAndCompleteProof(t *testing.T) {
	for _, kind := range []string{"valid", "source signature", "target signature", "source artifact", "target manifest", "rollback", "worker", "legacy marker", "target marker", "future target"} {
		t.Run(kind, func(t *testing.T) { testHostReaderInput(t, kind) })
	}
}

func testHostReaderInput(t *testing.T, kind string) {
	f := newHostFixture(t)
	input := f.readerInput("v0.1.1", "v0.1.2")
	current := f.current
	switch kind {
	case "source signature":
		input.Source.Envelope = []byte(`{}`)
	case "target signature":
		input.Target.Envelope = []byte(`{}`)
	case "source artifact":
		current.Digest = strings.Repeat("c", 64)
	case "target manifest":
		input.Target.Manifest = append(input.Target.Manifest, 'x')
	case "rollback":
		input.Rollback = []byte(`{}`)
	case "worker":
		input.Workers[0].Digest = strings.Repeat("c", 64)
	case "legacy marker":
		changeReaderMetadata(t, f.signer, &input.Source, func(m *release.UpdateMetadata) { m.Updater = nil })
	case "target marker":
		changeReaderMetadata(t, f.signer, &input.Target, func(m *release.UpdateMetadata) { m.Updater = nil })
	case "future target":
		changeReaderMetadata(t, f.signer, &input.Target, func(m *release.UpdateMetadata) { m.Epoch = 2; m.Updater.Epoch = 2 })
	}
	result, err := verifyHostReaderInput(t.Context(), current, 1, input, f.signer.trust)
	if kind != "valid" {
		if err == nil {
			t.Fatal("incomplete source reader acceptance succeeded")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != current || result.Target != "v0.1.2" || result.Metadata != discoveryDigest(input.Target.Metadata) {
		t.Fatalf("reader result: %+v", result)
	}
}

func changeReaderMetadata(t *testing.T, signer *signingFixture, documents *HostReaderDocuments, change func(*release.UpdateMetadata)) {
	t.Helper()
	var metadata release.UpdateMetadata
	if err := json.Unmarshal(documents.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	change(&metadata)
	documents.Metadata = mustHostJSON(metadata)
	documents.Envelope = signer.envelope(t, documents.Metadata)
}

func TestHostReaderPacketBoundsAndOriginalBytes(t *testing.T) {
	f := newHostFixture(t)
	input := f.readerInput("v0.1.1", "v0.1.2")
	input.Target.Metadata = append(input.Target.Metadata, '\n')
	input.Target.Envelope = f.signer.envelope(t, input.Target.Metadata)
	data := mustHostJSON(input)
	decoded, err := DecodeHostReaderInput(data)
	if err != nil {
		t.Fatal(err)
	}
	result, err := verifyHostReaderInput(t.Context(), f.current, 1, decoded, f.signer.trust)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata != discoveryDigest(input.Target.Metadata) {
		t.Fatal("reader re-encoded signed metadata before verification")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["current"] = json.RawMessage(`{"digest":"packet-source-claim"}`)
	if _, err := DecodeHostReaderInput(mustHostJSON(object)); err == nil {
		t.Fatal("packet supplied an executing source identity")
	}
	input.Source.Metadata = make([]byte, release.MaxMetadataBytes+1)
	if err := input.validate(); err == nil {
		t.Fatal("oversized component accepted")
	}
	if _, err := DecodeHostReaderInput(make([]byte, MaxHostReaderInputBytes+1)); err == nil {
		t.Fatal("oversized aggregate accepted")
	}
}

func buildOriginalHostReader(t *testing.T, signer *signingFixture, epoch uint32, version string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trust := filepath.Join(dir, "trusted-root.json")
	if err := os.WriteFile(trust, signer.trust.Trusted, 0600); err != nil {
		t.Fatal(err)
	}
	replace := map[string]string{filepath.Join(root, "internal/release/trust/trusted-root.json"): trust}
	if epoch != 1 {
		overlayHostReaderEpoch(t, root, dir, replace)
	}
	overlay := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlay, mustHostJSON(map[string]any{"Replace": replace}), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "mesh")
	command := exec.CommandContext(t.Context(), "go", "build")
	command.Args = append(command.Args, "-overlay", overlay, "-ldflags=-X github.com/shaul/mesh/internal/release.Version="+version, "-o", binary, "./cmd/mesh")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build original reader command: %s %v", output, err)
	}
	return binary
}

func overlayHostReaderEpoch(t *testing.T, root, dir string, replace map[string]string) {
	t.Helper()
	source := filepath.Join(root, "internal/update/host_reader_types.go")
	repository, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	data, err := repository.ReadFile("internal/update/host_reader_types.go")
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "uint32 = 1", "uint32 = 2", 1)
	if updated == string(data) {
		t.Fatal("compiled reader epoch fixture did not change")
	}
	path := filepath.Join(dir, "reader-types.go")
	destination, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destination.Close() }()
	if err := destination.WriteFile("reader-types.go", []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	replace[source] = path
}

func executeOriginalHostReader(t *testing.T, binary string, arguments ...string) ([]byte, error) {
	t.Helper()
	command := &exec.Cmd{Path: binary, Args: append([]string{binary, "update-reader"}, arguments...), WaitDelay: 5 * time.Second}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	directory := t.TempDir()
	blocked := filepath.Join(directory, "blocked-config")
	if err := os.WriteFile(blocked, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	command.Env = append(os.Environ(), "MESH_CONFIG_DIR="+blocked, "MESH_STATE_DIR="+blocked, "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=")
	output, err := command.CombinedOutput()
	root, rootErr := os.OpenRoot(directory)
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	defer func() { _ = root.Close() }()
	retained, readErr := root.ReadFile("blocked-config")
	if readErr != nil || string(retained) != "preserve" {
		t.Fatalf("reader initialized user state: %q %v", retained, readErr)
	}
	return output, hostError(err)
}

func originalReaderCapabilities(t *testing.T, binary string) HostReaderCapabilities {
	t.Helper()
	output, err := executeOriginalHostReader(t, binary, "-capabilities")
	if err != nil {
		t.Fatalf("reader capabilities: %s %v", output, err)
	}
	var capabilities HostReaderCapabilities
	if err := json.Unmarshal(output, &capabilities); err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func readerFixtureManifest(build release.Build) release.Manifest {
	manifest := release.Manifest{Schema: 1, Version: build.Version, Commit: build.Commit,
		Compatibility: release.Compatibility{StateReadMin: build.StateVersion, StateReadMax: build.StateVersion, StateWrite: build.StateVersion,
			WorkerMin: build.WorkerProtocol, WorkerMax: build.WorkerProtocol, WorkerWrite: build.WorkerProtocol, JournalVersion: release.CurrentJournalVersion}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: strings.Repeat("c", 64), BinarySHA256: build.Digest})
	}
	return manifest
}

func signedReaderDocuments(t *testing.T, signer *signingFixture, manifest release.Manifest, epoch uint32, position release.Position, marker *release.UpdaterReader, rollback []release.RollbackRef) HostReaderDocuments {
	t.Helper()
	page, err := release.BuildAnchorPage(epoch, 0, nil, nil, manifest.Version)
	if err != nil {
		t.Fatal(err)
	}
	pageData, err := release.EncodeAnchorPage(page, manifest.Version)
	if err != nil {
		t.Fatal(err)
	}
	data := mustHostJSON(manifest)
	metadata := release.UpdateMetadata{Schema: 1, Epoch: epoch, Trust: 1, Version: manifest.Version, Manifest: discoveryDigest(data), PublishedAt: signer.when,
		Position: position, Direct: release.DirectSources{AnchorsFrom: 0}, Updater: marker, Rollback: rollback,
		Page: release.AssetRef{Tag: manifest.Version, Name: discoveryDigest(pageData) + ".anchors.json", SHA256: discoveryDigest(pageData)}}
	encoded := mustHostJSON(metadata)
	return HostReaderDocuments{Metadata: encoded, Envelope: signer.envelope(t, encoded), Manifest: data, Page: pageData}
}

func originalReaderPacket(t *testing.T, signer *signingFixture, source HostReaderCapabilities, target release.Build, waypoint bool, retained []HostReaderWorker) HostReaderInput {
	t.Helper()
	sourceManifest, targetManifest := readerFixtureManifest(source.Build), readerFixtureManifest(target)
	sourceEpoch, sourcePosition := source.Epoch, release.Position{Anchor: true, Ordinal: 0}
	if source.Epoch == 2 {
		sourceEpoch, sourcePosition = 1, release.Position{Anchor: true, Ordinal: 7}
	}
	sourceMarker := &release.UpdaterReader{Schema: 1, Epoch: source.Epoch, Position: release.Position{Anchor: true, Ordinal: 0}}
	targetPosition := release.Position{Anchor: true, Ordinal: 1}
	targetMarker := &release.UpdaterReader{Schema: 1, Epoch: source.Epoch, Position: targetPosition}
	if waypoint {
		targetMarker.Epoch++
		targetMarker.Position = release.Position{Anchor: true, Ordinal: 0}
	}
	receipt := release.TransitionReceipt{Schema: 1, Platform: source.Build.Platform, FromDigest: source.Build.Digest, ToDigest: target.Digest,
		StateReadMin: source.Build.StateVersion, StateReadMax: target.StateVersion, StateWrite: target.StateVersion,
		WorkerMin: 1, WorkerMax: target.WorkerProtocol, WorkerWrite: target.WorkerProtocol, JournalVersion: release.CurrentJournalVersion,
		CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true}
	forward := mustHostJSON(receipt)
	transition := release.Transition{Platform: source.Build.Platform, FromDigest: source.Build.Digest, ToDigest: target.Digest, Proof: discoveryDigest(forward)}
	targetManifest.Compatibility.Transitions = []release.Transition{transition}
	proof := release.RollbackProof{Schema: 1, Platform: source.Build.Platform, FromDigest: source.Build.Digest, ToDigest: target.Digest, Forward: transition.Proof, Workers: []string{source.Build.Digest}}
	for state := source.Build.StateVersion; state <= target.StateVersion; state++ {
		proof.Boundaries = append(proof.Boundaries, release.RollbackBoundary{State: state, CandidateWrote: true, SourceStarted: true, SessionIO: true, RecoveryPreserved: true})
	}
	for _, worker := range retained {
		if worker.Digest != source.Build.Digest {
			proof.Workers = append(proof.Workers, worker.Digest)
		}
	}
	rollback := mustHostJSON(proof)
	return HostReaderInput{Schema: 1,
		Source:  signedReaderDocuments(t, signer, sourceManifest, sourceEpoch, sourcePosition, sourceMarker, nil),
		Target:  signedReaderDocuments(t, signer, targetManifest, source.Epoch, targetPosition, targetMarker, []release.RollbackRef{{Forward: transition.Proof, Rollback: discoveryDigest(rollback)}}),
		Forward: forward, Rollback: rollback, Workers: retained}
}

func verifyOriginalReaderPacket(t *testing.T, binary string, input HostReaderInput) (HostReaderResult, error) {
	t.Helper()
	dir := t.TempDir()
	packet := filepath.Join(dir, "packet.json")
	if err := os.WriteFile(packet, mustHostJSON(input), 0600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "trust")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	output, err := executeOriginalHostReader(t, binary, "-input", packet, "-trust-cache", cache)
	entries, readErr := os.ReadDir(cache)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("offline reader wrote trust cache: %v %v", entries, readErr)
	}
	if err != nil {
		return HostReaderResult{}, err
	}
	var result HostReaderResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("reader output: %s %v", output, err)
	}
	return result, nil
}

func TestHostReaderOriginalExecutablesVerifyWaypointAndNewEpoch(t *testing.T) {
	signer := newSigningFixture(t)
	old := buildOriginalHostReader(t, signer, 1, "v0.1.1")
	waypoint := buildOriginalHostReader(t, signer, 2, "v0.1.2")
	oldCaps, waypointCaps := originalReaderCapabilities(t, old), originalReaderCapabilities(t, waypoint)
	if oldCaps.Schema != 1 || oldCaps.Epoch != 1 || waypointCaps.Schema != 1 || waypointCaps.Epoch != 2 {
		t.Fatalf("compiled capabilities: %+v %+v", oldCaps, waypointCaps)
	}
	input := originalReaderPacket(t, signer, oldCaps, waypointCaps.Build, true, []HostReaderWorker{{Digest: oldCaps.Build.Digest, Protocol: oldCaps.Build.WorkerProtocol}})
	result, err := verifyOriginalReaderPacket(t, old, input)
	if err != nil || result.Source != oldCaps.Build || result.Binary != waypointCaps.Build.Digest {
		t.Fatalf("old source did not verify its old-format waypoint: %+v %v", result, err)
	}
	wrongSigner := newSigningFixture(t)
	rejected := input
	rejected.Target.Envelope = wrongSigner.envelope(t, rejected.Target.Metadata)
	if _, err := verifyOriginalReaderPacket(t, old, rejected); err == nil {
		t.Fatal("original reader accepted a target signed by an unrelated root")
	}
	later := waypointCaps.Build
	later.Version, later.Digest = "v0.1.3", discoveryDigest([]byte("later release executable"))
	next := originalReaderPacket(t, signer, waypointCaps, later, false, input.Workers)
	result, err = verifyOriginalReaderPacket(t, waypoint, next)
	if err != nil || result.Epoch != 2 || result.Position != (release.Position{Anchor: true, Ordinal: 0}) {
		t.Fatalf("waypoint source did not convert its authenticated epoch coordinates: %+v %v", result, err)
	}
	future := originalReaderPacket(t, signer, oldCaps, later, false, input.Workers)
	changeReaderMetadata(t, signer, &future.Target, func(m *release.UpdateMetadata) { m.Epoch = 2; m.Updater.Epoch = 2 })
	if _, err := verifyOriginalReaderPacket(t, old, future); err == nil {
		t.Fatal("older executable accepted a future-format target instead of its waypoint")
	}
	changeReaderMetadata(t, signer, &input.Source, func(m *release.UpdateMetadata) { m.Updater = nil })
	if _, err := verifyOriginalReaderPacket(t, old, input); err == nil {
		t.Fatal("legacy source metadata was treated as autonomous reader capability")
	}
	assertOriginalReaderImageBinding(t, old, oldCaps, signer, waypointCaps.Build)
}

func assertOriginalReaderImageBinding(t *testing.T, binary string, source HostReaderCapabilities, signer *signingFixture, target release.Build) {
	t.Helper()
	input := originalReaderPacket(t, signer, source, target, true, nil)
	root, err := os.OpenRoot(filepath.Dir(binary))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(filepath.Base(binary), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\nchanged reader image\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	changed := originalReaderCapabilities(t, binary)
	if changed.Build.Digest == source.Build.Digest {
		t.Fatal("capabilities reported packet identity instead of the executing image")
	}
	if _, err := verifyOriginalReaderPacket(t, binary, input); err == nil {
		t.Fatal("packet overrode the actual executing source image")
	}
}

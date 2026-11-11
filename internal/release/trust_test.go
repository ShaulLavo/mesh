package release

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore/pkg/signature"
	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
)

type fixtureTransport struct {
	documents map[string][]byte
	requests  []string
}

func (fixture *fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	fixture.requests = append(fixture.requests, request.URL.Path)
	contents, ok := fixture.documents[request.URL.Path]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(contents)), Header: make(http.Header), Request: request}, nil
}

func testTUFSigner(t *testing.T) (signature.Signer, *metadata.Key) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signature.LoadSigner(private, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	key, err := metadata.KeyFromPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer, key
}

func signedTUF[T metadata.Roles](t *testing.T, doc *metadata.Metadata[T], signers ...signature.Signer) []byte {
	t.Helper()
	for _, signer := range signers {
		if _, err := doc.Sign(signer); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := doc.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func tufFixture(t *testing.T, rotations int, material []byte, expiry time.Time) (TrustRoot, *fixtureTransport) {
	t.Helper()
	fixture := &fixtureTransport{documents: map[string][]byte{}}
	var bootstrap []byte
	var previous signature.Signer
	var signer signature.Signer
	for version := 1; version <= rotations; version++ {
		rootBytes, next := fixtureRoot(t, version, previous, expiry)
		if version == 1 {
			bootstrap = rootBytes
		}
		fixture.documents[fmt.Sprintf("/%d.root.json", version)] = rootBytes
		previous, signer = next, next
	}
	fixtureTargets(t, fixture, signer, material, expiry)
	trust := recordedTrust()
	trust.BootstrapRoot = bootstrap
	return trust, fixture
}

func fixtureRoot(t *testing.T, version int, previous signature.Signer, expiry time.Time) ([]byte, signature.Signer) {
	t.Helper()
	signer, key := testTUFSigner(t)
	root := metadata.Root(expiry)
	root.Signed.Version = int64(version)
	if version == 1 {
		root.Signed.Expires = time.Now().Add(-time.Hour)
	}
	for _, role := range []string{metadata.ROOT, metadata.TIMESTAMP, metadata.SNAPSHOT, metadata.TARGETS} {
		if err := root.Signed.AddKey(key, role); err != nil {
			t.Fatal(err)
		}
	}
	signers := []signature.Signer{signer}
	if previous != nil {
		signers = append(signers, previous)
	}
	return signedTUF(t, root, signers...), signer
}

func fixtureTargets(t *testing.T, fixture *fixtureTransport, signer signature.Signer, material []byte, expiry time.Time) {
	t.Helper()
	target, err := metadata.TargetFile().FromBytes("trusted_root.json", material, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	targets := metadata.Targets(expiry)
	targets.Signed.Targets["trusted_root.json"] = target
	fixture.documents["/1.targets.json"] = signedTUF(t, targets, signer)
	snapshot := metadata.Snapshot(expiry)
	snapshot.Signed.Meta["targets.json"] = metadata.MetaFile(1)
	fixture.documents["/1.snapshot.json"] = signedTUF(t, snapshot, signer)
	timestamp := metadata.Timestamp(expiry)
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(1)
	fixture.documents["/timestamp.json"] = signedTUF(t, timestamp, signer)
	fixture.documents["/targets/"+fmt.Sprintf("%x", []byte(target.Hashes["sha256"]))+".trusted_root.json"] = material
}

func TestTrustRefreshResumesAuthenticatedRotationFromExpiredBootstrap(t *testing.T) {
	trust, fixture := tufFixture(t, 10, recordedTrust().Trusted, time.Now().Add(time.Hour))
	cache := t.TempDir()
	client := &http.Client{Transport: fixture}
	_, err := refreshTrust(context.Background(), trust, cache, TrustRefreshBudget{Documents: 8, Duration: time.Second}, client)
	if !errors.Is(err, ErrTrustRefreshBudget) {
		t.Fatalf("want resumable budget error, got %v", err)
	}
	if _, err := readTrustFile(filepath.Join(cache, "roots", "9.root.json")); err != nil {
		t.Fatal(err)
	}
	fixture.requests = nil
	refreshed, err := refreshTrust(context.Background(), trust, cache, TrustRefreshBudget{Documents: 8, Duration: time.Second}, client)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.requests[0] != "/10.root.json" {
		t.Fatalf("repeated completed rotation: %v", fixture.requests)
	}
	if len(refreshed.history) != 1 {
		t.Fatal("authenticated material was not retained")
	}
	if err := VerifyArtifactSignature(refreshed, recordedSubject, recordedEnvelope(t)); err != nil {
		t.Fatal(err)
	}
}

func TestTrustRefreshRejectsUnsignedRotationAndExpiry(t *testing.T) {
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	wrongRoot, _ := fixtureRoot(t, 2, nil, time.Now().Add(time.Hour))
	fixture.documents["/2.root.json"] = wrongRoot
	_, err := refreshTrust(context.Background(), trust, t.TempDir(), TrustRefreshBudget{Documents: 8, Duration: time.Second}, &http.Client{Transport: fixture})
	if err == nil {
		t.Fatal("accepted an arbitrary new TUF signer")
	}
	trust, fixture = tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(-time.Minute))
	_, err = refreshTrust(context.Background(), trust, t.TempDir(), TrustRefreshBudget{Documents: 8, Duration: time.Second}, &http.Client{Transport: fixture})
	if err == nil {
		t.Fatal("accepted expired current root and timestamp")
	}
}

func TestHistoricalTrustMaterialSurvivesKeyRotation(t *testing.T) {
	trust := recordedTrust()
	cache := t.TempDir()
	if err := persistTrustMaterial(cache, trust.Trusted); err != nil {
		t.Fatal(err)
	}
	var rotated map[string]any
	if err := json.Unmarshal(trust.Trusted, &rotated); err != nil {
		t.Fatal(err)
	}
	rotated["ctlogs"] = []any{}
	rotated["tlogs"] = []any{}
	contents, err := json.Marshal(rotated)
	if err != nil {
		t.Fatal(err)
	}
	trust.Trusted = contents
	if err := VerifyArtifactSignature(trust, recordedSubject, recordedEnvelope(t)); err == nil {
		t.Fatal("accepted bundle without historical log keys")
	}
	if err := persistTrustMaterial(cache, contents); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.history) != 2 {
		t.Fatal("lost historical material")
	}
	if err := VerifyArtifactSignature(loaded, recordedSubject, recordedEnvelope(t)); err != nil {
		t.Fatal(err)
	}
}

func TestTrustFetcherBoundsAndOrigin(t *testing.T) {
	trust := recordedTrust()
	chain, _, err := loadRootChain(trust.BootstrapRoot, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixture := &fixtureTransport{documents: map[string][]byte{"/oversized": bytes.Repeat([]byte("a"), 20)}}
	fetch := &trustFetcher{ctx: context.Background(), client: &http.Client{Transport: fixture}, remaining: 2, chain: chain, cache: t.TempDir()}
	if _, err := fetch.DownloadFile("https://elsewhere.example/root.json", 100, 0); err == nil {
		t.Fatal("accepted unexpected trust origin")
	}
	if _, err := fetch.DownloadFile(tuf.DefaultMirror+"/oversized", 10, 0); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("accepted oversized trust document: %v", err)
	}
}

func TestUnsetTrustCacheUsesOnlyEmbeddedMaterial(t *testing.T) {
	cache := t.TempDir()
	if err := persistTrustMaterial(cache, recordedTrust().Trusted); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cache)
	trust, err := LoadTrustRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if len(trust.history) != 0 {
		t.Fatal("loaded trust from the working directory")
	}
}

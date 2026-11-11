package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

type discoveryFixture struct {
	from      release.Build
	at        release.Position
	documents map[string][]byte
	reads     map[string]int
	verified  int
}

func recordedDiscovery(t *testing.T) *discoveryFixture {
	t.Helper()
	f := &discoveryFixture{documents: make(map[string][]byte), reads: make(map[string]int)}
	root, err := os.OpenRoot("testdata/discovery")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("fixture walk: %w", walkErr)
		}
		if entry.IsDir() {
			return nil
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return fmt.Errorf("fixture read: %w", err)
		}
		f.documents[path] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var input struct {
		From release.Build    `json:"from"`
		At   release.Position `json:"at"`
	}
	if err := json.Unmarshal(f.documents["source.json"], &input); err != nil {
		t.Fatal(err)
	}
	f.from, f.at = input.From, input.At
	return f
}

func (f *discoveryFixture) source() release.DocumentSource {
	return release.DocumentSource{Read: func(_ context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		key := filepath.Join(ref.Tag, ref.Name)
		f.reads[key]++
		data, ok := f.documents[key]
		if !ok {
			return nil, fmt.Errorf("fixture document absent: %s", key)
		}
		return data, nil
	}, VerifyMetadata: func(_ context.Context, ref release.AssetRef, data []byte) error {
		f.verified++
		if !slices.Equal(f.documents[filepath.Join(ref.Tag, ref.Name)], data) {
			return errors.New("local fixture metadata identity mismatch")
		}
		return nil
	}}
}

func TestDiscoveryRecordedReceiptsCacheAndRestart(t *testing.T) {
	f := recordedDiscovery(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "host-discovery.json")
	cursor := DiscoveryCursor{}
	for pass := 0; pass < 2; pass++ {
		source := CacheDiscoveryDocuments(filepath.Join(directory, "documents"), f.source())
		budget := release.NewPassBudget()
		budget.Evidence = 1
		step, err := DiscoverNextHop(t.Context(), f.from, f.at, 1, cursor, source, budget)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveDiscoveryCursor(path, step.Cursor); err != nil {
			t.Fatal(err)
		}
		cursor, err = LoadDiscoveryCursor(path)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 && (!step.Exhausted || step.Next != nil) {
			t.Fatal("first pass did not stop before receipt")
		}
		if pass == 1 && (step.Next == nil || step.Next.Manifest.Version != "v0.1.306") {
			t.Fatalf("resumed hop = %v", step.Next)
		}
	}
	if f.reads["v0.1.306/mesh-release.json"] != 1 || f.reads["v0.1.306/mesh-update.json"] != 1 || f.verified != 2 {
		t.Fatalf("restart cache reads=%v, verifications=%d", f.reads, f.verified)
	}
	// A modified cache entry is refetched by digest, never accepted as a hit.
	for name := range f.documents {
		if name != "v0.1.306/mesh-release.json" {
			continue
		}
		digest := discoveryDigest(f.documents[name])
		if err := os.WriteFile(filepath.Join(directory, "documents", digest), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cursor.Route.Search.Pending.Manifest = nil
	step, err := DiscoverNextHop(t.Context(), f.from, f.at, 1, cursor, CacheDiscoveryDocuments(filepath.Join(directory, "documents"), f.source()), release.NewPassBudget())
	if err != nil || step.Next == nil || f.reads["v0.1.306/mesh-release.json"] != 2 {
		t.Fatalf("corrupt cache handling: %v", err)
	}
}

func TestDiscoveryMoreThanEightEpochsNeverReadsFutureMetadata(t *testing.T) {
	f := recordedDiscovery(t)
	var previous *release.AssetRef
	for epoch := uint32(1); epoch <= 19; epoch++ {
		tag := fmt.Sprintf("v0.2.%d", epoch)
		head := release.AssetRef{Tag: tag, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest([]byte("incompatible future metadata"))}
		if epoch == 1 {
			var root release.DiscoveryRoot
			_ = json.Unmarshal(f.documents["latest/mesh-update-root.json"], &root)
			head = root.Head
			tag = head.Tag
		}
		root := release.DiscoveryRoot{Format: 1, Epoch: epoch, Head: head, Previous: previous}
		data, err := release.EncodeDiscoveryRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		f.documents[filepath.Join(tag, release.DiscoveryRootAssetName)] = data
		previous = &release.AssetRef{Tag: tag, Name: release.DiscoveryRootAssetName, SHA256: discoveryDigest(data)}
		if epoch == 19 {
			f.documents[filepath.Join(release.LatestDiscoveryTag, release.DiscoveryRootAssetName)] = data
		}
	}
	cursor := DiscoveryCursor{}
	directory := t.TempDir()
	path := filepath.Join(directory, "cursor.json")
	for pass := 0; pass < 8; pass++ {
		refreshFixtureFutureHead(t, f, pass)
		step, err := DiscoverNextHop(t.Context(), f.from, f.at, 1, cursor, f.source(), release.NewPassBudget())
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveDiscoveryCursor(path, step.Cursor); err != nil {
			t.Fatal(err)
		}
		cursor, err = LoadDiscoveryCursor(path)
		if err != nil {
			t.Fatal(err)
		}
		if step.Next != nil && pass < 2 {
			t.Fatal("epoch traversal ignored pass bounds")
		}
		if step.Next != nil {
			assertNoRepeatedEpochReads(t, f)
			return
		}
		if !step.Exhausted {
			t.Fatal("epoch traversal did not save exhausted pass")
		}
	}
	t.Fatal("epoch traversal restarted instead of progressing")
}

func assertNoRepeatedEpochReads(t *testing.T, f *discoveryFixture) {
	t.Helper()
	for name, count := range f.reads {
		if name == "latest/mesh-update-root.json" {
			continue
		}
		if count != 1 {
			t.Fatalf("epoch/document %s fetched %d times", name, count)
		}
		if len(name) > 5 && name[:5] == "v0.2." && filepath.Base(name) != release.DiscoveryRootAssetName {
			t.Fatalf("read incompatible future document %s", name)
		}
	}
}

func TestDiscoveryRequiresSignatureAdapterAndExactRootIdentity(t *testing.T) {
	f := recordedDiscovery(t)
	if _, err := DiscoverNextHop(t.Context(), f.from, f.at, 1, DiscoveryCursor{}, CacheDiscoveryDocuments(t.TempDir(), release.DocumentSource{}), release.NewPassBudget()); err == nil {
		t.Fatal("host discovery accepted no document reader")
	}
	source := f.source()
	source.VerifyMetadata = nil
	if _, err := DiscoverNextHop(t.Context(), f.from, f.at, 1, DiscoveryCursor{}, source, release.NewPassBudget()); err == nil {
		t.Fatal("host discovery accepted no signature adapter")
	}
	var root release.DiscoveryRoot
	_ = json.Unmarshal(f.documents["latest/mesh-update-root.json"], &root)
	root.Head.Tag = "v0.1.305"
	data, _ := release.EncodeDiscoveryRoot(root)
	f.documents["latest/mesh-update-root.json"] = data
	if _, err := DiscoverNextHop(t.Context(), f.from, f.at, 1, DiscoveryCursor{}, f.source(), release.NewPassBudget()); err == nil {
		t.Fatal("host discovery accepted wrong head identity")
	}
}

func TestDiscoveryPageCacheReopensByDigest(t *testing.T) {
	f := recordedDiscovery(t)
	metadata, err := release.DecodeUpdateMetadata(f.documents["v0.1.306/mesh-update.json"])
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for restart := 0; restart < 3; restart++ {
		source := CacheDiscoveryDocuments(directory, f.source())
		budget := release.NewPassBudget()
		data, err := release.ReadDocument(t.Context(), source, metadata.Page, release.PageDocument, &budget)
		if err != nil {
			t.Fatal(err)
		}
		page, err := release.DecodeAnchorPage(data, metadata.Page)
		if err != nil || len(page.Entries) != 1 || page.Entries[0].Version != "v0.1.304" {
			t.Fatalf("cached page = %v, %v", page, err)
		}
	}
	if f.reads["v0.1.306/anchors.json"] != 1 {
		t.Fatalf("page refetched after restart: %v", f.reads)
	}
}

func refreshFixtureFutureHead(t *testing.T, f *discoveryFixture, pass int) {
	t.Helper()
	path := "latest/mesh-update-root.json"
	var root release.DiscoveryRoot
	if err := json.Unmarshal(f.documents[path], &root); err != nil {
		t.Fatal(err)
	}
	root.Head.Tag = fmt.Sprintf("v0.3.%d", pass)
	data, err := release.EncodeDiscoveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	f.documents[path] = data
}

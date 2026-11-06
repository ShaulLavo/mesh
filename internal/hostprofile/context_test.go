package hostprofile

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fleetFixture(t *testing.T, revision uint64) FleetDocument {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(public)
	doc, err := signFleetFixture(editable("fleet instructions"), "fixture", revision, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), id, key)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestReplicaPassiveAbsenceAndTrust(t *testing.T) {
	state := filepath.Join(t.TempDir(), "absent")
	store := OpenFleetStore(state)
	snapshot, err := store.ReadReplica(func(string) bool { return true })
	if err != nil || snapshot.State != StateAbsent || snapshot.Revision == nil || *snapshot.Revision != 0 {
		t.Fatalf("absent: %+v %v", snapshot, err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("reader created state: %v", err)
	}
	doc := fleetFixture(t, 4)
	writeReplica(t, state, doc)
	snapshot, err = store.ReadReplica(func(string) bool { return false })
	if err != nil || snapshot.State != StateUntrusted || snapshot.Value == nil || *snapshot.Revision != 4 {
		t.Fatalf("revoked: %+v %v", snapshot, err)
	}
	doc.Signature = "bad"
	writeReplica(t, state, doc)
	snapshot, err = store.ReadReplica(func(string) bool { return true })
	if err != nil || snapshot.State != StateInvalid || snapshot.Value != nil {
		t.Fatalf("bad signature: %+v %v", snapshot, err)
	}
}

func writeReplica(t *testing.T, state string, doc FleetDocument) {
	t.Helper()
	dir := filepath.Join(state, "fleet")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestContextSummaryRetainsByteCountNotInstructions(t *testing.T) {
	config, state := t.TempDir(), t.TempDir()
	store := OpenHostStore(config, "host")
	if _, _, err := store.Set(t.Context(), 0, editable("secret notes")); err != nil {
		t.Fatal(err)
	}
	reply := ReadContext(config, state, "host", func(string) bool { return true }, false)
	if reply.Host.Completeness != Summary || reply.Host.Profile == nil || reply.Host.Profile.Instructions != "" || reply.Host.InstructionsBytes != len("secret notes") {
		t.Fatalf("summary leaks or loses metadata: %+v", reply.Host)
	}
	full := ReadContext(config, state, "host", func(string) bool { return true }, true)
	if full.Host.Profile == nil || full.Host.Profile.Instructions != "secret notes" || full.Host.Completeness != Full {
		t.Fatal(full)
	}
}

func TestContextCacheFullSummaryAndRegression(t *testing.T) {
	config, state := t.TempDir(), t.TempDir()
	host := fleetFixture(t, 1).Author
	store := OpenHostStore(config, host)
	if _, _, err := store.Set(t.Context(), 0, editable("full notes")); err != nil {
		t.Fatal(err)
	}
	cache := OpenContextCache(state)
	full := ReadContext(config, state, host, func(string) bool { return true }, true)
	entry := CachedContext{HostID: host, MeshIdentity: host, ReceivedAt: time.Now().UTC(), Context: full}
	if _, err := cache.Save(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	entry.Context = ReadContext(config, state, host, func(string) bool { return true }, false)
	if regressed, err := cache.Save(t.Context(), entry); err != nil || regressed {
		t.Fatalf("summary: %v %v", regressed, err)
	}
	saved, err := cache.Read(host, host)
	if err != nil || saved == nil || saved.Context.Host.Completeness != Full || saved.Context.Host.Profile.Instructions != "full notes" {
		t.Fatalf("full erased: %+v %v", saved, err)
	}
	next := uint64(2)
	entry.Context.Host.Revision = &next
	entry.Context.Host.Digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := cache.Save(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	saved, err = cache.Read(host, host)
	if err != nil || saved.Context.Host.Completeness != Summary {
		t.Fatalf("changed digest retained body: %+v %v", saved, err)
	}
	old := uint64(1)
	entry.Context.Host.Revision = &old
	if regressed, err := cache.Save(t.Context(), entry); err != nil || !regressed {
		t.Fatalf("regression: %v %v", regressed, err)
	}
	if regressed, err := cache.Save(t.Context(), entry); err != nil || regressed {
		t.Fatalf("regression repeated: %v %v", regressed, err)
	}
	other, err := cache.Read(host, "different-pin")
	if err != nil || other != nil {
		t.Fatalf("identity cache leaked: %+v %v", other, err)
	}
}

func signFleetFixture(e Editable, fleet string, revision uint64, at time.Time, id string, key ed25519.PrivateKey) (FleetDocument, error) {
	normalized, err := normalizeEditable(e, FleetLimits)
	if err != nil {
		return FleetDocument{}, err
	}
	if len(key) != ed25519.PrivateKeySize || base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) != id {
		return FleetDocument{}, invalid("author", "does not match signing key")
	}
	d := FleetDocument{Editable: normalized, Fleet: fleet, Revision: revision, UpdatedAt: at.UTC(), Author: id}
	d.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, d.signedBytes()))
	if err := VerifyFleet(d); err != nil {
		return FleetDocument{}, err
	}
	return d, nil
}

func TestFleetSignatureAndStrictReplicaValidation(t *testing.T) {
	doc := fleetFixture(t, 7)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseFleet(data); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte) []byte{
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"revision":7`), []byte(`"revision":6`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"schemaVersion":1`), []byte(`"SchemaVersion":1`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":2`), 1)
		},
		func(data []byte) []byte { return append(data, 'x') },
	} {
		if _, err := parseFleet(mutate(data)); err == nil {
			t.Error("invalid fleet accepted")
		}
	}
	oldDigest := doc.Digest()
	doc.Signature = "invalid"
	if doc.Digest() != oldDigest || VerifyFleet(doc) == nil {
		t.Fatal("digest was mistaken for signature verification")
	}
}

func TestContextRejectsInvalidWireAndPinnedDigest(t *testing.T) {
	host := "host"
	store := OpenHostStore(t.TempDir(), host)
	doc, _, err := store.Set(t.Context(), 0, editable("notes"))
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	reply := ContextReply{Host: HostSection(Snapshot[HostProfile]{State: StateValid, Revision: &doc.Revision, Value: &doc}, true), Fleet: WireSection{State: StateAbsent, Revision: &zero, Completeness: Full}}
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseContext(data, "other-host"); err == nil {
		t.Fatal("full body not pinned to host")
	}
	for _, mutate := range []func([]byte) []byte{
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"instructionsBytes":5`), []byte(`"instructionsBytes":4`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"completeness":"full"`), []byte(`"completeness":"summary"`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"state":"valid"`), []byte(`"state":"unknown"`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"revision":1`), []byte(`"Revision":1`), 1)
		},
		func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"profile":{`), []byte(`"profile":{"extra":true,`), 1)
		},
		func(data []byte) []byte { return []byte(`{"host":null,"fleet":null}`) },
	} {
		if _, err := ParseContext(mutate(data), host); err == nil {
			t.Error("invalid context accepted")
		}
	}
}

func TestContextCacheMissingStateUnsafeFilesAndPrune(t *testing.T) {
	state := filepath.Join(t.TempDir(), "missing")
	cache := OpenContextCache(state)
	if entry, err := cache.Read("host", "pin"); err != nil || entry != nil {
		t.Fatal(entry, err)
	}
	zero := uint64(0)
	empty := WireSection{State: StateAbsent, Revision: &zero, Completeness: Full}
	entry := CachedContext{HostID: "host", MeshIdentity: "pin", ReceivedAt: time.Now().UTC(), Context: ContextReply{Host: empty, Fleet: empty}}
	if _, err := cache.Save(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("missing state created")
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Save(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "context-cache", contextKey("host", "pin"))
	saved, err := os.ReadFile(path) //nolint:gosec // Test-owned cache path.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Read("host", "pin"); err == nil {
		t.Fatal("multiply-linked cache accepted")
	}
	if err := os.Remove(path + ".link"); err != nil {
		t.Fatal(err)
	}
	if err := cache.Prune(t.Context(), map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("orphan cache not pruned")
	}
	if err := os.Symlink(filepath.Join(state, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Save(t.Context(), entry); err == nil {
		t.Fatal("unsafe cache failed to report error")
	}
	if got, err := cache.Read("host", "pin"); err != nil || got == nil {
		t.Fatalf("validated observation did not replace unsafe entry: %v", err)
	}
	if len(saved) == 0 {
		t.Fatal("empty cache")
	}
}

func TestContextCacheConcurrentReadersAndSummaries(t *testing.T) {
	state, config := t.TempDir(), t.TempDir()
	store := OpenHostStore(config, "host")
	if _, _, err := store.Set(t.Context(), 0, editable("complete")); err != nil {
		t.Fatal(err)
	}
	cache := OpenContextCache(state)
	entry := CachedContext{HostID: "host", MeshIdentity: "pin", ReceivedAt: time.Now().UTC(), Context: ReadContext(config, state, "host", nil, true)}
	if _, err := cache.Save(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	entry.Context = ReadContext(config, state, "host", nil, false)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() { checkConcurrentContext(t, cache, entry) })
	}
	group.Wait()
}

func checkConcurrentContext(t *testing.T, cache *ContextCache, entry CachedContext) {
	t.Helper()
	if _, err := cache.Save(t.Context(), entry); err != nil {
		t.Error(err)
		return
	}
	got, err := cache.Read(entry.HostID, entry.MeshIdentity)
	if err != nil || got == nil || got.Context.Host.Completeness != Full {
		t.Errorf("concurrent full content lost: %+v %v", got, err)
	}
}

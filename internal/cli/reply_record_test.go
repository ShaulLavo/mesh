package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/update"
)

func TestFleetReplyReadLeavesLiveAndCleanWALUntouched(t *testing.T) {
	state, host := setupUpdateCLI(t)
	store, err := storage.Open(t.Context(), filepath.Join(state, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if _, err := store.UpsertHost(t.Context(), storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID, LastSeenAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := writeReplyRecord(state, host.ID, at); err != nil {
		t.Fatal(err)
	}
	before := replyDatabaseFiles(t, state)
	if len(before) != 3 {
		t.Fatalf("live WAL positive control: %#v", before)
	}
	if got := fleetLastReplies([]update.Host{host})[host.ID]; !got.Equal(at) {
		t.Fatal("lost verified timestamp", got)
	}
	if after := replyDatabaseFiles(t, state); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("reader changed live WAL files")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before = replyDatabaseFiles(t, state)
	if len(before) != 1 {
		t.Fatal("closed WAL positive control", before)
	}
	_ = fleetLastReplies([]update.Host{host})
	if after := replyDatabaseFiles(t, state); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("reader created sidecars on a clean WAL database")
	}
}

func replyDatabaseFiles(t *testing.T, state string) map[string]string {
	t.Helper()
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	result := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		name := catalogDatabaseName + suffix
		data, err := root.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		info, err := root.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = fmt.Sprintf("%x/%d/%d", sha256.Sum256(data), info.Size(), info.ModTime().UnixNano())
	}
	return result
}

func TestReplyRecordThrottleAndAtomicReplacement(t *testing.T) {
	state := t.TempDir()
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := writeReplyRecord(state, "peer", at); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, replyRecordDirectory, replyRecordName("peer"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReplyRecord(state, "peer", at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("refresh loop replaced recent record", err)
	}
	var group sync.WaitGroup
	for index := range 4 {
		group.Go(func() { writeAndReadReplyRecord(t, state, at.Add(time.Duration(index)*2*time.Minute)) })
	}
	group.Wait()
}

func writeAndReadReplyRecord(t *testing.T, state string, at time.Time) {
	t.Helper()
	for range 8 {
		if err := writeReplyRecord(state, "peer", at); err != nil {
			t.Error(err)
			return
		}
		if got, ok := readReplyRecord(state, "peer"); !ok || got.IsZero() {
			t.Error("atomic record read was torn")
			return
		}
	}
}

func TestReplyRecordRejectsInvalidContentsWithoutWriting(t *testing.T) {
	for _, data := range []string{"", "{", `{"meshIdentity":"other","verifiedAt":"2026-01-01T00:00:00Z"}`, `{"meshIdentity":"peer","verifiedAt":"0001-01-01T00:00:00Z"}`, `{"meshIdentity":"peer","verifiedAt":"2026-01-01T00:00:00Z","extra":1}`, strings.Repeat(" ", replyRecordLimit+1)} {
		checkInvalidReplyRecord(t, []byte(data))
	}
}

func checkInvalidReplyRecord(t *testing.T, data []byte) {
	t.Helper()
	state := t.TempDir()
	if err := writeReplyRecord(state, "peer", time.Now()); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(state, replyRecordDirectory))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	name := replyRecordName("peer")
	if err := root.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readReplyRecord(state, "peer"); ok {
		t.Fatalf("accepted invalid record %q", data)
	}
	after, err := root.ReadFile(name)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("rejected read changed record", err)
	}
}

func TestReplyRecordFollowsOnlyCommittedCatalogObservations(t *testing.T) {
	state, _ := setupUpdateCLI(t)
	store, err := storage.Open(t.Context(), filepath.Join(state, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Now().UTC().Truncate(time.Millisecond)
	cache := &SQLiteCatalogCache{store: store, now: func() time.Time { return at }, stateDir: state}
	host := HostRecord{ID: "peer", MeshIdentity: "peer"}
	if saved, err := cache.Save(t.Context(), host, nil); err != nil || !saved {
		t.Fatal(saved, err)
	}
	row, err := store.GetHost(t.Context(), "peer")
	got, ok := readReplyRecord(state, "peer")
	if err != nil || !ok || !got.Equal(row.LastSeenAt) || !got.Equal(at) {
		t.Fatal("record differs from committed host observation", got, row, err)
	}
	data, err := json.Marshal(replyRecord{MeshIdentity: "peer", VerifiedAt: at})
	if err != nil || len(data) > replyRecordLimit {
		t.Fatal("record exceeds bound", err)
	}
}

func TestReplyRecordServiceCommitUsesSameObservedTime(t *testing.T) {
	state, _ := setupUpdateCLI(t)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	at := time.Now().UTC().Truncate(time.Millisecond)
	cache.now = func() time.Time { return at }
	host := HostRecord{ID: "peer", MeshIdentity: "peer"}
	if saved, err := cache.SaveServices(t.Context(), host, "mesh.test", nil); err != nil || !saved {
		t.Fatal(saved, err)
	}
	row, err := cache.store.GetHost(t.Context(), "peer")
	got, ok := readReplyRecord(state, "peer")
	if err != nil || !ok || !got.Equal(at) || !got.Equal(row.LastSeenAt) {
		t.Fatal("service commit timestamp differs", got, row, err)
	}
}

func TestReplyRecordRefusesLinksAndNonRegularFiles(t *testing.T) {
	for _, directory := range []bool{false, true} {
		state := t.TempDir()
		if err := writeReplyRecord(state, "peer", time.Now()); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(filepath.Join(state, replyRecordDirectory))
		if err != nil {
			t.Fatal(err)
		}
		name := replyRecordName("peer")
		if err := root.Rename(name, "target"); err != nil {
			t.Fatal(err)
		}
		if directory {
			err = root.Mkdir(name, 0o700)
		} else {
			err = root.Symlink("target", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, ok := readReplyRecord(state, "peer")
		_ = root.Close()
		if ok {
			t.Fatal("nonregular record supplied a reply")
		}
	}
}

func TestReplyRecordSkippedAndRejectedObservationsDoNotPublish(t *testing.T) {
	state, _ := setupUpdateCLI(t)
	host := HostRecord{ID: "peer", MeshIdentity: "peer"}
	empty := &SQLiteCatalogCache{stateDir: state}
	if saved, err := empty.Save(t.Context(), host, nil); err != nil || saved {
		t.Fatal("unavailable cache claimed a save", saved, err)
	}
	store, err := storage.Open(t.Context(), filepath.Join(state, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cache := &SQLiteCatalogCache{store: store, stateDir: state, now: time.Now}
	if saved, err := cache.Save(t.Context(), host, []protocol.SessionInfo{{HostID: "foreign"}}); err == nil || saved {
		t.Fatal("invalid observation claimed a save", saved, err)
	}
	if _, err := os.Stat(filepath.Join(state, replyRecordDirectory)); !os.IsNotExist(err) {
		t.Fatal("uncommitted observation published a reply", err)
	}
}

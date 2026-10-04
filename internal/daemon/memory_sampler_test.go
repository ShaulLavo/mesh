package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func TestMemorySamplerMeasuresSessionAddedAfterEmptyList(t *testing.T) {
	requireProcessMemory(t)
	dir := t.TempDir()
	now := time.Now()
	var sampler memorySampler
	if got := sampler.sizesFor(dir, nil, now); len(got) != 0 {
		t.Fatalf("empty sample = %v", got)
	}
	session := memorySession(t, dir, "7K3D")
	if got := sampler.sizesFor(dir, []storage.Session{session}, now.Add(time.Second)); got["7K3D"] == 0 {
		t.Fatalf("new live session remains unknown: %v", got)
	}
}

func TestMemorySamplerAddsMissingSizesWithoutChangingCachedValuesOrDeadline(t *testing.T) {
	requireProcessMemory(t)
	dir := t.TempDir()
	now := time.Now()
	previous := map[string]uint64{"OLD1": 123}
	sampler := memorySampler{sampled: now, sizes: previous}
	sessions := []storage.Session{
		{ID: "OLD1", State: storage.StateRunning},
		memorySession(t, dir, "NEW1"),
	}
	got := sampler.sizesFor(dir, sessions, now.Add(time.Second))
	if got["OLD1"] != 123 || got["NEW1"] == 0 {
		t.Fatalf("expanded sample = %v", got)
	}
	if len(previous) != 1 || !sampler.sampled.Equal(now) {
		t.Fatal("adding a session changed a prior reader's map or extended the cache deadline")
	}
	if got := sampler.sizesFor(dir, sessions, now.Add(memorySampleTTL)); got["OLD1"] != 0 {
		t.Fatalf("expired cached value survived a full refresh: %v", got)
	}
}

func TestMemorySamplerMeasuresMetadataPublishedAfterFirstList(t *testing.T) {
	requireProcessMemory(t)
	dir := t.TempDir()
	now := time.Now()
	var sampler memorySampler
	sessions := []storage.Session{{ID: "7K3D", State: storage.StateDetached}}
	if got := sampler.sizesFor(dir, sessions, now); got["7K3D"] != 0 {
		t.Fatalf("missing metadata produced a size: %v", got)
	}
	memorySession(t, dir, "7K3D")
	if got := sampler.sizesFor(dir, sessions, now.Add(time.Second)); got["7K3D"] == 0 {
		t.Fatalf("published metadata remains unknown: %v", got)
	}
}

func memorySession(t *testing.T, dir, id string) storage.Session {
	t.Helper()
	root := filepath.Join(dir, id)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(root, worker.Meta{ID: id, PID: os.Getpid(), State: worker.StateRunning, Command: []string{"memory-fixture"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return storage.Session{ID: storage.SessionID(id), State: storage.StateRunning}
}

func requireProcessMemory(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process memory sampling requires Linux procfs or Darwin ps")
	}
}

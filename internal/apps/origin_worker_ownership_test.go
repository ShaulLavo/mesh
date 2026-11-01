package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type postLaunchAppWorkers struct {
	*fakeWorkers
	mu            sync.Mutex
	failure       error
	live          map[string]string
	label         string
	root          string
	finds         []string
	stops         []string
	sourceAtStart string
	sourceErr     error
}

func (w *postLaunchAppWorkers) Start(_ context.Context, label, command, root string, env []string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.label, w.root = label, root
	sourceRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("fixture source root: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()
	source, err := sourceRoot.ReadFile("index.html")
	w.sourceAtStart, w.sourceErr = string(source), err
	w.live["7K3D"] = root
	return "7K3D", w.failure
}

func (w *postLaunchAppWorkers) Find(_ context.Context, label string) (string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finds = append(w.finds, label)
	return "", false, nil
}

func (w *postLaunchAppWorkers) Stop(_ context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stops = append(w.stops, id)
	delete(w.live, id)
	return nil
}

func TestAppPostLaunchFailureStopsKnownWorker(t *testing.T) {
	f := newAppFixture(t)
	failure := errors.New("synthetic post-launch publication failure")
	workers := &postLaunchAppWorkers{fakeWorkers: f.workers, failure: failure, live: map[string]string{}}
	f.origin.config.Workers = workers
	t.Cleanup(func() {
		workers.mu.Lock()
		clear(workers.live)
		workers.mu.Unlock()
		f.origin.mu.Lock()
		defer f.origin.mu.Unlock()
		if len(f.origin.ops) != 0 {
			t.Errorf("operation gates remain after Handle returned: %d", len(f.origin.ops))
		}
		t.Log("cleanup cleared fixture-owned synthetic workers; no operation gates remain")
	})
	upload, digest := uploadSource(t, f, sourceFixture(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.origin.Handle(ctx, Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest, Setup: "synthetic setup"})
	if !errors.Is(err, failure) {
		t.Fatalf("Handle error = %v, want the post-launch failure", err)
	}

	workers.mu.Lock()
	defer workers.mu.Unlock()
	if !strings.HasPrefix(workers.label, "app-setup ") || workers.root == "" {
		t.Fatalf("create did not reach setup start: label=%q root=%q", workers.label, workers.root)
	}
	if workers.sourceErr != nil || workers.sourceAtStart != "original page" {
		t.Fatalf("setup did not receive the extracted source: %q, %v", workers.sourceAtStart, workers.sourceErr)
	}
	appID := strings.TrimPrefix(workers.label, "app-setup ")
	if !slices.Contains(workers.finds, workers.label) || !slices.Contains(workers.finds, "app "+appID) {
		t.Fatalf("rollback did not perform both successful empty label lookups: %q", workers.finds)
	}
	_, alive := workers.live["7K3D"]
	_, rootErr := os.Stat(filepath.Join(workers.root, "index.html"))
	t.Logf("Start returned ID 7K3D plus sentinel error; rollback finds=%q stops=%q alive=%t workspace error=%v", workers.finds, workers.stops, alive, rootErr)
	if alive && errors.Is(rootErr, os.ErrNotExist) {
		t.Fatal("rollback deleted the workspace while known post-launch worker 7K3D remained alive; empty catalog discovery did not establish exit")
	}
	if alive && rootErr != nil {
		t.Fatalf("cannot establish retained workspace for live worker 7K3D: %v", rootErr)
	}
	if !alive && !slices.Contains(workers.stops, "7K3D") {
		t.Fatal("known worker vanished without an exact-ID stop")
	}
}

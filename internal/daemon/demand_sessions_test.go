package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func labelledTestLifecycle(t *testing.T, catalog *lifecycleTestCatalog, launch launchWorker) *lifecycle {
	t.Helper()
	return mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: t.TempDir(),
		Launch:      launch,
	})
}

func TestLabelledPublicationFailureKeepsWorkerIdentity(t *testing.T) {
	publishErr := errors.New("catalog is read-only")
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{reconcileErr: publishErr}, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, "/work", nil)
	if !errors.Is(err, publishErr) {
		t.Fatalf("startLabelled error = %v, want the publication failure", err)
	}
	if id != "7K3D" {
		t.Fatalf("launched worker 7K3D was discarded on publication failure (got %q)", id)
	}
}

func TestLabelledLaunchFailureOwnsNoWorker(t *testing.T) {
	launchErr := errors.New("fork: resource temporarily unavailable")
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{}, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{}, launchErr
	})
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, "/work", nil)
	if !errors.Is(err, launchErr) || id != "" {
		t.Fatalf("startLabelled = %q, %v; want no session and the launch failure", id, err)
	}
}

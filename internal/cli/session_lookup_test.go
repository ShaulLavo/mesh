package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/worker"
)

func TestFindProbesOnlyTheMatchingSession(t *testing.T) {
	setupCommandTestHost(t)
	for i := range 12 {
		writeLocalSessionDir(t, fmt.Sprintf("%04d", i), worker.StateDetached)
	}
	var probes atomic.Int32
	setWorkerProbe(t, func(string) error { probes.Add(1); return nil })
	current, err := Find("0005")
	if err != nil || current.ID != "0005" || current.Liveness != LivenessAlive {
		t.Fatalf("matching session = %+v, %v", current, err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("Find probed %d sessions, want 1", got)
	}
}

func TestFindAbsentSessionDoesNotProbeOtherWorkers(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "7K3D", worker.StateDetached)
	var probes atomic.Int32
	setWorkerProbe(t, func(string) error { probes.Add(1); return nil })
	if _, err := Find("91AZ"); !errors.Is(err, ErrNoLocalSession) {
		t.Fatalf("missing session error = %v", err)
	}
	if got := probes.Load(); got != 0 {
		t.Fatalf("absent Find probed %d other workers, want 0", got)
	}
}

func TestAwaitHibernatedProbesOnlyItsTarget(t *testing.T) {
	setupCommandTestHost(t)
	for i := range 9 {
		writeLocalSessionDir(t, fmt.Sprintf("%04d", i), worker.StateRunning)
	}
	var probes, targetProbes atomic.Int32
	setWorkerProbe(t, func(socket string) error {
		probes.Add(1)
		if filepath.Base(filepath.Dir(socket)) == "0000" && targetProbes.Add(1) == 3 {
			return syscall.ENOENT
		}
		return nil
	})
	app := &application{}
	current, err := app.awaitHibernated(t.Context(), resolvedSession{local: &Session{Meta: worker.Meta{ID: "0000"}}})
	if err != nil || current.local == nil || current.local.Liveness != LivenessGone {
		t.Fatalf("hibernated target = %+v, %v", current, err)
	}
	if got := probes.Load(); got != 3 {
		t.Fatalf("three hibernation polls probed %d sessions, want 3", got)
	}
}

func TestFindSharesListVisibilityRules(t *testing.T) {
	for _, hidden := range []string{"forgotten", "launching", "missing meta", "broken meta", "not directory"} {
		t.Run(hidden, func(t *testing.T) {
			setupCommandTestHost(t)
			writeLocalSessionDir(t, "7K3D", worker.StateDetached)
			dir, err := paths.SessionDir("7K3D")
			if err != nil {
				t.Fatal(err)
			}
			switch hidden {
			case "forgotten":
				err = os.WriteFile(paths.Forgotten(dir), nil, 0o600)
			case "launching":
				err = os.WriteFile(paths.Launching(dir), nil, 0o600)
			case "missing meta":
				err = os.Remove(paths.Meta(dir))
			case "broken meta":
				err = os.WriteFile(paths.Meta(dir), []byte("{"), 0o600)
			case "not directory":
				err = os.RemoveAll(dir)
				if err == nil {
					err = os.WriteFile(dir, nil, 0o600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			var probes atomic.Int32
			setWorkerProbe(t, func(string) error { probes.Add(1); return nil })
			if _, err := Find("7k3d"); !errors.Is(err, ErrNoLocalSession) {
				t.Errorf("hidden session error = %v", err)
			}
			rows, err := List()
			if err != nil || len(rows) != 0 || probes.Load() != 0 {
				t.Fatalf("hidden rows = %+v, %v, probes = %d", rows, err, probes.Load())
			}
		})
	}
}

func TestFindCannotReadOutsideTheSessionRoot(t *testing.T) {
	setupCommandTestHost(t)
	stateDir := os.Getenv("MESH_STATE_DIR")
	if err := worker.WriteMeta(stateDir, worker.Meta{ID: "..", State: worker.StateRunning}); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	setWorkerProbe(t, func(string) error { probes.Add(1); return nil })
	for _, id := range []string{"..", ".", "", "../7K3D", stateDir} {
		if _, err := Find(id); !errors.Is(err, ErrNoLocalSession) {
			t.Errorf("Find(%q) error = %v, want ErrNoLocalSession", id, err)
		}
	}
	if got := probes.Load(); got != 0 {
		t.Fatalf("invalid IDs caused %d probes", got)
	}
}

func TestListProbesWithABoundedWorkerPool(t *testing.T) {
	setupCommandTestHost(t)
	for i := range 20 {
		writeLocalSessionDir(t, fmt.Sprintf("%04d", i), worker.StateDetached)
	}
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	var active, maximum, probes atomic.Int32
	setWorkerProbe(t, func(string) error {
		probes.Add(1)
		current := active.Add(1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return nil
	})
	done := make(chan error, 1)
	go func() {
		rows, err := List()
		if err == nil && len(rows) != 20 {
			err = fmt.Errorf("listed %d sessions, want 20", len(rows))
		}
		done <- err
	}()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ready := 0
waiting:
	for ready < 8 {
		select {
		case <-started:
			ready++
		case <-deadline.C:
			t.Errorf("only %d probes started concurrently, want 8", ready)
			break waiting
		}
	}
	if got := active.Load(); got != 8 {
		t.Errorf("active probes = %d, want 8", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got > 8 {
		t.Errorf("maximum active probes = %d, want at most 8", got)
	}
	if got := probes.Load(); got != 20 {
		t.Errorf("total probes = %d, want 20", got)
	}
}

func TestFindNormalizesIDsAndIgnoresDirectorySymlinks(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "7K3D", worker.StateDetached)
	dir, err := paths.SessionDir("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	setWorkerProbe(t, func(string) error { probes.Add(1); return nil })
	current, err := Find("7k3d")
	if err != nil || current.ID != "7K3D" || probes.Load() != 1 {
		t.Fatalf("lowercase Find = %+v, %v, probes = %d", current, err, probes.Load())
	}
	link, err := paths.SessionDir("91AZ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Find("91AZ"); !errors.Is(err, ErrNoLocalSession) || probes.Load() != 1 {
		t.Fatalf("directory symlink error = %v, probes = %d", err, probes.Load())
	}
}

func TestFindKeepsDirectoryFailuresDistinctFromMissingSessions(t *testing.T) {
	setupCommandTestHost(t)
	stateFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(stateFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_STATE_DIR", stateFile)
	_, err := Find("7K3D")
	if err == nil || errors.Is(err, ErrNoLocalSession) {
		t.Fatalf("invalid state directory error = %v, want a directory failure", err)
	}
}

package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type transactionRuntime struct {
	mu                 sync.Mutex
	sources            map[string]Source
	launches           []Launch
	launchError        error
	publishBeforeError bool
}

func (r *transactionRuntime) Inspect(_ context.Context, id string) (Source, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	source, ok := r.sources[id]
	if !ok {
		return Source{}, os.ErrNotExist
	}
	return source, nil
}

func (r *transactionRuntime) Launch(_ context.Context, launch Launch) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launches = append(r.launches, launch)
	if r.launchError == nil || r.publishBeforeError {
		r.sources[launch.ID] = Source{ID: launch.ID, State: "detached", Cwd: launch.Cwd, Command: launch.Command, RecoveredFrom: launch.SourceID, Published: true}
	}
	return r.launchError
}

func (r *transactionRuntime) ConfigureCommand(context.Context, string, *Command) error {
	return errors.New("unexpected live recipe request")
}

func transactionFixture(t *testing.T) (Config, *transactionRuntime) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "7K3D"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := &transactionRuntime{sources: map[string]Source{"7K3D": {ID: "7K3D", State: "interrupted", Cwd: root, Command: []string{"dangerous-program", "argument with spaces"}, Shell: "/bin/sh", Published: true}}}
	return Config{SessionsDir: root, HostID: "host", Runtime: runtime}, runtime
}

func TestRecoveryConcurrentClientsCreateOneReplacement(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	results := make(chan Result, 12)
	errors := make(chan error, 12)
	var callers sync.WaitGroup
	for range 12 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
			results <- result
			errors <- err
		}()
	}
	callers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for result := range results {
		if id != "" && id != result.SessionID {
			t.Fatalf("multiple replacements: %s and %s", id, result.SessionID)
		}
		id = result.SessionID
	}
	if id == "7K3D" || len(runtime.launches) != 1 {
		t.Fatalf("replacement=%s launches=%d", id, len(runtime.launches))
	}
	if command := runtime.launches[0].Command; len(command) != 1 || command[0] != "/bin/sh" {
		t.Fatalf("default recovery executed saved program: %q", command)
	}
	if _, err := os.Stat(filepath.Join(cfg.SessionsDir, "7K3D")); err != nil {
		t.Fatalf("previous attempt removed: %v", err)
	}
}

func TestRecoveryDroppedAcknowledgementReconcilesPublishedWorker(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	runtime.launchError = errors.New("reply lost")
	runtime.publishBeforeError = true
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("first result = %v", err)
	}
	result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Existing || len(runtime.launches) != 1 {
		t.Fatalf("retry result=%+v launches=%d", result, len(runtime.launches))
	}
}

func TestRecoveryUncertainLaunchNeverSpawnsAgain(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	runtime.launchError = errors.New("caller died after dispatch")
	for range 3 {
		if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); !errors.Is(err, ErrUncertain) {
			t.Fatalf("result = %v", err)
		}
	}
	if len(runtime.launches) != 1 {
		t.Fatalf("uncertain launch spawned %d times", len(runtime.launches))
	}
	id, err := ReplacementID(filepath.Join(cfg.SessionsDir, "7K3D"), cfg.HostID, "7K3D")
	if err != nil || id == "" {
		t.Fatalf("lost reserved ID: %q %v", id, err)
	}
}

func TestRecoveryProvenSpawnFailureRetriesSameReservation(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	runtime.launchError = &LaunchFailure{Err: errors.New("executable missing")}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); err == nil || errors.Is(err, ErrUncertain) {
		t.Fatalf("first result = %v", err)
	}
	runtime.launchError = nil
	result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.launches) != 2 || runtime.launches[0].ID != result.SessionID || runtime.launches[1].ID != result.SessionID {
		t.Fatalf("retry used a different reservation: %+v", runtime.launches)
	}
}

func TestRecoveryMissingDirectoryFallsBackOnlyForShell(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	source := runtime.sources["7K3D"]
	source.Cwd = filepath.Join(cfg.SessionsDir, "gone", "project")
	runtime.sources[source.ID] = source
	if _, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionCommand}); err == nil {
		t.Fatal("command restart changed directory silently")
	}
	result, err := Recover(context.Background(), cfg, Request{SessionID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cwd != cfg.SessionsDir || result.OriginalCwd != source.Cwd {
		t.Fatalf("directory fallback = %+v", result)
	}
}

func TestRecoveryExplicitRecipePreservesArgumentsAndCheckpoint(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	dir := filepath.Join(cfg.SessionsDir, "7K3D")
	recipe := &Command{Argv: []string{"sh", "-lc", "printf '%s' 'quoted argument'"}, Cwd: cfg.SessionsDir}
	if err := ConfigureCommand(context.Background(), cfg, "7K3D", recipe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recipe mutated checkpoint: %v", err)
	}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: ActionCommand}); err != nil {
		t.Fatal(err)
	}
	if runtime.launches[0].Command[2] != recipe.Argv[2] {
		t.Fatalf("recipe lost quoting: %q", runtime.launches[0].Command)
	}
}

func TestRecoveryLockHonorsCancellation(t *testing.T) {
	cfg, _ := transactionFixture(t)
	unlock, err := lockSource(context.Background(), filepath.Join(cfg.SessionsDir, "7K3D"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Recover(ctx, cfg, Request{SessionID: "7K3D"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lock: %v", err)
	}
}

func TestRecoveryUnknownCheckpointVersionAllowsExplicitShell(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	path := filepath.Join(cfg.SessionsDir, "7K3D", "recovery.json")
	contents := []byte(`{"version":999,"hostId":"host","sessionId":"7K3D"}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); err == nil {
		t.Fatal("default recovery accepted unknown checkpoint")
	}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: ActionShell}); err != nil {
		t.Fatal(err)
	}
	if runtime.launches[0].Cwd != cfg.SessionsDir || runtime.launches[0].Command[0] != "/bin/sh" {
		t.Fatalf("explicit shell did not use launch fallback: %+v", runtime.launches[0])
	}
	retained, err := os.ReadFile(path) //nolint:gosec // path is a fixture checkpoint inside t.TempDir
	if err != nil || string(retained) != string(contents) {
		t.Fatalf("unknown checkpoint changed: %q %v", retained, err)
	}
}

func TestRecoveryNewBootReconcilesUndispatchedWorkerReservation(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	cfg.BootID = "first-boot"
	runtime.launchError = errors.New("crash between dispatch intent and exec.Start")
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("first boot result = %v", err)
	}
	reserved := runtime.launches[0].ID
	runtime.launchError = nil
	cfg.BootID = "second-boot"
	result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != reserved || len(runtime.launches) != 2 {
		t.Fatalf("new boot did not reuse safe reservation: %+v launches=%+v", result, runtime.launches)
	}
}

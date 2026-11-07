package worker

import (
	"errors"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/testenv"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExecutionRegressionCaptureInitFailurePublishesNoStart(t *testing.T) {
	root := testenv.SocketTempDir(t)
	ref, err := Reserve(filepath.Join(root, "s"), session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	j, err := execution.Open(root, BootID(), "review-init")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	key := execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "init"}
	effect := filepath.Join(root, "child-started")
	intent, err := execution.Normalize(execution.Intent{Kind: "pty", Command: []string{"/bin/sh", "-c", "touch \"$1\"", "review", effect}, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	env := ExecutionEnvironment(os.Environ(), key.Host, ref, intent.Term)
	r, owner, err := j.Reserve(t.Context(), key, intent, func() (execution.Binding, error) { return execution.NewBinding(key, intent, ref, env), nil })
	if err != nil || !owner {
		t.Fatal(owner, err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, intent); err != nil {
		t.Fatal(err)
	}
	// A deterministic initial stream-directory creation failure, not a running-child failure.
	if err := os.WriteFile(filepath.Join(root, "execution", "attempts", ref.Instance, "pty"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	_, runErr := Run(Config{ID: ref.ID, HostID: key.Host, Dir: filepath.Join(root, "s", ref.ID), Reservation: &ref, Execution: &r.Binding, Command: intent.Command, Cwd: root, Env: env, Cols: intent.Cols, Rows: intent.Rows, Label: "agent request " + ref.Instance})
	if runErr == nil {
		t.Fatal("capture init failure did not arm")
	}
	if _, err := os.Stat(effect); !os.IsNotExist(err) {
		t.Fatal("child unexpectedly started", err)
	}
	terminal, lookupErr := j.Lookup(t.Context(), key)
	if lookupErr != nil || terminal.Result == nil {
		t.Fatalf("definite pre-child failure left accepted key unresolved: phase=%s result=%+v lookup=%v worker=%v", terminal.Phase, terminal.Result, lookupErr, runErr)
	}
	if terminal.Result.Termination.Kind != "not_started" {
		t.Fatalf("wrong termination: %+v", terminal.Result)
	}
}

func TestExecutionRegressionExitObserverErrorHasNoReapFallback(t *testing.T) {
	if err := observeChildExit(os.Getpid()); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("non-child observation did not fail closed: %v", err)
	}
}

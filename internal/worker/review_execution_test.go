package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

func TestExecutionReviewKernelCwdHelper(t *testing.T) {
	if os.Getenv("MESH_REVIEW_CWD_HELPER") != "1" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(2)
	}
	fmt.Println(cwd)
	os.Exit(0)
}

func TestExecutionReviewLexicalSymlinkCwd(t *testing.T) {
	root := t.TempDir()
	physical, alias := filepath.Join(root, "real"), filepath.Join(root, "alias")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	ref, err := Reserve(filepath.Join(root, "s"), session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := execution.Open(root, BootID(), "review")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	key := execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "symlink"}
	intent, err := execution.Normalize(execution.Intent{Kind: "pty", Command: []string{"/bin/true"}, Cwd: alias})
	if err != nil {
		t.Fatal(err)
	}
	env := ExecutionEnvironment([]string{"PATH=/bin", "MESH_REVIEW_CWD_HELPER=1"}, key.Host, ref, intent.Term)
	r, _, err := journal.Reserve(t.Context(), key, intent, func() (execution.Binding, error) { return execution.NewBinding(key, intent, ref, env), nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Dispatch(t.Context(), r.Binding, intent); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: executable, Args: []string{executable, "-test.run=^TestExecutionReviewKernelCwdHelper$"}, Dir: alias, Env: env}
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	actualCwd := strings.TrimSpace(string(output))
	args := launchArguments(LaunchConfig{Reservation: &ref, Execution: &r.Binding, Cwd: intent.Cwd, Command: intent.Command, Cols: intent.Cols, Rows: intent.Rows}, ref.ID, filepath.Join(root, "s", ref.ID))
	index := slices.Index(args, "--request-cwd")
	if index < 0 || index+1 >= len(args) || args[index+1] != alias {
		t.Fatalf("hidden worker lost accepted lexical cwd: %q", args)
	}
	acceptedCwd := args[index+1]
	attempt, err := prepareExecution(Config{ID: ref.ID, HostID: key.Host, Dir: filepath.Join(root, "s", ref.ID), Reservation: &ref, Execution: &r.Binding, Command: intent.Command, Cwd: acceptedCwd, Env: env, Cols: intent.Cols, Rows: intent.Rows, Label: "agent request " + ref.Instance})
	if attempt != nil {
		defer func() { _ = attempt.Close() }()
	}
	t.Logf("accepted cwd=%s; hidden worker os.Getwd=%s; claim=%v", alias, actualCwd, err)
	if err != nil {
		t.Fatalf("valid lexical symlink cwd cannot claim its accepted dispatch: %v", err)
	}
}

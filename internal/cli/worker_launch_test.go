package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/session/execution"
)

func TestExecutionWorkerDirectoryPreservesLexicalIntent(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(cwd, alias); err != nil {
		t.Fatal(err)
	}
	got, err := workerLaunchDirectory(&execution.Binding{}, alias)
	if err != nil || got != alias {
		t.Fatalf("accepted alias lost: %s %v", got, err)
	}
	plain, err := workerLaunchDirectory(nil, "")
	if err != nil || plain != cwd {
		t.Fatalf("ordinary worker directory changed: %s %v", plain, err)
	}
}

func TestExecutionWorkerDirectoryRejectsMissingOrDifferentTarget(t *testing.T) {
	for _, requested := range []string{"", "relative", filepath.Join(t.TempDir(), "absent"), t.TempDir()} {
		if _, err := workerLaunchDirectory(&execution.Binding{}, requested); err == nil {
			t.Fatalf("accepted directory not matching actual process cwd: %q", requested)
		}
	}
}

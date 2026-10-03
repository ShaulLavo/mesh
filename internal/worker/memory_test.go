package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/xpty"
)

type startFailurePTY struct {
	xpty.Pty
	err error
}

func (p startFailurePTY) Start(*exec.Cmd) error { return p.err }

func TestSessionStartReportsRawErrnoAndPath(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EACCES, syscall.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shell")
			command := exec.Command(path, "SECRET_ARG") //nolint:gosec // the fixture PTY never executes the command
			command.Env = []string{"SECRET_ENV=value"}
			cause := &os.PathError{Op: "fork/exec", Path: path, Err: errno}
			err := startSession(startFailurePTY{err: cause}, command)
			t.Logf("child-start diagnostic: %v", err)
			var original *os.PathError
			if !errors.Is(err, errno) || !errors.As(err, &original) || original != cause {
				t.Fatalf("child-start diagnostic lost the original error: %v", err)
			}
			for _, fact := range []string{fmt.Sprintf("path=%q", path), fmt.Sprintf("errno=%d", errno), "stage=unavailable"} {
				if !strings.Contains(err.Error(), fact) {
					t.Errorf("child-start diagnostic lacks %q: %v", fact, err)
				}
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("child-start diagnostic disclosed arguments or environment: %v", err)
			}
		})
	}
}

func TestSessionStartPreservesOtherErrorsAndSuccess(t *testing.T) {
	command := exec.Command("fixture-shell", "SECRET_ARG")
	cause := errors.New("fixture PTY start failure")
	err := startSession(startFailurePTY{err: cause}, command)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "errno=unavailable") || !strings.Contains(err.Error(), "stage=unavailable") {
		t.Fatalf("unclassified child-start error = %v", err)
	}
	if err := startSession(startFailurePTY{}, command); err != nil {
		t.Fatalf("successful child-start = %v", err)
	}
}

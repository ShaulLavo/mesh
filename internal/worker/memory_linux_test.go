//go:build linux

package worker

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/x/xpty"
)

func TestSessionCommandKeepsSystemHugePagePolicy(t *testing.T) {
	setHugePages(false)
	t.Cleanup(func() { setHugePages(true) })
	if got := thpEnabled(t, "/proc/self/status"); got != "0" {
		t.Skipf("kernel does not report the THP opt-out (THP_enabled %q)", got)
	}

	pty, err := xpty.NewPty(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close() //nolint:errcheck // test cleanup
	cmd := exec.Command("grep", "^THP_enabled:", "/proc/self/status")
	if err := startSession(pty, cmd); err != nil {
		t.Fatal(err)
	}
	if p, ok := pty.(interface{ Slave() *os.File }); ok {
		_ = p.Slave().Close()
	}
	var output bytes.Buffer
	_, _ = io.Copy(&output, pty)
	_ = cmd.Wait()

	if !strings.Contains(output.String(), "THP_enabled:\t1") {
		t.Fatalf("session command inherited the worker's THP opt-out: %q", output.String())
	}
	if got := thpEnabled(t, "/proc/self/status"); got != "0" {
		t.Fatalf("worker THP_enabled = %q after starting the command, want 0", got)
	}
}

func thpEnabled(t *testing.T, path string) string {
	t.Helper()
	status, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, "THP_enabled:"); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

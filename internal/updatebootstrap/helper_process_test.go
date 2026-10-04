package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHelperProcessChild(t *testing.T) {
	if os.Getenv("MESH_TEST_HELPER_PROCESS_CHILD") != "1" {
		return
	}
	time.Sleep(time.Minute)
}

func TestHelperProcessProbeBindsRealPIDAndMappedImage(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := imageDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	image := filepath.Join(root, "mesh")
	data, err := os.ReadFile(filepath.Clean(executable))
	if err != nil {
		t.Fatal(err)
	}
	if err = writeProcessFixture(image, data); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("./mesh", "-test.run=^TestHelperProcessChild$")
	child.Dir = root
	child.Env = append(os.Environ(), "MESH_TEST_HELPER_PROCESS_CHILD=1")
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	tools := filepath.Join(root, "tools")
	if err = os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	tool, output, kind := "systemctl", strconv.Itoa(child.Process.Pid), "systemd"
	if runtime.GOOS == "darwin" {
		tool, kind = "launchctl", "launchd"
		output = fmt.Sprintf("gui/501/dev.shaulavo.mesh-update-helper = {\n\tstate = running\n\tpid = %d\n\tresource coalition = {\n\t\tstate = active\n\t}\n}\n", child.Process.Pid)
	}
	if err = writeProcessFixture(filepath.Join(tools, tool), []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	probe := HelperProcessProbe(updateinstall.HelperConfig{Kind: kind, Domain: "gui/501"})
	installed := updateinstall.HelperInstallation{Executable: image, Digest: digest}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pid, err := probe(ctx, installed)
	if err != nil || pid != child.Process.Pid {
		t.Fatalf("actual mapped helper PID = %d, %v", pid, err)
	}
	other := filepath.Join(root, "another-mesh")
	if err = writeProcessFixture(other, data); err != nil {
		t.Fatal(err)
	}
	installed.Executable = other
	if _, err = probe(ctx, installed); err == nil {
		t.Fatal("same digest on another inode was accepted as the executing helper")
	}
	installed.Executable, installed.Digest = image, digest[:len(digest)-1]+"z"
	if _, err = probe(ctx, installed); !errors.Is(err, release.ErrExecutableChecksumMismatch) {
		t.Fatalf("mapped helper digest mismatch = %v", err)
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, err = probe(cancelled, installed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled helper process probe = %v", err)
	}
	installed.Digest = digest
	t.Setenv("MESH_TEST_HELPER_PID_COUNTER", filepath.Join(root, "pid-counter"))
	changedOutput := strings.ReplaceAll(output, strconv.Itoa(child.Process.Pid), strconv.Itoa(child.Process.Pid+1))
	body := "#!/bin/sh\nif [ -e \"$MESH_TEST_HELPER_PID_COUNTER\" ]; then\nprintf '%s' '" + changedOutput + "'\nelse\n: > \"$MESH_TEST_HELPER_PID_COUNTER\"\nprintf '%s' '" + output + "'\nfi\n"
	if err = writeProcessFixture(filepath.Join(tools, tool), []byte(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = probe(ctx, installed); err == nil || !strings.Contains(err.Error(), "changed during verification") {
		t.Fatalf("changed service-owned helper PID = %v", err)
	}
	if err = writeProcessFixture(filepath.Join(tools, tool), []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n")); err != nil {
		t.Fatal(err)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if _, err = probe(ctx, installed); err == nil {
		t.Fatal("exited helper process was accepted through its stale service PID")
	}
}

func TestLaunchdHelperPIDRealCapture(t *testing.T) {
	capture, err := os.ReadFile("testdata/launchctl-print-helper.txt")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := launchdHelperPID(string(capture))
	if err != nil || pid != 91306 {
		t.Fatalf("real capture helper PID = %d, %v; want 91306", pid, err)
	}
}

func TestLaunchdHelperPIDTopLevelFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields string
		want   int
	}{
		{name: "running", fields: "\tstate = running\n\tpid = 123\n", want: 123},
		{name: "pid before state", fields: "\tpid = 123\n\tstate = running\n", want: 123},
		{name: "nested state after", fields: "\tstate = running\n\tpid = 123\n\tendpoint = {\n\t\tstate = active\n\t}\n", want: 123},
		{name: "nested pid after", fields: "\tstate = running\n\tpid = 123\n\tendpoint = {\n\t\tpid = 456\n\t}\n", want: 123},
		{name: "nested fields before", fields: "\tendpoint = {\n\t\tstate = active\n\t\tpid = 456\n\t}\n\tstate = running\n\tpid = 123\n", want: 123},
		{name: "nested malformed fields", fields: "\tstate = running\n\tpid = 123\n\tendpoint = {\n\t\tstate =\n\t\tpid = invalid\n\t}\n", want: 123},
		{name: "missing fields"},
		{name: "missing state", fields: "\tpid = 123\n"},
		{name: "missing pid", fields: "\tstate = running\n"},
		{name: "stopped", fields: "\tstate = waiting\n\tpid = 123\n"},
		{name: "stopped with nested running", fields: "\tstate = waiting\n\tpid = 123\n\tendpoint = {\n\t\tstate = running\n\t}\n"},
		{name: "nested only", fields: "\tendpoint = {\n\t\tstate = running\n\t\tpid = 123\n\t}\n"},
		{name: "nested pid only", fields: "\tstate = running\n\tendpoint = {\n\t\tpid = 123\n\t}\n"},
		{name: "nested state only", fields: "\tpid = 123\n\tendpoint = {\n\t\tstate = running\n\t}\n"},
		{name: "zero pid", fields: "\tstate = running\n\tpid = 0\n"},
		{name: "negative pid", fields: "\tstate = running\n\tpid = -123\n"},
		{name: "signed pid", fields: "\tstate = running\n\tpid = +123\n"},
		{name: "invalid pid", fields: "\tstate = running\n\tpid = invalid\n"},
		{name: "overflow pid", fields: "\tstate = running\n\tpid = 18446744073709551616\n"},
		{name: "duplicate pid", fields: "\tstate = running\n\tpid = 123\n\tpid = 123\n"},
		{name: "contradictory pid", fields: "\tstate = running\n\tpid = 123\n\tpid = 456\n"},
		{name: "duplicate state", fields: "\tstate = running\n\tstate = running\n\tpid = 123\n"},
		{name: "contradictory state running last", fields: "\tstate = waiting\n\tstate = running\n\tpid = 123\n"},
		{name: "contradictory state waiting last", fields: "\tstate = running\n\tstate = waiting\n\tpid = 123\n"},
		{name: "malformed state before valid", fields: "\tstate =\n\tstate = running\n\tpid = 123\n"},
		{name: "malformed state after valid", fields: "\tstate = running\n\tstate = running extra\n\tpid = 123\n"},
		{name: "malformed pid before valid", fields: "\tstate = running\n\tpid =\n\tpid = 123\n"},
		{name: "malformed pid after valid", fields: "\tstate = running\n\tpid = 123\n\tpid = 456 extra\n"},
		{name: "invalid pid separator", fields: "\tstate = running\n\tpid => 123\n"},
		{name: "unindented", fields: "state = running\npid = 123\n"},
		{name: "space indented", fields: "    state = running\n    pid = 123\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pid, err := launchdHelperPID("gui/501/dev.shaulavo.mesh-update-helper = {\n" + test.fields + "}\n")
			if test.want == 0 {
				if err == nil || pid != 0 {
					t.Fatalf("unready or ambiguous helper PID = %d, %v", pid, err)
				}
				return
			}
			if err != nil || pid != test.want {
				t.Fatalf("helper PID = %d, %v; want %d", pid, err, test.want)
			}
		})
	}
}

func TestHelperProcessLaunchdQueryFailure(t *testing.T) {
	root := t.TempDir()
	body := "#!/bin/sh\nprintf '\\tstate = running\\n\\tpid = 123\\n'\nexit 1\n"
	if err := writeProcessFixture(filepath.Join(root, "launchctl"), []byte(body)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	pid, err := helperServicePID(t.Context(), updateinstall.HelperConfig{Kind: "launchd", Domain: "gui/501"})
	if pid != 0 || err == nil || !strings.Contains(err.Error(), "inspect helper service process") {
		t.Fatalf("failed launchctl query helper PID = %d, %v", pid, err)
	}
}

func TestHelperLaunchdTargetRejectsForeignDomains(t *testing.T) {
	for _, domain := range []string{"system", "gui/1/other", "gui/-1", "user/1\n", "other/501"} {
		if _, err := helperLaunchdTarget(domain); err == nil {
			t.Fatalf("unsafe helper domain accepted: %q", domain)
		}
	}
	target, err := helperLaunchdTarget("gui/501")
	if err != nil || target != "gui/501/dev.shaulavo.mesh-update-helper" {
		t.Fatalf("helper launchd target = %q, %v", target, err)
	}
}

func writeProcessFixture(path string, data []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open process fixture directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(path)
	if err = root.WriteFile(name, data, 0600); err != nil {
		return fmt.Errorf("write process fixture: %w", err)
	}
	if err = root.Chmod(name, 0700); err != nil {
		return fmt.Errorf("make process fixture executable: %w", err)
	}
	return nil
}

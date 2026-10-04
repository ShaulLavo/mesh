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
		output = fmt.Sprintf("state = running\npid = %d\n", child.Process.Pid)
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

func TestLaunchdHelperPIDRejectsStoppedAndAmbiguousJobs(t *testing.T) {
	for _, output := range []string{"pid = 123", "state = waiting\npid = 123", "state = running\npid = 0", "state = running\npid = 123\npid = 456"} {
		if _, err := launchdHelperPID(output); err == nil {
			t.Fatal("unready or ambiguous service PID was accepted")
		}
	}
	if pid, err := launchdHelperPID("state = running\npid = 123"); err != nil || pid != 123 {
		t.Fatalf("running service PID = %d, %v", pid, err)
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

package update

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func requireNativeAdoption(t *testing.T) {
	t.Helper()
	if os.Getenv("MESH_ADOPTION_NATIVE") != "1" || runtime.GOOS != "linux" {
		t.Skip("set MESH_ADOPTION_NATIVE=1 for published process fixtures")
	}
}

func TestAdoptionNativeHelperFailureRestoresOwnedState(t *testing.T) {
	requireNativeAdoption(t)
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) { testNativeHelperFailure(t, existing) })
	}
}

func testNativeHelperFailure(t *testing.T, existing bool) {
	f := newAdoptionNativeFixture(t, "v0.1.249", existing)
	original := f.awaitDaemon()
	var receipt []byte
	if existing {
		receipt, _ = readNativeFixtureFile(filepath.Join(f.state, "update", "helper", "installed.json"))
	}
	side, pin := f.publishCandidate()
	f.write("fail-helper-start", nil, false)
	output, err := f.sideCommand(side, pin).CombinedOutput()
	if err == nil {
		t.Fatal("failed helper start was accepted")
	}
	if !strings.Contains(string(output), "19") {
		t.Fatalf("unexpected helper failure: %s", output)
	}
	actual := f.awaitDaemon()
	if actual.PID != original.PID || actual.Health.Build.Digest != original.Health.Build.Digest {
		t.Fatal("helper failure restarted or replaced the daemon")
	}
	if _, err := updateinstall.Read(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("helper failure fabricated an installation journal")
	}
	if existing {
		after, err := readNativeFixtureFile(filepath.Join(f.state, "update", "helper", "installed.json"))
		if err != nil || string(after) != string(receipt) {
			t.Fatal("initial helper failure did not restore the exact prior receipt")
		}
	} else {
		assertNativeHelperRemoved(t, f)
	}
	f.roundTrip()
}

func assertNativeHelperRemoved(t *testing.T, f *adoptionNativeFixture) {
	t.Helper()
	for _, path := range []string{filepath.Join(f.state, "update", "helper", "installed.json"), filepath.Join(f.state, "update", "helper", "current"), filepath.Join(f.root, "home", ".config", "systemd", "user", "mesh-update-helper.service"), filepath.Join(f.state, "update", "helper", "adoption-preparation.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned initial helper resource retained after failure: %s %v", path, err)
		}
	}
}

func TestAdoptionNativeHelperResumesKilledStart(t *testing.T) {
	requireNativeAdoption(t)
	f := newAdoptionNativeFixture(t, "v0.1.249", false)
	side, pin := f.publishCandidate()
	f.write("delay-helper-probe", nil, false)
	command := f.sideCommand(side, pin)
	log, err := os.CreateTemp(f.root, "start-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	pid := f.awaitHelperPID()
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := os.Remove(filepath.Join(f.root, "delay-helper-probe")); err != nil {
		t.Fatal(err)
	}
	envelope := f.awaitAdoption(AdoptionComplete)
	if !envelope.Confirmed {
		t.Fatal("killed one-time start was not resumed by its helper")
	}
	actual := f.awaitHelperPID()
	if actual != pid {
		t.Fatal("resumption restarted its already verified helper process")
	}
	f.roundTrip()
}

func (f *adoptionNativeFixture) awaitHelperPID() int {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := readNativeFixtureFile(filepath.Join(f.root, "helper.pid"))
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && pid > 0 && syscall.Kill(pid, 0) == nil {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("actual helper process did not start within the existing readiness bound")
	return 0
}

func TestAdoptionNativeRollbackPreservesPromotedHelper(t *testing.T) {
	requireNativeAdoption(t)
	f := newAdoptionNativeFixture(t, "v0.1.249", true)
	before := f.awaitDaemon()
	side, pin := f.publishCandidate()
	f.write("fail-candidate", nil, false)
	f.startOnce(side, pin)
	pid := f.awaitHelperPID()
	f.awaitAdoption(AdoptionRolledBack)
	actual := f.awaitDaemon()
	if actual.Health.Build.Digest != before.Health.Build.Digest {
		t.Fatal("candidate failure did not restore the actual old image")
	}
	if f.awaitHelperPID() != pid {
		t.Fatal("old staging or rollback restarted the promoted helper")
	}
	f.roundTrip()
}

func TestAdoptionNativePackageMigration(t *testing.T) {
	requireNativeAdoption(t)
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) { testNativePackageMigration(t, fail) })
	}
}

func testNativePackageMigration(t *testing.T, fail bool) {
	f := newAdoptionNativeFixture(t, "v0.1.249", true)
	f.manager("stop", "mesh.service")
	packageImage := filepath.Join(f.root, "Caskroom", "mesh", "v0.1.249", "mesh")
	if err := os.MkdirAll(filepath.Dir(packageImage), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.install, packageImage); err != nil {
		t.Fatal(err)
	}
	f.write("daemon.source", []byte(packageImage), false)
	f.write("home/.config/systemd/user/mesh.service", []byte("[Service]\nExecStart="+packageImage+" daemon --tailnet-port=0 --ssh-port=0\nKillMode=process\n"), false)
	f.manager("start", "mesh.service")
	original := f.awaitDaemon()
	side, pin := f.publishCandidate()
	if fail {
		f.write("fail-managed", nil, false)
	}
	f.startOnce(side, pin)
	if fail {
		assertNativeMigrationRestored(t, f, original, packageImage)
		return
	}
	f.awaitAdoption(AdoptionComplete)
	actual := f.awaitDaemon()
	if actual.Executable != filepath.Join(f.root, "home", ".local", "bin", "mesh") || actual.Health.Build.Version != "v0.1.400" {
		t.Fatal("package-owned installation did not migrate to the managed target")
	}
	if err := release.VerifyExecutable(t.Context(), packageImage, original.Health.Build.Digest); err != nil {
		t.Fatal("migration overwrote the package payload")
	}
	f.roundTrip()
}

func assertNativeMigrationRestored(t *testing.T, f *adoptionNativeFixture, original updatebootstrap.Observation, packageImage string) {
	t.Helper()
	f.awaitAdoption(AdoptionAttention)
	actual := f.awaitDaemon()
	if actual.Executable != packageImage || !sameHostIdentity(actual.Health.Build, original.Health.Build) {
		t.Fatal("failed migration did not restore the same old package image")
	}
	if _, err := os.Stat(filepath.Join(f.root, "home", ".local", "bin", "mesh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned managed copy remains after successful restoration")
	}
	if !reflect.DeepEqual(actual.Health.Workers, original.Health.Workers) {
		t.Fatal("failed migration changed retained workers")
	}
	if _, err := updateinstall.Read(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed migration fabricated or edited a journal")
	}
	f.roundTrip()
}

func (f *adoptionNativeFixture) sideCommand(side, pin string) *exec.Cmd {
	_, host := f.localClient()
	input := AdoptionLocalInput{Schema: 1, ID: strings.Repeat("a", 32), Pin: pin, Version: "v0.1.400", Expected: host.ID, Policy: HostPolicy{Mode: HostAutomatic}}
	return &exec.Cmd{Path: side, Args: []string{side, "update", "adopt", "start", "--local", "--release", "v0.1.400", "--json"}, Env: os.Environ(), Stdin: strings.NewReader(string(mustHostJSON(input)))}
}

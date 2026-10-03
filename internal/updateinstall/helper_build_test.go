package updateinstall

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func TestUpgradeHelperRealBuildContract(t *testing.T) {
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("real build contract requires %s", tool)
		}
	}
	checkout, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mesh checkout")
	if err = os.Symlink(checkout, root); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("testdata/helperbuild/main.go")
	if err != nil {
		t.Fatal(err)
	}
	workspace, artifacts := t.TempDir(), t.TempDir()
	module := "module github.com/shaul/mesh/internal/updateinstall/helperfixture\n\ngo 1.27.0\n\nrequire github.com/shaul/mesh v0.0.0\nreplace github.com/shaul/mesh => " + strconv.Quote(root) + "\n"
	for name, data := range map[string][]byte{"go.mod": []byte(module), "main.go": source} {
		if err = os.WriteFile(filepath.Join(workspace, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := func(name string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed local build tools and fixture executables
		cmd.Dir = workspace
		output, commandErr := cmd.CombinedOutput()
		if commandErr != nil {
			t.Fatalf("fixture tool %s failed: %v\n%s", name, commandErr, output)
		}
		return output
	}
	command("go", "mod", "tidy")
	command("git", "init", "-q")
	command("git", "add", "go.mod", "go.sum", "main.go")
	command("git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "helper build fixture")
	binaries := make(map[string][]byte)
	builds := make(map[string]release.Build)
	for _, version := range []string{"v0.1.150", "v0.1.151", "v0.1.159", "v0.1.170", "v0.1.171"} {
		path := filepath.Join(artifacts, version)
		command("go", "build", "-mod=readonly", "-buildvcs=true", "-trimpath", "-ldflags", "-s -w -buildid= -X github.com/shaul/mesh/internal/release.Version="+version, "-o", path, ".")
		var build release.Build
		if err = json.Unmarshal(command(path, "version", "--json"), &build); err != nil {
			t.Fatal(err)
		}
		binaries[version], err = os.ReadFile(path) //nolint:gosec // isolated compiled fixture
		if err != nil {
			t.Fatal(err)
		}
		if build.Modified || build.Commit == "" || build.Version != version || build.Digest != digestBytes(binaries[version]) || build.StateVersion != release.CurrentStateVersion {
			t.Fatalf("real release.Current metadata is invalid: %+v", build)
		}
		builds[version] = build
	}
	for _, kind := range []string{"systemd", "launchd"} {
		t.Run(kind, func(t *testing.T) {
			f := newHelperUpgradeFixture(t, kind, "v0.1.170")
			stage := func(version string) {
				t.Helper()
				if err := atomicWrite(f.cfg.Executable, binaries[version], 0755); err != nil {
					t.Fatal(err)
				}
				f.publish(t, builds[version])
			}
			stage("v0.1.170")
			f.prior, err = prepareHelper(t.Context(), f.cfg, true, "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			for _, version := range []string{"v0.1.151", "v0.1.159", "v0.1.150", "v0.1.170"} {
				stage(version)
				before := f.snapshot(t)
				got, upgradeErr := UpgradeHelper(ctx, f.cfg)
				if upgradeErr != nil || got != f.prior {
					t.Fatalf("real %s commit replaced newer helper: %v", version, upgradeErr)
				}
				f.assertUnchanged(t, before)
			}
			stage("v0.1.171")
			got, upgradeErr := UpgradeHelper(ctx, f.cfg)
			if upgradeErr != nil || got.Digest != builds["v0.1.171"].Digest {
				t.Fatalf("real future compatible helper upgrade failed: %v", upgradeErr)
			}
			commands, readErr := os.ReadFile(f.commands)
			if readErr != nil || !strings.Contains(string(commands), "mesh-update-helper") {
				t.Fatalf("real future helper restart missing: %v", readErr)
			}
		})
	}
}

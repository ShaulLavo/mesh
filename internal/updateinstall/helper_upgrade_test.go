package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type helperUpgradeFixture struct {
	cfg      HelperConfig
	prior    HelperInstallation
	commands string
	probes   string
}

func newHelperUpgradeFixture(t *testing.T, kind, version string) helperUpgradeFixture {
	t.Helper()
	root := t.TempDir()
	f := helperUpgradeFixture{
		cfg: HelperConfig{StateDir: filepath.Join(root, "state"), Executable: filepath.Join(root, "mesh"),
			ServiceDir: filepath.Join(root, "services"), Kind: kind, Domain: "gui/501"},
		commands: filepath.Join(root, "commands"), probes: filepath.Join(root, "probes"),
	}
	t.Setenv("MESH_HELPER_TEST_COMMANDS", f.commands)
	t.Setenv("MESH_HELPER_TEST_PROBES", f.probes)
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"systemctl", "launchctl"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MESH_HELPER_TEST_COMMANDS\"\n"), 0755); err != nil { //nolint:gosec // isolated service manager fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.commit(t, version, "")
	var err error
	f.prior, err = PrepareHelper(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f helperUpgradeFixture) commit(t *testing.T, version, suffix string) {
	t.Helper()
	binary := strings.Replace(string(testExecutable(version)), "health=$2", `if [ "$1" = update-helper ]; then
  printf '%s\n' "$0" >> "$MESH_HELPER_TEST_PROBES"
  exit 0
fi
health=$2`, 1) + suffix
	if err := os.WriteFile(f.cfg.Executable, []byte(binary), 0755); err != nil { //nolint:gosec // isolated executable fixture
		t.Fatal(err)
	}
	f.publish(t, release.Build{Version: version, Commit: strings.Repeat("a", 40),
		Digest: digestBytes([]byte(binary)), StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1})
}

func (f helperUpgradeFixture) publish(t *testing.T, build release.Build) {
	t.Helper()
	manifest := release.Manifest{Schema: 1, Version: build.Version, Commit: build.Commit,
		Compatibility: release.Compatibility{StateReadMin: build.StateVersion, StateReadMax: build.StateVersion, StateWrite: build.StateVersion,
			WorkerMin: build.WorkerProtocol, WorkerMax: build.WorkerProtocol, WorkerWrite: build.WorkerProtocol, JournalVersion: release.CurrentJournalVersion}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform,
			Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz",
			SHA256:  strings.Repeat("b", 64), BinarySHA256: build.Digest})
	}
	status := Status{Schema: 1, Phase: Committed,
		Request:  Request{ID: "helper-commit", TargetID: "fixture-host", Generation: 1, Manifest: manifest},
		Settings: Settings{StateDir: f.cfg.StateDir, Executable: f.cfg.Executable, CacheDir: filepath.Join(f.cfg.StateDir, "cache"), ClientOnly: true}}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(journalPath(f.cfg.StateDir), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f helperUpgradeFixture) snapshot(t *testing.T) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for _, path := range []string{helperRecord(f.cfg.StateDir), f.prior.ServicePath, f.prior.Executable,
		f.cfg.Executable, journalPath(f.cfg.StateDir)} {
		data, err := os.ReadFile(path) //nolint:gosec // isolated installation fixture paths
		if err != nil {
			t.Fatal(err)
		}
		values[path] = string(data)
	}
	launcher := filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current")
	link, err := os.Readlink(launcher)
	if err != nil {
		t.Fatal(err)
	}
	values[launcher] = link
	return values
}

func (f helperUpgradeFixture) assertUnchanged(t *testing.T, before map[string]string) {
	t.Helper()
	for path, value := range f.snapshot(t) {
		if value != before[path] {
			t.Errorf("helper upgrade changed %s", filepath.Base(path))
		}
	}
	if data, err := os.ReadFile(f.commands); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("service manager ran during retained helper decision: %q, %v", data, err)
	}
}

func TestUpgradeHelperPreservesNewerHelperAcrossBridgeCommits(t *testing.T) {
	for _, kind := range []string{"systemd", "launchd"} {
		t.Run(kind, func(t *testing.T) {
			f := newHelperUpgradeFixture(t, kind, "v0.1.170")
			for _, version := range []string{"v0.1.151", "v0.1.159", "v0.1.150"} {
				f.commit(t, version, "")
				before := f.snapshot(t)
				got, err := UpgradeHelper(context.Background(), f.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if got != f.prior {
					t.Fatalf("committed %s replaced verified newer v0.1.170 helper: got digest %s, want %s", version, got.Digest, f.prior.Digest)
				}
				f.assertUnchanged(t, before)
			}
		})
	}
}

func TestUpgradeHelperNormalUpgrade(t *testing.T) {
	for _, kind := range []string{"systemd", "launchd"} {
		for _, versions := range [][2]string{{"v0.1.150", "v0.1.151"}, {"v0.1.151", "v0.1.159"}, {"v0.1.159", "v0.1.170"}, {"v0.1.170", "v0.1.171"}} {
			t.Run(kind+"/"+versions[0]+"_to_"+versions[1], func(t *testing.T) {
				f := newHelperUpgradeFixture(t, kind, versions[0])
				f.commit(t, versions[1], "")
				before := f.snapshot(t)
				got, err := UpgradeHelper(context.Background(), f.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if got.Digest == f.prior.Digest || got.Digest != digestBytes([]byte(before[f.cfg.Executable])) {
					t.Fatal("helper did not advance to committed executable")
				}
				if err = verifyFile(f.prior.Executable, f.prior.Digest); err != nil {
					t.Fatal(err)
				}
				var record HelperInstallation
				if err = readJSON(helperRecord(f.cfg.StateDir), &record); err != nil || record != got {
					t.Fatalf("promoted helper receipt = %+v, %v", record, err)
				}
				link, err := os.Readlink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"))
				if err != nil || link != got.Executable {
					t.Fatalf("promoted helper link = %s, %v", link, err)
				}
				commands, err := os.ReadFile(f.commands)
				if err != nil || !strings.Contains(string(commands), "restart --no-block mesh-update-helper.service") && !strings.Contains(string(commands), "kickstart -k gui/501/dev.shaulavo.mesh-update-helper") {
					t.Fatalf("helper restart missing: %q, %v", commands, err)
				}
				probes, err := os.ReadFile(f.probes)
				if err != nil || !strings.Contains(string(probes), got.Executable) {
					t.Fatalf("replacement journal probe missing: %q, %v", probes, err)
				}
				for _, path := range []string{f.cfg.Executable, journalPath(f.cfg.StateDir)} {
					data, err := os.ReadFile(path) //nolint:gosec // isolated installation fixture paths
					if err != nil || string(data) != before[path] {
						t.Fatalf("helper upgrade mutated daemon or journal: %s, %v", filepath.Base(path), err)
					}
				}
			})
		}
	}
}

func TestUpgradeHelperSameExecutableIsIdempotent(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.159")
	before := f.snapshot(t)
	got, err := UpgradeHelper(context.Background(), f.cfg)
	if err != nil || got != f.prior {
		t.Fatalf("matching helper = %+v, %v", got, err)
	}
	f.assertUnchanged(t, before)
}

func TestUpgradeHelperLockCancellation(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.151", "")
	before := f.snapshot(t)
	lock, err := lockInstallation(context.Background(), f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = UpgradeHelper(ctx, f.cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting helper upgrade = %v", err)
	}
	f.assertUnchanged(t, before)
}

func TestUpgradeHelperRefusesAmbiguousVersions(t *testing.T) {
	for _, test := range []struct{ name, prior, candidate, suffix string }{
		{"same-version-different-digest", "v0.1.159", "v0.1.159", "\n# different artifact\n"},
		{"same-version-build-metadata", "v0.1.159+fixed", "v0.1.159", ""},
		{"unknown-prior", "", "v0.1.159", ""},
		{"unknown-candidate", "v0.1.170", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHelperUpgradeFixture(t, "systemd", test.prior)
			f.commit(t, test.candidate, test.suffix)
			before := f.snapshot(t)
			if _, err := UpgradeHelper(context.Background(), f.cfg); err == nil {
				t.Fatal("ambiguous helper replacement accepted")
			}
			f.assertUnchanged(t, before)
		})
	}
}

func TestUpgradeHelperRejectsInvalidExistingReceipt(t *testing.T) {
	for _, problem := range []string{"corrupt-copy", "wrong-copy-path", "wrong-link", "wrong-build-digest"} {
		t.Run(problem, func(t *testing.T) {
			f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
			f.commit(t, "v0.1.151", "")
			switch problem {
			case "corrupt-copy":
				if err := os.WriteFile(f.prior.Executable, []byte("corrupted"), 0755); err != nil { //nolint:gosec // isolated executable fixture
					t.Fatal(err)
				}
			case "wrong-copy-path":
				f.prior.Executable = f.cfg.Executable
				f.prior.Digest, _ = fileDigest(f.cfg.Executable)
			case "wrong-link":
				if err := replaceHelperLink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"), f.cfg.Executable); err != nil {
					t.Fatal(err)
				}
			case "wrong-build-digest":
				binary := fmt.Sprintf("#!/bin/sh\nprintf '{\"version\":\"v0.1.170\",\"digest\":\"%s\",\"platform\":{\"os\":\"%s\",\"arch\":\"%s\"}}\\n'\n", strings.Repeat("f", 64), release.CurrentPlatform().OS, release.CurrentPlatform().Arch)
				f.prior.Digest = digestBytes([]byte(binary))
				f.prior.Executable = filepath.Join(transactionDir(f.cfg.StateDir), "helper", f.prior.Digest, "mesh")
				if err := atomicWrite(f.prior.Executable, []byte(binary), 0755); err != nil {
					t.Fatal(err)
				}
				if err := replaceHelperLink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"), f.prior.Executable); err != nil {
					t.Fatal(err)
				}
			}
			record, err := json.Marshal(f.prior)
			if err != nil {
				t.Fatal(err)
			}
			if err = atomicWrite(helperRecord(f.cfg.StateDir), record, 0600); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot(t)
			if _, err = UpgradeHelper(context.Background(), f.cfg); err == nil {
				t.Fatal("invalid helper receipt accepted")
			}
			f.assertUnchanged(t, before)
		})
	}
}

func TestUpgradeHelperRejectsIncompatibleBuilds(t *testing.T) {
	for _, change := range []struct{ name, from, to string }{
		{"state", `"stateVersion":7`, `"stateVersion":10`},
		{"worker", `"workerProtocol":1`, `"workerProtocol":2`},
		{"update", `"updateProtocol":1`, `"updateProtocol":2`},
		{"missing-update", `"updateProtocol":1`, `"updateProtocol":0`},
		{"missing-commit", strings.Repeat("a", 40), ""},
		{"modified", `"stateVersion":7`, `"modified":true,"stateVersion":7`},
		{"platform", `"os":"` + release.CurrentPlatform().OS + `"`, `"os":"unknown"`},
		{"invalid-version", "v0.1.170", "development"},
	} {
		for _, side := range []string{"prior", "candidate"} {
			t.Run(change.name+"/"+side, func(t *testing.T) {
				f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
				f.commit(t, "v0.1.171", "")
				path := f.cfg.Executable
				if side == "prior" {
					path = f.prior.Executable
				}
				binary, err := os.ReadFile(path) //nolint:gosec // isolated fixture executable
				if err != nil {
					t.Fatal(err)
				}
				from := change.from
				if change.name == "invalid-version" && side == "candidate" {
					from = "v0.1.171"
				}
				changed := strings.Replace(string(binary), from, change.to, 1)
				if changed == string(binary) {
					t.Fatal("build mutation did not change fixture")
				}
				digest := digestBytes([]byte(changed))
				if side == "candidate" {
					f.publish(t, release.Build{Version: "v0.1.171", Commit: strings.Repeat("a", 40), Digest: digest, StateVersion: 7, WorkerProtocol: 1})
				} else {
					f.replacePrior(t, []byte(changed))
					path = f.prior.Executable
				}
				if err = atomicWrite(path, []byte(changed), 0755); err != nil {
					t.Fatal(err)
				}
				before := f.snapshot(t)
				if _, err = UpgradeHelper(t.Context(), f.cfg); err == nil {
					t.Fatal("invalid or incompatible build accepted")
				}
				f.assertUnchanged(t, before)
			})
		}
	}
}

func TestUpgradeHelperBuildProbeCancellation(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	binary := []byte("#!/bin/sh\nexec sleep 10\n")
	f.replacePrior(t, binary)
	before := f.snapshot(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := UpgradeHelper(ctx, f.cfg); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("build probe cancellation failed: %v", err)
	}
	f.assertUnchanged(t, before)
}

func (f *helperUpgradeFixture) replacePrior(t *testing.T, binary []byte) {
	t.Helper()
	f.prior.Digest = digestBytes(binary)
	f.prior.Executable = filepath.Join(transactionDir(f.cfg.StateDir), "helper", f.prior.Digest, "mesh")
	if err := atomicWrite(f.prior.Executable, binary, 0755); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(f.prior)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(helperRecord(f.cfg.StateDir), record, 0600); err != nil {
		t.Fatal(err)
	}
	if err = replaceHelperLink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"), f.prior.Executable); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeHelperRejectsInvalidBuildReports(t *testing.T) {
	for _, report := range []struct{ name, command string }{
		{"absent", "exit 0"},
		{"invalid-json", "printf 'broken'"},
		{"oversized", "printf '%65537s' x"},
	} {
		t.Run(report.name, func(t *testing.T) {
			f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
			f.commit(t, "v0.1.171", "")
			f.replacePrior(t, []byte("#!/bin/sh\n"+report.command+"\n"))
			before := f.snapshot(t)
			if _, err := UpgradeHelper(t.Context(), f.cfg); err == nil {
				t.Fatal("invalid build report accepted")
			}
			f.assertUnchanged(t, before)
		})
	}
}

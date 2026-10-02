package macapp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBundle(t *testing.T) (Bundle, *int) {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "bin", "mesh")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	signs := 0
	return Bundle{
		Dir: filepath.Join(dir, "state"), Executable: executable, Version: "v0.1.115", Icon: []byte("icns"),
		// Like codesign, rewrite the copy so it no longer matches the installed file.
		Sign: func(bundle string) error {
			signs++
			binary := filepath.Join(bundle, "Contents", "MacOS", "mesh")
			data, err := os.ReadFile(binary) //nolint:gosec // inside this test's bundle
			if err != nil {
				return fmt.Errorf("read staged binary: %w", err)
			}
			return os.WriteFile(binary, append(data, " signed"...), 0o600) //nolint:gosec // inside this test's bundle
		},
	}, &signs
}

func TestSyncBuildsBundleOnceAndRebuildsWhenTheBinaryChanges(t *testing.T) {
	bundle, signs := testBundle(t)
	if err := os.MkdirAll(bundle.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Sync(bundle)
	if err != nil || !rebuilt {
		t.Fatalf("first Sync = %v, %v; want rebuilt", rebuilt, err)
	}
	contents := filepath.Join(bundle.Dir, "Mesh.app", "Contents")
	for name, want := range map[string]string{
		"MacOS/mesh":                     "binary v1 signed",
		"Resources/mesh.icns":            "icns",
		"Resources/installed-executable": bundle.Executable + "\n",
	} {
		data, err := os.ReadFile(filepath.Join(contents, name)) //nolint:gosec // fixed names inside this test's bundle
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
	plist, _ := os.ReadFile(filepath.Join(contents, "Info.plist")) //nolint:gosec // fixed name inside this test's bundle
	for _, want := range []string{"<string>dev.shaulavo.mesh</string>", "<string>0.1.115</string>", "<key>LSUIElement</key><true/>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("Info.plist lacks %s:\n%s", want, plist)
		}
	}
	if rebuilt, err = Sync(bundle); err != nil || rebuilt {
		t.Fatalf("unchanged Sync = %v, %v; want no rebuild", rebuilt, err)
	}
	if err := os.WriteFile(bundle.Executable, []byte("binary v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rebuilt, err = Sync(bundle); err != nil || !rebuilt {
		t.Fatalf("Sync after update = %v, %v; want rebuilt", rebuilt, err)
	}
	if data, _ := os.ReadFile(bundle.BundleExecutable()); string(data) != "binary v2 signed" {
		t.Fatalf("bundle binary = %q after update", data)
	}
	if *signs != 2 {
		t.Fatalf("signed %d times; want once per build", *signs)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(bundle.Dir, "Mesh.app.*")); len(leftovers) != 0 {
		t.Fatalf("staging left behind: %v", leftovers)
	}
}

func TestFailedSigningKeepsThePreviousBundle(t *testing.T) {
	bundle, _ := testBundle(t)
	if err := os.MkdirAll(bundle.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle.Executable, []byte("binary v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle.Sign = func(string) error { return errors.New("no codesign") }
	if _, err := Sync(bundle); err == nil || !strings.Contains(err.Error(), "no codesign") {
		t.Fatalf("Sync with failing signer = %v", err)
	}
	if data, _ := os.ReadFile(bundle.BundleExecutable()); string(data) != "binary v1 signed" {
		t.Fatalf("bundle binary = %q; want the previous build kept", data)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(bundle.Dir, "Mesh.app.*")); len(leftovers) != 0 {
		t.Fatalf("staging left behind: %v", leftovers)
	}
}

func TestInstalledFromResolvesTheBundleMarker(t *testing.T) {
	bundle, _ := testBundle(t)
	if err := os.MkdirAll(bundle.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(bundle); err != nil {
		t.Fatal(err)
	}
	if got := installedFrom(bundle.BundleExecutable()); got != bundle.Executable {
		t.Fatalf("installedFrom(bundle) = %q; want %q", got, bundle.Executable)
	}
	if got := installedFrom(bundle.Executable); got != bundle.Executable {
		t.Fatalf("installedFrom(installed) = %q; want itself", got)
	}
}

func TestInfoPlistVersionIsNumeric(t *testing.T) {
	for version, want := range map[string]string{"v0.1.115": "0.1.115", "1.2": "1.2", "dev": "0", "v0.1.115-3-gabc": "0"} {
		if plist := infoPlist(version); !strings.Contains(plist, "<key>CFBundleShortVersionString</key><string>"+want+"</string>") {
			t.Fatalf("infoPlist(%q) lacks version %s", version, want)
		}
	}
}

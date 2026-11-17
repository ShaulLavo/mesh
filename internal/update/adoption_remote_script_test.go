package update

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type adoptionScriptHost struct {
	home string
	bin  string
	env  []string
}

// newAdoptionScriptHost simulates a remote account: uname reports os, the
// listed tools exist as stubs, and real tools stay reachable only when inherit
// is set.
func newAdoptionScriptHost(t *testing.T, os string, tools []string, inherit bool) adoptionScriptHost {
	t.Helper()
	root := t.TempDir()
	h := adoptionScriptHost{home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin")}
	writeAdoptionScriptTool(t, h.bin, "uname", "case \"$1\" in -s) echo "+os+" ;; -m) echo arm64 ;; esac\n")
	for _, tool := range tools {
		writeAdoptionScriptTool(t, h.bin, tool, "exit 0\n")
	}
	path := h.bin
	if inherit {
		path += string(filepath.ListSeparator) + "/usr/bin:/bin"
	}
	h.env = []string{"HOME=" + h.home, "PATH=" + path}
	return h
}

func writeAdoptionScriptTool(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // isolated tool stub
		t.Fatal(err)
	}
}

func (h adoptionScriptHost) cache(t *testing.T, parts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(append([]string{h.home}, parts...)...), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (h adoptionScriptHost) run(t *testing.T, script string) (string, string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", script) //nolint:gosec // runs the fixed adoption scripts under test in an isolated account
	command.Env = h.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func TestAdoptionProbeUsesTheDaemonCacheRuleAndChecksDeployTools(t *testing.T) {
	all := []string{"curl", "tar", "mktemp", "sha256sum"}
	cases := []struct {
		name, os, refusal string
		tools             []string
		cache             []string
	}{
		{name: "mac cache", os: "Darwin", tools: all, cache: []string{"Library", "Caches", "mesh", "updates"}},
		{name: "mac without its cache", os: "Darwin", tools: all, cache: []string{".cache", "mesh", "updates"}, refusal: "Library/Caches/mesh/updates is unavailable"},
		{name: "linux cache", os: "Linux", tools: all, cache: []string{".cache", "mesh", "updates"}},
		{name: "linux without its cache", os: "Linux", tools: all, cache: []string{"Library", "Caches", "mesh", "updates"}, refusal: ".cache/mesh/updates is unavailable"},
		{name: "shasum only", os: "Darwin", tools: []string{"curl", "tar", "mktemp", "shasum"}, cache: []string{"Library", "Caches", "mesh", "updates"}},
		{name: "no curl", os: "Linux", tools: []string{"tar", "mktemp", "sha256sum"}, cache: []string{".cache", "mesh", "updates"}, refusal: "needs curl"},
		{name: "no digest tool", os: "Linux", tools: []string{"curl", "tar", "mktemp"}, cache: []string{".cache", "mesh", "updates"}, refusal: "sha256sum or shasum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newAdoptionScriptHost(t, c.os, c.tools, false)
			h.cache(t, c.cache...)
			stdout, stderr, err := h.run(t, adoptionProbeScript)
			if c.refusal == "" && (err != nil || stdout != c.os+"\narm64\n") {
				t.Fatalf("probe = %q, %q, %v", stdout, stderr, err)
			}
			if c.refusal != "" && (err == nil || !strings.Contains(stderr, c.refusal)) {
				t.Fatalf("probe accepted a host the deploy would fail on: %q, %v", stderr, err)
			}
		})
	}
}

func TestAdoptionProbeHonorsCacheOverride(t *testing.T) {
	h := newAdoptionScriptHost(t, "Darwin", []string{"curl", "tar", "mktemp", "sha256sum"}, false)
	h.cache(t, "custom")
	h.env = append(h.env, "MESH_UPDATE_CACHE_DIR="+filepath.Join(h.home, "custom"))
	if _, stderr, err := h.run(t, adoptionProbeScript); err != nil {
		t.Fatalf("override cache refused: %q, %v", stderr, err)
	}
}

// The deploy itself must find the macOS cache: before this fix it looked only
// in ~/.cache and failed after SSH, which made the remote count as touched.
func TestAdoptionDeployFindsTheMacCache(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	_, operation := f.state(t)
	command, err := AdoptionRemoteCommand(operation, operation.Remotes[0])
	if err != nil {
		t.Fatal(err)
	}
	h := newAdoptionScriptHost(t, "Darwin", nil, true)
	writeAdoptionScriptTool(t, h.bin, "curl", "printf '%s\\n' 'fixture curl reached' >&2\nexit 7\n")
	h.cache(t, "Library", "Caches", "mesh", "updates")
	_, stderr, err := h.run(t, command)
	if err == nil || !strings.Contains(stderr, "fixture curl reached") || strings.Contains(stderr, "unavailable") {
		t.Fatalf("deploy did not get past the macOS cache check: %q, %v", stderr, err)
	}
	if entries, err := os.ReadDir(filepath.Join(h.home, "Library", "Caches", "mesh", "updates", "adoption")); err != nil || len(entries) != 0 {
		t.Fatalf("failed deploy left side work: %v %v", entries, err)
	}
}

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestVersionBundledBrotliNotice(t *testing.T) {
	stdout, stderr, err := executeCommand(t, Dependencies{}, "version", "--licenses")
	if err != nil || stderr != "" {
		t.Fatalf("notice stdout %q stderr %q error %v", stdout, stderr, err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(stdout)))
	if stdout != release.BrotliNotice() || len(stdout) != 1084 || digest != "3d180008e36922a4e8daec11c34c7af264fed5962d07924aea928c38e8663c94" {
		t.Fatalf("notice bytes %d, digest %s", len(stdout), digest)
	}
}

func TestVersionPlainJSONAndFalseNoticeFlag(t *testing.T) {
	stdout, stderr, err := executeCommand(t, Dependencies{}, "version")
	build := release.Metadata()
	version := build.Version
	if version == "" {
		version = "development"
	}
	want := fmt.Sprintf("mesh %s %s %s\n", version, build.Commit, build.Platform.String())
	if err != nil || stderr != "" || stdout != want {
		t.Fatalf("plain %q, %q, %v", stdout, stderr, err)
	}
	stdout, stderr, err = executeCommand(t, Dependencies{}, "version", "--licenses=false")
	if err != nil || stderr != "" || stdout != want {
		t.Fatalf("false flag %q, %q, %v", stdout, stderr, err)
	}
	stdout, stderr, err = executeCommand(t, Dependencies{}, "version", "--json")
	var actual release.Build
	if err != nil || stderr != "" || json.Unmarshal([]byte(stdout), &actual) != nil || actual != release.Current() {
		t.Fatalf("JSON %q, %q, %v", stdout, stderr, err)
	}
}

func TestVersionNoticeUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"version", "--licenses", "--json"}, {"version", "--json", "--licenses"}, {"version", "--licenses", "operand"}} {
		stdout, _, err := executeCommand(t, Dependencies{}, args...)
		if err == nil || stdout != "" {
			t.Fatalf("usage %v: stdout %q, error %v", args, stdout, err)
		}
	}
}

func TestVersionNoticePropagatesWriterFailure(t *testing.T) {
	command := versionCommand()
	command.SetArgs([]string{"--licenses"})
	command.SetOut(helperDiagnosticWriter(func([]byte) (int, error) { return 0, io.ErrClosedPipe }))
	command.SetErr(io.Discard)
	if err := command.Execute(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write failure: %v", err)
	}
}

func TestVersionNoticeDoesNotReadOrMutateConfiguration(t *testing.T) {
	for _, mode := range []string{"absent", "retained", "malformed"} {
		t.Run(mode, func(t *testing.T) { testNoticeConfiguration(t, mode) })
	}
}

func testNoticeConfiguration(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "absent-state")
	t.Setenv("MESH_CONFIG_DIR", config)
	t.Setenv("MESH_STATE_DIR", state)
	var path string
	var before []byte
	if mode != "absent" {
		before = obsoleteConfigFixture()
		if mode == "malformed" {
			before = []byte("invalid configuration")
		}
		path = writeClientConfigFixture(t, before)
	}
	for _, args := range [][]string{{"version", "--licenses"}, {"version", "--licenses", "--json"}, {"version", "--json", "--licenses"}, {"version", "--licenses", "operand"}} {
		_, _, _ = executeCommand(t, Dependencies{}, args...)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created state: %v", err)
	}
	if mode == "absent" {
		if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created configuration: %v", err)
		}
		return
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("changed configuration: %q, %v", after, err)
	}
}

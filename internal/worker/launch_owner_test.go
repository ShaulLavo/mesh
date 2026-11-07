package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/paths"
)

type faultOwnerFile struct {
	ownerMarkerFile
	phase  string
	events *[]string
}

func (f faultOwnerFile) Write(data []byte) (int, error) {
	*f.events = append(*f.events, "write")
	if f.phase == "short-write" {
		return 0, nil
	}
	if f.phase == "write" {
		return 0, errors.New("owner write")
	}
	n, err := f.ownerMarkerFile.Write(data)
	if err != nil {
		return n, fmt.Errorf("write fixture marker: %w", err)
	}
	return n, nil
}
func (f faultOwnerFile) Sync() error {
	*f.events = append(*f.events, "file-sync")
	if f.phase == "file-sync" {
		return errors.New("owner sync")
	}
	if err := f.ownerMarkerFile.Sync(); err != nil {
		return fmt.Errorf("sync fixture marker: %w", err)
	}
	return nil
}
func (f faultOwnerFile) Close() error {
	*f.events = append(*f.events, "close")
	err := f.ownerMarkerFile.Close()
	if f.phase == "close" {
		return errors.Join(err, errors.New("owner close"))
	}
	if err != nil {
		return fmt.Errorf("close fixture marker: %w", err)
	}
	return nil
}

func TestLaunchWritesOwnerBeforeStart(t *testing.T) {
	t.Run("actual-child", ownerMarkerVisibleToChild)
	for _, phase := range []string{"success", "open", "write", "short-write", "file-sync", "close", "session-sync", "parent-sync", "cmd-start"} {
		t.Run(phase, func(t *testing.T) { launchOwnerFault(t, phase) })
	}
}
func launchOwnerFault(t *testing.T, phase string) {
	root := t.TempDir()
	marker := "app test\n0123456789abcdef0123456789abcdef"
	var events []string
	publisher := func(dir, owner string) error {
		if owner != marker {
			t.Errorf("owner changed")
		}
		ops := &faultOwnerIO{phase: phase, root: root, events: &events}
		return publishOwnerMarker(dir, owner, ownerMarkerIO{open: ops.open, syncDir: ops.syncDir})
	}
	cfg := LaunchConfig{SessionsDir: root, Owner: marker, Label: "app test", Executable: filepath.Join(root, "missing-mesh"), Command: []string{"sh"}}
	cfg.ownerPublisher = publisher
	startedPath := ""
	if phase != "success" && phase != "cmd-start" {
		cfg.Executable = "/bin/sh"
		cfg.Cwd = ownerChildScript(t)
		startedPath = filepath.Join(cfg.Cwd, "started")
		cfg.Env = append(os.Environ(), "MESH_EXPECT_OWNER="+marker, "MESH_OWNER_STARTED="+startedPath)
	}
	_, err := LaunchDetached(cfg)
	if err == nil {
		t.Fatal("missing executable launched")
	}
	var started *StartedError
	if errors.As(err, &started) {
		t.Fatal("pre-spawn failure reported a live child")
	}
	if startedPath != "" {
		assertOwnerPublicationAborted(t, err, startedPath)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 1 || entries[0].Name() != ".barrier" || entries[0].IsDir() {
		t.Fatalf("reservation cleanup: %v %v; want only the strict shared barrier", entries, readErr)
	}
	if phase == "success" || phase == "cmd-start" {
		if !slices.Equal(events, []string{"open", "write", "file-sync", "close", "session-sync", "parent-sync"}) {
			t.Fatalf("durability order %v", events)
		}
	}
	if phase != "success" && phase != "cmd-start" && slices.Contains(events, "parent-sync") && phase != "parent-sync" {
		t.Fatalf("failed publication continued %v", events)
	}
}

func ownerMarkerVisibleToChild(t *testing.T) {
	root := t.TempDir()
	// A bounded shell child checks the marker before publishing exited metadata.
	cfg := LaunchConfig{SessionsDir: root, Owner: "app test\n0123456789abcdef0123456789abcdef", Executable: "/bin/sh", Command: []string{"unused"}}
	cfg.Cwd = ownerChildScript(t)
	cfg.Env = append(os.Environ(), "MESH_EXPECT_OWNER="+cfg.Owner)
	launched, err := LaunchDetached(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(paths.Owner(launched.Dir)); err != nil || string(data) != cfg.Owner {
		t.Fatalf("child marker %q %v", data, err)
	}
}
func ownerChildScript(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "session-worker")
	// A shell fixture receives the launcher's private --dir, then proves that the
	// complete marker preceded exec. It exits without a real session worker.
	script := "#!/bin/sh\nif [ -n \"${MESH_OWNER_STARTED:-}\" ]; then : > \"$MESH_OWNER_STARTED\"; fi\nwhile [ \"$#\" -gt 0 ]; do\n if [ \"$1\" = --dir ]; then dir=$2; break; fi\n shift\ndone\ntest \"$(cat \"$dir/.owner\")\" = \"$MESH_EXPECT_OWNER\" || exit 9\nprintf '%s' '{\"id\":\"'\"$(basename \"$dir\")\"'\",\"state\":\"exited\",\"createdAt\":\"2026-11-01T00:00:00Z\",\"exitCode\":0}' > \"$dir/meta.json\"\nrm \"$dir/.launching\"\n"
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

type faultOwnerIO struct {
	phase  string
	root   string
	events *[]string
}

func (ops *faultOwnerIO) open(root *os.Root) (ownerMarkerFile, error) {
	*ops.events = append(*ops.events, "open")
	if ops.phase == "open" {
		return nil, errors.New("owner open")
	}
	file, err := root.OpenFile(".owner", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open test marker: %w", err)
	}
	return faultOwnerFile{ownerMarkerFile: file, phase: ops.phase, events: ops.events}, nil
}
func (ops *faultOwnerIO) syncDir(path string) error {
	event := "session-sync"
	if path == ops.root {
		event = "parent-sync"
	}
	*ops.events = append(*ops.events, event)
	if ops.phase == event {
		return errors.New("directory sync")
	}
	return syncOwnerDir(path)
}

func assertOwnerPublicationAborted(t *testing.T, err error, startedPath string) {
	t.Helper()
	if !strings.Contains(err.Error(), "publish owner") {
		t.Fatalf("fault did not abort publication: %v", err)
	}
	if _, err := os.Stat(startedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child started after marker publication failed: %v", err)
	}
}

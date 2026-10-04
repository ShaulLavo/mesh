package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

func obsoleteConfigFixture() []byte {
	return []byte(`{"version":1,"hosts":[{"alias":"obsolete","id":"owner","meshIdentity":"pin","endpoint":"ws://127.0.0.1:7777/mesh","addresses":["127.0.0.1"]}],"dashboard":{"theme":"oled"}}`)
}

func writeClientConfigFixture(t *testing.T, contents []byte) string {
	t.Helper()
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClientRetirementKeepsStrictReader(t *testing.T) {
	path := writeClientConfigFixture(t, obsoleteConfigFixture())
	if _, err := LoadHosts(); err == nil || !strings.Contains(err.Error(), `unknown field "alias"`) {
		t.Fatalf("strict read accepted retired state: %v", err)
	}
	before, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := retireClientAliases(context.Background()); err != nil {
		t.Fatal(err)
	}
	valid, err := readClientConfigTestFile(path)
	if err != nil || bytes.Equal(valid, before) {
		t.Fatal("retirement did not publish current state")
	}
	if err := retireClientAliases(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, valid) {
		t.Fatal("settled configuration was rewritten")
	}
}

func TestClientRetirementInvalidInputUnchanged(t *testing.T) {
	for _, contents := range []string{
		`{"version":1,"hosts":[],"unexpected":true}`,
		`{"version":1,"hosts":[{"alias":"old","id":"owner","meshIdentity":"pin","endpoint":"ws://127.0.0.1:7777/mesh","unexpected":true}]}`,
		`{"version":1,"hosts":[{"alias":"old","id":"owner","meshIdentity":"pin","endpoint":"invalid"}]}`,
		`{"version":1,"version":1,"hosts":[]}`,
		`{"version":1,"hosts":[],"dashboard":{"theme":"oled","theme":"oled"}}`,
		`{"version":1,"hosts":[]} {}`,
		`{"version":1,"hosts":`,
		`{"version":2,"hosts":[]}`,
		`{"version":1,"hosts":[],"alias":"old"}`,
	} {
		t.Run(contents, func(t *testing.T) {
			path := writeClientConfigFixture(t, []byte(contents))
			if err := retireClientAliases(context.Background()); err == nil {
				t.Fatal("retirement accepted invalid input")
			}
			after, err := readClientConfigTestFile(path)
			if err != nil || string(after) != contents {
				t.Fatal("invalid configuration was changed")
			}
		})
	}
}

func TestClientRetirementUnsafeFilesUnchanged(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "public", "fifo", "unsafe-directory", "unsafe-lock"} {
		t.Run(kind, func(t *testing.T) {
			contents := obsoleteConfigFixture()
			path := writeClientConfigFixture(t, contents)
			preserved := path
			switch kind {
			case "symlink", "hardlink":
				preserved = filepath.Join(t.TempDir(), "retained.json")
				if err := os.Rename(path, preserved); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if kind == "hardlink" {
					link = os.Link
				}
				if err := link(preserved, path); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // deliberate unsafe fixture must be refused

					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsafe-directory":
				if err := os.Chmod(filepath.Dir(path), 0o777); err != nil { //nolint:gosec // deliberate unsafe fixture must be refused

					t.Fatal(err)
				}
			case "unsafe-lock":
				if err := os.Symlink(path, filepath.Join(filepath.Dir(path), ".hosts.lock")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := retireClientAliases(ctx); err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
			if kind == "fifo" {
				info, err := os.Lstat(path)
				if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
					t.Fatal("unsafe FIFO changed")
				}
				return
			}
			after, err := readClientConfigTestFile(preserved)
			if err != nil || !bytes.Equal(after, contents) {
				t.Fatal("unsafe file contents changed")
			}
		})
	}
}

func TestClientRetirementLockCancellation(t *testing.T) {
	contents := obsoleteConfigFixture()
	path := writeClientConfigFixture(t, contents)
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".hosts.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := retireClientAliases(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock contention did not fail closed: %v", err)
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatal("lock refusal changed configuration")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := retireClientAliases(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientRetirementDirectorySettlementFailure(t *testing.T) {
	path := writeClientConfigFixture(t, obsoleteConfigFixture())
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	writeClientActivationJournal(t, state, updateinstall.Status{Schema: 1, Phase: updateinstall.Committed, Settings: updateinstall.Settings{StateDir: state}})
	failure := errors.New("injected directory durability failure")
	settlements := 0
	settle := func(*os.File) error { settlements++; return failure }
	for attempt := range 2 {
		if err := retireClientAliasesWithSettlement(context.Background(), settle); !errors.Is(err, failure) {
			t.Fatalf("attempt %d accepted unconfirmed durability: %v", attempt, err)
		}
		after, err := readClientConfigTestFile(path)
		if err != nil || bytes.Contains(after, []byte(`"alias"`)) {
			t.Fatal("published deletion was not visible at the failed settlement boundary")
		}
		status, err := updateinstall.Read(state)
		if err != nil || status.Phase != updateinstall.Committed {
			t.Fatalf("postcommit activation failure changed the update result: %v", err)
		}
	}
	if settlements != 2 {
		t.Fatal("retry bypassed directory durability")
	}
	if err := retireClientAliases(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientConfigWriteFailurePreservesFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure needs an unprivileged user")
	}
	contents := obsoleteConfigFixture()
	path := writeClientConfigFixture(t, contents)
	err := withClientConfigLock(context.Background(), false, func(dir *os.File, _ string) error {
		if err := dir.Chmod(0o500); err != nil {
			return fmt.Errorf("make owned fixture unwritable: %w", err)
		}
		defer func() { _ = dir.Chmod(0o700) }()
		return publishClientConfig(dir, []byte(`{"version":1,"hosts":[]}`), syncClientConfig)
	})
	if err == nil {
		t.Fatal("write failure was accepted")
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatal("failed write replaced retained state")
	}
}

func TestClientConfigWritersSerializeReadModifyWrite(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	var workers sync.WaitGroup
	for _, id := range []string{"first", "second", "third", "fourth"} {
		workers.Go(func() {
			if err := SaveHost(HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:7777/mesh"}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 4 {
		t.Fatalf("concurrent writers lost a record: %d, %v", len(hosts), err)
	}
}

func readClientConfigTestFile(path string) ([]byte, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // callers supply files in their owned t.TempDir fixtures
	if err != nil {
		return nil, fmt.Errorf("read owned config fixture: %w", err)
	}
	return contents, nil
}

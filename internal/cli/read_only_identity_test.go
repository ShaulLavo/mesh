package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/worker"
)

func TestReadOnlyCommandsDoNotWriteFilesWithoutIdentity(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "fresh host"
		if saved {
			name = "saved session without key"
		}
		t.Run(name, func(t *testing.T) {
			for _, operation := range []string{"ls", "resolve", "picker cancel", "logs previous", "recovery read", "saved target"} {
				t.Run(operation, func(t *testing.T) {
					stateDir, configDir := compactSocketTempDir(t), t.TempDir()
					t.Setenv("MESH_STATE_DIR", stateDir)
					t.Setenv("MESH_CONFIG_DIR", configDir)
					dir := filepath.Join(stateDir, "s", "7K3D")
					if saved {
						writeLocalSessionDir(t, "7K3D", worker.StateInterrupted)
						record := recovery.Record{Version: recovery.Version, HostID: "original-host", SessionID: "7K3D",
							CheckpointAt: commandTestTime, Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryLaunch,
							Command: []string{"/bin/sh"}, Lines: []string{"preserved output"}}
						if err := recovery.Write(dir, record); err != nil {
							t.Fatal(err)
						}
					}
					before := readOnlyFileInventory(t, stateDir, configDir)
					err := runReadOnlyIdentityOperation(t, operation, dir)
					if operation == "logs previous" || operation == "recovery read" {
						if err == nil {
							t.Error("unowned saved output was readable")
						} else if saved && !strings.Contains(err.Error(), "local host identity is missing") {
							t.Errorf("saved output error = %v, want missing identity", err)
						}
					}
					if after := readOnlyFileInventory(t, stateDir, configDir); !maps.Equal(before, after) {
						for path := range after {
							if after[path] != before[path] {
								t.Errorf("read-only operation created or changed %s", path)
							}
						}
						for path := range before {
							if _, ok := after[path]; !ok {
								t.Errorf("read-only operation removed %s", path)
							}
						}
					}
				})
			}
		})
	}
}

func runReadOnlyIdentityOperation(t *testing.T, operation, dir string) error {
	t.Helper()
	app := &application{}
	switch operation {
	case "ls":
		_, _, err := executeCommand(t, Dependencies{}, "ls")
		if err != nil {
			t.Fatal(err)
		}
		return err
	case "resolve":
		_, err := app.resolveSession(t.Context(), nil, "91AZ")
		if !errors.Is(err, ErrNoLocalSession) {
			t.Fatalf("missing session resolution = %v", err)
		}
		return err
	case "picker cancel":
		called := false
		_, _, err := executeCommand(t, Dependencies{Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
			called = true
			if len(input.Hosts) != 1 || !input.Hosts[0].Local || input.Hosts[0].Host.ID != "" {
				t.Errorf("first-run catalog = %+v, want local host without identity", input.Hosts)
			}
			if _, err := input.Refresh(ctx, localHostID()); err != nil {
				t.Error(err)
			}
			if hosts, err := input.LoadHosts(ctx); err != nil || len(hosts) != 0 {
				t.Errorf("empty remote catalog = %+v, %v", hosts, err)
			}
			return PickerSelection{}, nil
		}})
		if err != nil || !called {
			t.Fatalf("picker cancel = %v, called = %t", err, called)
		}
		return err
	case "logs previous":
		_, _, err := executeCommand(t, Dependencies{}, "logs", "7K3D", "--previous")
		return err
	case "recovery read":
		_, err := app.readRecoveryRecord(t.Context(), resolvedSession{local: &Session{Meta: worker.Meta{ID: "7K3D"}, Dir: dir}})
		return err
	case "saved target":
		_, err := app.resolveSavedTarget(t.Context(), recovery.Target{HostID: "unadopted-host", SessionID: "7K3D"})
		if err == nil {
			t.Fatal("resolved an unadopted saved target")
		}
		return err
	default:
		t.Fatalf("unknown operation %q", operation)
		return nil
	}
}

func readOnlyFileInventory(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	for _, root := range roots {
		directory, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		err = fs.WalkDir(directory.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			contents, err := fs.ReadFile(directory.FS(), path)
			if err == nil {
				files[filepath.Join(root, path)] = string(contents)
			}
			if err != nil {
				return fmt.Errorf("read inventory file %s: %w", path, err)
			}
			return nil
		})
		closeErr := directory.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	return files
}

func TestSavedTargetWithoutIdentityStillResolvesRemoteHost(t *testing.T) {
	fixture := setupCommandTestHost(t)
	app := &application{dependencies: Dependencies{DialHost: fixture.dial, DialControl: fixture.dial}}
	resolved, err := app.resolveSavedTarget(t.Context(), recovery.Target{HostID: fixture.host.ID, SessionID: "7K3D"})
	if err != nil || resolved.host == nil || resolved.host.ID != fixture.host.ID {
		t.Fatalf("saved remote target = %+v, %v", resolved, err)
	}
	if _, err := identity.Load(os.Getenv("MESH_STATE_DIR")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote target resolution created an identity: %v", err)
	}
}

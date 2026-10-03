package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

func TestHelperRecoveryCLIRejectsUnsafeJournalBeforeWork(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "directory", "oversize", "permissions", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "update")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(directory, "installation.json")
			status := updateinstall.Status{Schema: 1, Settings: updateinstall.Settings{StateDir: root}}
			data, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			metadataRoot, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = metadataRoot.Close() }()
			if err = metadataRoot.WriteFile("installation.json", data, 0600); err != nil {
				t.Fatal(err)
			}
			if settings, readErr := updateinstall.ReadSettingsContext(t.Context(), root); readErr != nil || settings.StateDir != root {
				t.Fatalf("known-good journal settings = %v, %v", settings, readErr)
			}
			if err = metadataRoot.Remove("installation.json"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "fifo":
				err = unix.Mkfifo(journal, 0600)
			case "symlink":
				err = metadataRoot.WriteFile("redirect.json", data, 0600)
				if err == nil {
					err = metadataRoot.Symlink("redirect.json", "installation.json")
				}
			case "directory":
				err = metadataRoot.Mkdir("installation.json", 0700)
			case "oversize":
				err = metadataRoot.WriteFile("installation.json", []byte(strings.Repeat("x", (2<<20)+1)), 0600)
			case "permissions":
				err = metadataRoot.WriteFile("installation.json", data, 0600)
				if err == nil {
					err = metadataRoot.Chmod("installation.json", 0644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			request := updateinstall.HelperRecovery{Helper: updateinstall.HelperConfig{StateDir: root,
				Executable: filepath.Join(root, "replacement"), ServiceDir: filepath.Join(root, "services")},
				Expected: updateinstall.JournalExpectation{Operation: "reviewed", Generation: 1,
					Phase: updateinstall.RolledBack, OriginalDigest: strings.Repeat("a", 64)}}
			_, err = runLocalHelperRecovery(ctx, request, "v0.1.162")
			if err == nil || !strings.Contains(err.Error(), "read helper recovery settings") {
				t.Fatalf("unsafe journal reached recovery work: %v", err)
			}
			if kind == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled CLI metadata read = %v", err)
				}
				return
			}
			if err = ctx.Err(); err != nil {
				t.Fatalf("CLI metadata rejection waited for deadline: %v", err)
			}
			if _, err = metadataRoot.Stat("installation.lock"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe CLI journal reached installation lock: %v", err)
			}
		})
	}
}

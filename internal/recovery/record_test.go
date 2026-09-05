package recovery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func checkpointFixture() Record {
	return Record{Version: Version, HostID: "host-one", SessionID: "7K3D", CheckpointAt: time.Now().UTC(),
		Shell: "/bin/bash", ShellDirectory: "/project", DirectorySource: DirectoryShell,
		Command: []string{"/bin/bash"}, Lines: []string{"$ go test ./...", "ok mesh"}}
}

func TestCheckpointReadersOnlySeeCompleteAtomicRecords(t *testing.T) {
	dir := t.TempDir()
	record := checkpointFixture()
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	readers.Go(func() {
		for range 80 {
			if _, err := Read(dir); err != nil {
				t.Errorf("concurrent checkpoint read: %v", err)
			}
		}
	})
	for index := range 30 {
		record.Title = strings.Repeat("x", index)
		if err := Write(dir, record); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	file, err := os.Stat(filepath.Join(dir, "recovery.json"))
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint permissions = %v, err = %v", file, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary checkpoint remains: %v", err)
	}
}

func TestReadRejectsMalformedFutureForeignAndOversizedRecords(t *testing.T) {
	for _, name := range []string{"malformed", "future", "oversized", "controls", "arguments"} {
		t.Run(name, func(t *testing.T) {
			record := checkpointFixture()
			contents := invalidCheckpoint(t, name, &record)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "recovery.json"), contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir); err == nil {
				t.Fatal("invalid checkpoint was accepted")
			}
		})
	}
	if err := ValidateOwner(checkpointFixture(), "another-host", "7K3D"); err == nil {
		t.Fatal("foreign ownership accepted")
	}
}

func invalidCheckpoint(t *testing.T, name string, record *Record) []byte {
	t.Helper()
	switch name {
	case "malformed":
		return []byte(`{"version":`)
	case "oversized":
		return []byte(strings.Repeat(" ", MaxRecordBytes+1))
	case "future":
		record.Version++
	case "controls":
		record.Lines = []string{"\x1b[31munsafe"}
	case "arguments":
		record.Restart = &Command{Argv: []string{"sh", strings.Repeat("x", MaxCommandBytes)}, Cwd: "/"}
	}
	contents, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestDiskFullKeepsPreviousCheckpointAndRemovesTemporary(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	dir := t.TempDir()
	record := checkpointFixture()
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", filepath.Join(dir, "recovery.json.tmp")); err != nil {
		t.Fatal(err)
	}
	updated := record
	updated.Title = "not saved"
	if err := Write(dir, updated); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write error = %v, want ENOSPC", err)
	}
	saved, err := Read(dir)
	if err != nil || saved.Title != record.Title {
		t.Fatalf("previous checkpoint changed: %+v, %v", saved, err)
	}
}

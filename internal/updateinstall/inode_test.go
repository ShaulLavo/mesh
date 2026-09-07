package updateinstall

import (
	"context"
	"os"
	"testing"
)

func TestOriginalInodeSurvivesRollbackRetryAndCommit(t *testing.T) {
	f := newFixture(t)
	original, err := os.Stat(f.engine.cfg.Executable)
	if err != nil {
		t.Fatal(err)
	}
	status := f.stage()
	assertOriginalInode(t, original, status.Previous)
	f.manager.failCandidate = true
	if _, err = f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err = f.engine.Run(context.Background())
	if err == nil || status.Phase != RolledBack {
		t.Fatalf("expected rollback: %s %v", status.Phase, err)
	}
	assertOriginalInode(t, original, f.engine.cfg.Executable)
	assertOriginalInode(t, original, status.Previous)
	f.roundTrip()
	status, err = f.engine.Retry(context.Background(), f.request.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertOriginalInode(t, original, status.Previous)
	f.manager.failCandidate = false
	if _, err = f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err = f.engine.Run(context.Background())
	if err != nil || status.Phase != Committed {
		t.Fatalf("expected commitment: %s %v", status.Phase, err)
	}
	assertOriginalInode(t, original, status.Previous)
	installed, err := os.Stat(f.engine.cfg.Executable)
	if err != nil || os.SameFile(original, installed) {
		t.Fatalf("candidate did not replace install inode: %v", err)
	}
	f.roundTrip()
}

func assertOriginalInode(t *testing.T, original os.FileInfo, path string) {
	t.Helper()
	retained, err := os.Stat(path)
	if err != nil || !os.SameFile(original, retained) {
		t.Fatalf("mapped executable inode was lost at %s: %v", path, err)
	}
}

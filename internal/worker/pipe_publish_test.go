package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The start record and the metadata are written together, but nothing may
// become visible to the daemon until the start record is durable.
func TestFailedStartRecordNeverOpensTheSocket(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{cfg: Config{Dir: dir}}
	recorded := make(chan error, 1)
	recorded <- errors.New("disk full")
	sock := filepath.Join(dir, "sock")
	ln, err := w.publishPipe(nil, sock, Meta{ID: "ABCD"}, recorded)
	if ln != nil {
		_ = ln.Close()
		t.Fatal("a listener was opened without a durable start record")
	}
	if err == nil || !strings.Contains(err.Error(), "record child start") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error %v does not name the failed start record", err)
	}
	if _, statErr := os.Lstat(sock); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("socket exists after a failed start record: %v", statErr)
	}
}

func TestFailedMetadataNeverOpensTheSocketEvenWithADurableStartRecord(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{cfg: Config{Dir: filepath.Join(dir, "missing")}}
	recorded := make(chan error, 1)
	recorded <- nil
	sock := filepath.Join(dir, "sock")
	ln, err := w.publishPipe(nil, sock, Meta{ID: "ABCD"}, recorded)
	if ln != nil {
		_ = ln.Close()
		t.Fatal("a listener was opened without metadata")
	}
	if err == nil {
		t.Fatal("a metadata failure was not reported")
	}
	if len(recorded) != 0 {
		t.Fatal("the start record result was left unread")
	}
}

//go:build mesh_installed_fixture && (linux || darwin)

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestPreparedReplacedFIFODoesNotBlock(t *testing.T) {
	root, file := preparedMutationFile(t, []byte("immutable"))
	if err := root.Remove("asset"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(root.Name()+"/asset", 0600); err != nil {
		t.Fatal(err)
	}
	release := func() {
		writer, err := root.OpenFile("asset", os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, _ = writer.Write([]byte("immutable"))
			_ = writer.Close()
		}
	}
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		file.serve(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/asset", nil))
	}()
	select {
	case <-done:
		if response.Code != http.StatusNotFound {
			t.Fatalf("FIFO response returned %d", response.Code)
		}
		return
	case <-time.After(100 * time.Millisecond):
		cancel()
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owned FIFO handler did not exit after teardown")
	}
	t.Fatal("registered FIFO blocked the handler beyond cancellation")
}

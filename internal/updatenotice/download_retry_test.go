package updatenotice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestNoticeRetriesHTTP2BodyResetInsideCache(t *testing.T) {
	contents, _ := json.Marshal(testManifest("v0.2.0"))
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write(contents[:len(contents)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(contents)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	store := New(Config{Directory: t.TempDir(), Current: release.Build{Version: "v0.1.0"}, Client: release.Client{BaseURL: server.URL, HTTPClient: server.Client()}})
	notice, err := store.Refresh(context.Background(), true)
	if err != nil || notice.Version != "v0.2.0" || calls.Load() != 2 {
		t.Fatalf("cached HTTP/2 body reset: notice %q, error %v, requests %d", notice.Version, err, calls.Load())
	}
}

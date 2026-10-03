package release

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
)

type goAwayConnectionKey struct{}

func TestManifestRecoversAcceptedHTTP2GoAwayBeforeHeaders(t *testing.T) {
	contents, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var disconnect atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		if disconnect.Swap(false) {
			conn := r.Context().Value(goAwayConnectionKey{}).(net.Conn)
			framer := http2.NewFramer(conn, conn)
			if err := framer.WriteGoAway(0x7fffffff, http2.ErrCodeNo, nil); err != nil {
				t.Errorf("write accepted-stream GOAWAY: %v", err)
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write(contents)
	}))
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, goAwayConnectionKey{}, conn)
	}
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	if manifest, err := client.Manifest(context.Background(), "v0.2.0"); err != nil || manifest.Version != "v0.2.0" || calls.Load() != 1 {
		t.Fatalf("known-good HTTP/2 manifest: version %q, error %v, requests %d", manifest.Version, err, calls.Load())
	}
	disconnect.Store(true)
	manifest, err := client.Manifest(context.Background(), "v0.2.0")
	if err != nil || manifest.Version != "v0.2.0" || calls.Load() != 3 {
		t.Fatalf("accepted-stream GOAWAY not recovered: version %q, error %v, requests %d; want one fresh-connection retry", manifest.Version, err, calls.Load())
	}
}

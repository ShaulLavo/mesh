package daemon

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppRegistryBodyDoesNotChargeServerPauses(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay time.Duration
		pause time.Duration
		size  int
	}{
		{name: "delayed-first-read", delay: 600 * time.Millisecond, size: 256 << 10},
		{name: "ten-KiB-per-second-reader", pause: 100 * time.Millisecond, size: 32 << 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(guardAppRegistryBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(test.delay)
				buffer := make([]byte, 1024)
				for {
					_, err := r.Body.Read(buffer)
					if errors.Is(err, io.EOF) {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					if err != nil {
						w.WriteHeader(http.StatusRequestTimeout)
						return
					}
					time.Sleep(test.pause)
				}
			}), 200*time.Millisecond))
			server.Config.ConnContext = appRegistryConnectionContext
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Post(server.URL, "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte("x"), test.size)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("prompt upload with server pause status %d, want 204", response.StatusCode)
			}
		})
	}
}

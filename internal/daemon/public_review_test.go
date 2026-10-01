package daemon

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPublicLoopbackFrontDoorHasNoSourceQuota(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedPublicListener(base, maximumPublicConnections)
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, ConnState: listener.connState,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "stream\n")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.closeActive() })
	for i := range 40 {
		connection, err := net.Dial("tcp4", base.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		if _, err := io.WriteString(connection, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatalf("front-door stream %d rejected: %v", i+1, err)
		}
		t.Cleanup(func() { _ = connection.Close(); _ = response.Body.Close() })
		if response.StatusCode != http.StatusOK {
			t.Fatalf("front-door stream %d status %d", i+1, response.StatusCode)
		}
	}
}

func TestPublicBodyDoesNotChargeServerPauses(t *testing.T) {
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
			server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			server.Config.ConnContext = publicConnectionContext
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

package update

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A finished tunnel must end its Serve goroutine; the fixture proxy used to
// keep one blocked in Accept for every tunnel until the whole proxy closed.
func TestAdoptionSingleConnectionServeEndsWithItsTunnel(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	listener := &adoptionSingleConnection{connection: serverSide, closed: make(chan struct{})}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         listener.connectionState,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	if _, err := io.WriteString(clientSide, "GET /asset HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(clientSide), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("tunnel response %q, %v", body, err)
	}
	_ = clientSide.Close()

	select {
	case err := <-served:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve ended with %v, want the closed single-connection listener", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve still blocked after its only tunnel closed")
	}
}

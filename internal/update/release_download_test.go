package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
)

func TestUpdatePlanRetriesFitRPCBudget(t *testing.T) {
	stateDir := t.TempDir()
	id, key := testIdentity(t)
	store, err := OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(stateDir, "rpc.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host := Host{ID: id, Alias: "local", Endpoint: "unix://" + listener.Addr().String(), Platform: release.CurrentPlatform()}
	manifest := testManifest()
	contents, _ := json.Marshal(manifest)
	var calls atomic.Int32
	var maximumBudget atomic.Int64
	client := release.Client{HTTPClient: &http.Client{Transport: releaseDownloadTransport(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			return nil, fmt.Errorf("metadata attempt has no deadline")
		}
		// Charge each attempt its full allowance without sleeping through resolver stalls.
		maximumBudget.Add(int64(time.Until(deadline)))
		if calls.Add(1) < 3 {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(contents)), Request: request}, nil
	})}}
	coordinator := Coordinator{ID: id, Store: store, Release: client}
	authority := Authority{StateDir: stateDir, ID: id, Key: key, Handle: func(ctx context.Context, action string, data json.RawMessage) (any, error) {
		if action != "plan" {
			return nil, fmt.Errorf("unexpected action %s", action)
		}
		var plan Plan
		if err := json.Unmarshal(data, &plan); err != nil {
			return nil, fmt.Errorf("decode plan: %w", err)
		}
		return coordinator.Start(ctx, plan)
	}}
	done := make(chan error, 1)
	go func() { done <- serveDownloadPlanRPC(listener, &authority) }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var run Run
	err = (Client{ID: id, Key: key}).Call(ctx, host, "plan", Plan{Fleet: Fleet{Version: 1, Name: "test", Revision: 1, Members: []Host{host}}, Manifest: manifest}, &run)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	budget := time.Duration(maximumBudget.Load()) + 750*time.Millisecond
	if calls.Load() != 3 || run.ID == "" || budget >= 45*time.Second {
		t.Fatalf("plan RPC: calls %d, run %q, worst-case retry budget %s; want acknowledged run within 45s", calls.Load(), run.ID, budget)
	}
}

func serveDownloadPlanRPC(listener net.Listener, authority *Authority) error {
	stream, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept update RPC: %w", err)
	}
	defer stream.Close() //nolint:errcheck // request result is authoritative
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return fmt.Errorf("open RPC transport: %w", err)
	}
	for range 2 {
		if err := exchangeDownloadPlan(conn, authority); err != nil {
			return err
		}
	}
	return nil
}

func exchangeDownloadPlan(conn transport.Conn, authority *Authority) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("read RPC: %w", err)
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode RPC: %w", err)
	}
	response, _, err := authority.HandleControl(context.Background(), request)
	if err != nil {
		return fmt.Errorf("handle RPC: %w", err)
	}
	payload, err := response.Encode()
	if err != nil {
		return fmt.Errorf("encode RPC: %w", err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("write RPC: %w", err)
	}
	return nil
}

type releaseDownloadTransport func(*http.Request) (*http.Response, error)

func (t releaseDownloadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t(request)
}

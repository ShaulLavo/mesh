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
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/transport"
)

func TestUpdatePlanRetriesAcknowledgeRPC(t *testing.T) {
	stateDir := testenv.SocketTempDir(t)
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
	host := Host{ID: id, MachineName: "local", Endpoint: "unix://" + listener.Addr().String(), Platform: release.CurrentPlatform()}
	manifest := testManifest()
	contents, _ := json.Marshal(manifest)
	var calls atomic.Int32
	var thirdAllowance atomic.Int64
	client := release.Client{HTTPClient: &http.Client{Transport: releaseDownloadTransport(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			return nil, fmt.Errorf("metadata attempt has no deadline")
		}
		allowance := time.Until(deadline)
		switch calls.Add(1) {
		case 1:
			if allowance > 6*time.Second {
				return nil, fmt.Errorf("initial metadata allowance %s exceeds the early-retry budget", allowance)
			}
			return nil, context.DeadlineExceeded
		case 2:
			if allowance < 20*time.Second {
				return nil, fmt.Errorf("retry allowance %s cannot accommodate a slow DNS lookup", allowance)
			}
			return nil, context.DeadlineExceeded
		default:
			thirdAllowance.Store(int64(allowance))
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
	serverCtx, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	done := make(chan error, 1)
	go func() { done <- serveDownloadPlanRPC(serverCtx, listener, &authority) }()
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
	allowance := time.Duration(thirdAllowance.Load())
	if calls.Load() != 3 || run.ID == "" || allowance > 30*time.Second || ctx.Err() != nil {
		t.Fatalf("plan RPC: calls %d, run %q, final allowance %s; want acknowledged run within the enclosing deadline", calls.Load(), run.ID, allowance)
	}
}

func serveDownloadPlanRPC(ctx context.Context, listener net.Listener, authority *Authority) error {
	stream, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept update RPC: %w", err)
	}
	defer stream.Close() //nolint:errcheck // request result is authoritative
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return fmt.Errorf("open RPC transport: %w", err)
	}
	for range 2 {
		if err := exchangeDownloadPlan(ctx, conn, authority); err != nil {
			return err
		}
	}
	return nil
}

func exchangeDownloadPlan(ctx context.Context, conn transport.Conn, authority *Authority) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("read RPC: %w", err)
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode RPC: %w", err)
	}
	response, _, err := authority.HandleControl(ctx, request)
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

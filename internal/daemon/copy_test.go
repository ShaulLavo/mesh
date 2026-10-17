package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transfer"
	"github.com/shaul/mesh/internal/transport"
)

func TestClientServerCopyDoesNotCreateSessionAndCleansDisconnect(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "output")
	client := newServerTestConn()
	catalog := &lifecycleTestCatalog{}
	lifecycle := mustServerTestLifecycle(t, catalog, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), client) }()
	client.pushRead(copyServerFrame(t, "begin-copy", transfer.Request{Action: "upload.begin", Path: dest, Info: transfer.Info{Name: "source"}}))
	begin := decodeServerControl(t, client.nextWrite(t))
	if begin.Type != protocol.TypeCopyResult || begin.RequestID != "begin-copy" || begin.SessionID != "" {
		t.Fatalf("copy begin response = %+v", begin)
	}
	client.pushRead(copyServerFrame(t, "chunk-copy", transfer.Request{Action: "upload.chunk", Data: []byte("incomplete archive")}))
	chunk := decodeServerControl(t, client.nextWrite(t))
	if chunk.Type != protocol.TypeCopyResult || chunk.RequestID != "chunk-copy" {
		t.Fatalf("copy chunk response = %+v", chunk)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "copy client disconnect"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("copy left destination or staged data: %v (%v)", entries, err)
	}
	if len(catalog.sessions) != 0 {
		t.Fatalf("copy created persistent sessions: %+v", catalog.sessions)
	}
}

func TestClientServerMalformedCopyIsCorrelatedAndKeepsServing(t *testing.T) {
	client := newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeCopyRequest, RequestID: "malformed", Copy: json.RawMessage(`[]`)}))
	assertServerError(t, client.nextWrite(t), "malformed", "", "decode copy request")
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "after-copy"}))
	response := decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeHostInfoResult || response.RequestID != "after-copy" {
		t.Fatalf("after invalid copy response = %+v", response)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "copy error recovery"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateOnlyConnectionCannotSendCopy(t *testing.T) {
	_, auth, options := updaterPolicyAuthentication(t)
	server := updaterPolicyServer(t, auth)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, server.URL, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	frame := copyServerFrame(t, "updater-copy", transfer.Request{Action: "upload.begin", Path: filepath.Join(t.TempDir(), "forbidden"), Info: transfer.Info{Name: "source"}})
	if err := conn.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadFrame(); err == nil {
		t.Fatal("updater-only connection admitted a copy request")
	}
	if ctx.Err() != nil {
		t.Fatal("copy denial did not settle before the connection deadline")
	}
}

func copyServerFrame(t *testing.T, id string, request transfer.Request) protocol.Frame {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return serverControlFrame(t, protocol.Control{Type: protocol.TypeCopyRequest, RequestID: id, Copy: payload})
}

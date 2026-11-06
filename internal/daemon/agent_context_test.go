package daemon

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type contextWorkerCounter struct{ calls atomic.Int32 }

func (c *contextWorkerCounter) ConnectWorker(context.Context, protocol.SessionID) (transport.Conn, error) {
	c.calls.Add(1)
	return nil, io.ErrUnexpectedEOF
}

type contextControlCounter struct{ calls atomic.Int32 }

func (c *contextControlCounter) HandleControl(context.Context, protocol.Control) (protocol.Control, bool, error) {
	c.calls.Add(1)
	return protocol.Control{}, false, nil
}

func TestAgentContextDispatchNeverTouchesWorkersOrServices(t *testing.T) {
	config, state := t.TempDir(), t.TempDir()
	if _, _, err := hostprofile.OpenHostStore(config, "host-a").Set(t.Context(), 0, hostprofile.Editable{SchemaVersion: 1, Instructions: "owner notes"}); err != nil {
		t.Fatal(err)
	}
	workers := &contextWorkerCounter{}
	services := &contextControlCounter{}
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, workers)
	server, err := newClientServer(lifecycle, workers, services, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.context = &agentContextController{configDir: config, stateDir: state, hostID: "host-a", trusted: func(string) bool { return true }}
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), client) }()
	for _, full := range []bool{false, true} {
		checkContextDispatch(t, client, full)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "context server"); err != nil {
		t.Fatal(err)
	}
	if workers.calls.Load() != 0 || services.calls.Load() != 0 {
		t.Fatalf("side effects: workers=%d services=%d", workers.calls.Load(), services.calls.Load())
	}
	if _, err := hostprofile.OpenFleetStore(filepath.Join(state, "missing")).ReadReplica(nil); err != nil {
		t.Fatal(err)
	}
}

func checkContextDispatch(t *testing.T, client *serverTestConn, full bool) {
	t.Helper()
	data, err := json.Marshal(hostprofile.ContextRequest{Full: full})
	if err != nil {
		t.Fatal(err)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostContext, RequestID: "context", Context: data}))
	response := decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeHostContextResult || response.RequestID != "context" {
		t.Fatal(response)
	}
	reply, err := hostprofile.ParseContext(response.Context, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	if full {
		want = "owner notes"
	}
	if reply.Host.Profile == nil || reply.Host.Profile.Instructions != want || reply.Fleet.State != hostprofile.StateAbsent {
		t.Fatalf("context: %+v", reply)
	}
}

func TestAgentContextRejectsMalformedAndCancelledRequest(t *testing.T) {
	root := t.TempDir()
	controller := &agentContextController{configDir: filepath.Join(root, "config"), stateDir: filepath.Join(root, "state"), hostID: "host"}
	for _, data := range []string{`{}`, `{"full":false,"Full":true}`, `{"full":null}`, `{"full":true,"unknown":1}`, `{"full":"yes"}`} {
		_, handled, err := controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeHostContext, Context: json.RawMessage(data)})
		if !handled || (data != `{}` && err == nil) {
			t.Fatalf("%s: handled=%v err=%v", data, handled, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := controller.HandleControl(ctx, protocol.Control{Type: protocol.TypeHostContext, Context: json.RawMessage(`{"full":true}`)}); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

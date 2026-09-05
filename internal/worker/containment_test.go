package worker

import (
	"net"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func TestAttachStoresContainmentAndQueryPrependsWorker(t *testing.T) {
	sid, err := protocol.NewSessionID("B222")
	if err != nil {
		t.Fatal(err)
	}
	pty := newPipePTY()
	defer pty.Close() //nolint:errcheck // test resource cleanup
	w := &Worker{
		cfg:      Config{ID: sid.String(), HostID: "host-b"},
		sid:      sid,
		pty:      pty,
		ring:     session.NewRing(ringSize),
		screen:   terminalstate.NewScreen(80, 24),
		exited:   make(chan struct{}),
		attached: make(chan struct{}),
	}
	upstream := []protocol.SessionIdentity{
		{HostID: "host-a", SessionID: "A111"},
		{HostID: "host-root", SessionID: "R000"},
	}

	attachClient, attachServer := net.Pipe()
	defer attachClient.Close() //nolint:errcheck // test resource cleanup
	go w.serve(attachServer)
	if err := protocol.NewWriter(attachClient).WriteControlMsg(protocol.Control{
		Type:               protocol.TypeAttach,
		SessionID:          sid.String(),
		ContainingSessions: upstream,
	}); err != nil {
		t.Fatal(err)
	}
	attachReader := protocol.NewReader(attachClient)
	for range 2 {
		if _, err := attachReader.ReadFrame(); err != nil {
			t.Fatalf("read attach response: %v", err)
		}
	}

	response := queryWorkerContainment(t, w, sid.String())
	want := []protocol.SessionIdentity{
		{HostID: "host-b", SessionID: "B222"},
		{HostID: "host-a", SessionID: "A111"},
		{HostID: "host-root", SessionID: "R000"},
	}
	if response.Type != protocol.TypeContained || response.RequestID != "containment-1" || response.SessionID != sid.String() {
		t.Fatalf("containment response = %#v", response)
	}
	assertSessionIdentities(t, response.ContainingSessions, want)

	response.ContainingSessions[0].HostID = "mutated"
	second := queryWorkerContainment(t, w, sid.String())
	assertSessionIdentities(t, second.ContainingSessions, want)
}

func TestInvalidOrCyclicContainmentDoesNotDisplaceAttachment(t *testing.T) {
	sid, err := protocol.NewSessionID("B222")
	if err != nil {
		t.Fatal(err)
	}
	oldClient, oldServer := net.Pipe()
	defer oldClient.Close() //nolint:errcheck // test resource cleanup
	old := newAttachment(oldServer, sid)
	defer old.close()
	w := &Worker{
		cfg:    Config{ID: sid.String(), HostID: "host-b"},
		sid:    sid,
		client: old,
	}

	tests := []struct {
		name    string
		lineage []protocol.SessionIdentity
	}{
		{
			name: "invalid",
			lineage: []protocol.SessionIdentity{
				{HostID: "host-a", SessionID: "nope"},
			},
		},
		{
			name: "duplicate",
			lineage: []protocol.SessionIdentity{
				{HostID: "host-a", SessionID: "A111"},
				{HostID: "host-a", SessionID: "A111"},
			},
		},
		{
			name: "contains worker itself",
			lineage: []protocol.SessionIdentity{
				{HostID: "host-b", SessionID: "B222"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close() //nolint:errcheck // test resource cleanup
			go w.serve(server)
			if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
				Type:               protocol.TypeAttach,
				SessionID:          sid.String(),
				ContainingSessions: test.lineage,
			}); err != nil {
				t.Fatal(err)
			}
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			frame, err := protocol.NewReader(client).ReadFrame()
			if err != nil {
				t.Fatalf("read rejection: %v", err)
			}
			response, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Kind != protocol.KindControl || response.Type != protocol.TypeError {
				t.Fatalf("rejection = kind %v, message %#v", frame.Kind, response)
			}
			w.mu.Lock()
			current := w.client
			w.mu.Unlock()
			if current != old {
				t.Fatalf("invalid containment displaced active client: got %p, want %p", current, old)
			}
		})
	}
}

func TestContainmentQueryRequiresExactWorkerIdentity(t *testing.T) {
	sid, err := protocol.NewSessionID("B222")
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{cfg: Config{ID: sid.String()}, sid: sid}
	response := queryWorkerContainment(t, w, sid.String())
	if response.Type != protocol.TypeError {
		t.Fatalf("worker without host identity returned %#v", response)
	}

	w.cfg.HostID = "host-b"
	response = queryWorkerContainment(t, w, "A111")
	if response.Type != protocol.TypeError {
		t.Fatalf("wrong-session query returned %#v", response)
	}
}

func TestWorkerHostIDComesFromConfigOrEnvironment(t *testing.T) {
	if got, err := workerHostID(Config{HostID: "configured", Env: []string{MeshHostIDVariable + "=inherited"}}); err != nil || got != "configured" {
		t.Fatalf("configured host ID = %q, %v", got, err)
	}
	if got, err := workerHostID(Config{Env: []string{"A=B", MeshHostIDVariable + "=inherited"}}); err != nil || got != "inherited" {
		t.Fatalf("inherited host ID = %q, %v", got, err)
	}
	if _, err := workerHostID(Config{HostID: " invalid "}); err == nil {
		t.Fatal("non-canonical configured host ID was accepted")
	}
}

func queryWorkerContainment(t *testing.T, w *Worker, sessionID string) protocol.Control {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close() //nolint:errcheck // test resource cleanup
	go w.serve(server)
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeContainment,
		RequestID: "containment-1",
		SessionID: sessionID,
	}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := protocol.NewReader(client).ReadFrame()
	if err != nil {
		t.Fatalf("read containment: %v", err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != protocol.KindControl {
		t.Fatalf("containment response kind = %v", frame.Kind)
	}
	return response
}

func assertSessionIdentities(t *testing.T, got, want []protocol.SessionIdentity) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("session identities = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("session identity %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

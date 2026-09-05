package worker

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func newNestingTestWorker(t *testing.T) *Worker {
	t.Helper()
	sid, err := protocol.NewSessionID("A111")
	if err != nil {
		t.Fatal(err)
	}
	pty := newPipePTY()
	w := &Worker{
		cfg: Config{ID: sid.String(), HostID: "host-a"}, sid: sid, pty: pty,
		ring: session.NewRing(ringSize), screen: terminalstate.NewScreen(80, 24),
		exited: make(chan struct{}), attached: make(chan struct{}),
		observeProcess: func(int, int) processObservation { return processObservation{} },
	}
	t.Cleanup(func() {
		w.finish(0)
		_ = pty.Close()
	})
	return w
}

func nestingTestConnection(t *testing.T, w *Worker, request protocol.Control) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go w.serve(server)
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.NewWriter(client).WriteControlMsg(request); err != nil {
		t.Fatal(err)
	}
	return client
}

func readNestingTestControl(t *testing.T, conn net.Conn) protocol.Control {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := protocol.NewReader(conn)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Kind != protocol.KindControl {
			continue
		}
		response, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
}

func registerNestingTestClient(t *testing.T, w *Worker, target protocol.SessionIdentity) net.Conn {
	t.Helper()
	conn := nestingTestConnection(t, w, protocol.Control{
		Type: protocol.TypeNest, SessionID: w.cfg.ID, RequestID: "register-1", NestedSession: &target,
	})
	response := readNestingTestControl(t, conn)
	if response.Type != protocol.TypeNesting || !response.NestingSupported || response.RequestID != "register-1" || response.SessionID != w.cfg.ID {
		t.Fatalf("registration response = %#v", response)
	}
	return conn
}

func TestNestingUpdatesAttachmentInspectionAndSteal(t *testing.T) {
	w := newNestingTestWorker(t)
	zero := uint64(0)
	outer := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeAttach, LastSeq: &zero, NestingSupported: true})
	initial := readNestingTestControl(t, outer)
	if initial.Type != protocol.TypeAttached || !initial.NestingSupported || len(initial.Nested) != 0 {
		t.Fatalf("initial attachment = %#v", initial)
	}
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "B222"}
	registration := registerNestingTestClient(t, w, target)
	update := readNestingTestControl(t, outer)
	if update.Type != protocol.TypeNesting || !update.NestingSupported {
		t.Fatalf("nesting update = %#v", update)
	}
	assertSessionIdentities(t, update.Nested, []protocol.SessionIdentity{target})

	inspectionConn := nestingTestConnection(t, w, protocol.Control{
		Type: protocol.TypeInspect, PreviewCols: 80, PreviewRows: 4,
	})
	inspection := readNestingTestControl(t, inspectionConn)
	if inspection.Type != protocol.TypeInspected || inspection.Inspection == nil || !inspection.Inspection.NestingSupported || !inspection.Inspection.Attached {
		t.Fatalf("inspection = %#v", inspection)
	}
	assertSessionIdentities(t, inspection.Inspection.Nested, []protocol.SessionIdentity{target})

	replacement := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeAttach, LastSeq: &zero, NestingSupported: true})
	stolen := readNestingTestControl(t, outer)
	if stolen.Type != protocol.TypeDetach || stolen.Reason != protocol.ReasonStolen {
		t.Fatalf("old attachment response = %#v", stolen)
	}
	attached := readNestingTestControl(t, replacement)
	if attached.Type != protocol.TypeAttached || !attached.NestingSupported {
		t.Fatalf("replacement attachment = %#v", attached)
	}
	assertSessionIdentities(t, attached.Nested, []protocol.SessionIdentity{target})

	_ = replacement.Close()
	waitFor(t, func() bool { return !w.inspectSession(80, 4).Attached })
	assertSessionIdentities(t, w.inspectSession(80, 4).Nested, []protocol.SessionIdentity{target})
	resumed := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeAttachDetached, LastSeq: &zero, NestingSupported: true})
	assertSessionIdentities(t, readNestingTestControl(t, resumed).Nested, []protocol.SessionIdentity{target})
	_ = registration.Close()
	empty := readNestingTestControl(t, resumed)
	if empty.Type != protocol.TypeNesting || !empty.NestingSupported || len(empty.Nested) != 0 {
		t.Fatalf("unregistered state = %#v", empty)
	}
	if nested := w.inspectSession(80, 4).Nested; len(nested) != 0 {
		t.Fatalf("inspection retained closed registration: %#v", nested)
	}
}

func TestNestingRegistrationLimitCountsConnections(t *testing.T) {
	w := newNestingTestWorker(t)
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "B222"}
	var registrations []net.Conn
	for range protocol.MaxNestedSessions {
		registrations = append(registrations, registerNestingTestClient(t, w, target))
	}
	assertSessionIdentities(t, w.inspectSession(80, 4).Nested, []protocol.SessionIdentity{target})
	rejected := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeNest, SessionID: w.cfg.ID, NestedSession: &target})
	if response := readNestingTestControl(t, rejected); response.Type != protocol.TypeError {
		t.Fatalf("registration beyond limit = %#v", response)
	}
	_ = registrations[0].Close()
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.nesting) == protocol.MaxNestedSessions-1
	})
	registerNestingTestClient(t, w, target)
}

func TestNestingFallsBackWhenContainingAttachmentCannotForwardDefaultKey(t *testing.T) {
	w := newNestingTestWorker(t)
	zero := uint64(0)
	outer := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeAttach, LastSeq: &zero})
	readNestingTestControl(t, outer)
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "B222"}
	registration := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeNest, SessionID: w.cfg.ID, NestedSession: &target})
	response := readNestingTestControl(t, registration)
	if response.Type != protocol.TypeError || response.NestingSupported {
		t.Fatalf("incapable containing attachment registration = %#v; want unsupported", response)
	}
	if nested := w.inspectSession(80, 4).Nested; len(nested) != 0 {
		t.Fatalf("unsupported registration survived: %#v", nested)
	}
}

func TestNestingRejectsIncompatibleTakeoverBeforeResizing(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(fmt.Sprintf("detached=%t", detached), func(t *testing.T) {
			w := newNestingTestWorker(t)
			registration := registerNestingTestClient(t, w, protocol.SessionIdentity{HostID: "pc", SessionID: "B222"})
			zero := uint64(0)
			outer := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeAttach, LastSeq: &zero, NestingSupported: true, Cols: 80, Rows: 24})
			readNestingTestControl(t, outer)
			if detached {
				_ = outer.Close()
				waitFor(t, func() bool { return !w.inspectSession(80, 4).Attached })
			}
			w.mu.Lock()
			owner := w.client
			w.mu.Unlock()
			kind := protocol.TypeAttach
			if detached {
				kind = protocol.TypeAttachDetached
			}
			incoming := nestingTestConnection(t, w, protocol.Control{Type: kind, LastSeq: &zero, Cols: 100, Rows: 30})
			if response := readNestingTestControl(t, incoming); response.Type != protocol.TypeError {
				t.Errorf("incompatible takeover = %#v", response)
			}
			w.mu.Lock()
			unchanged := w.client == owner
			w.mu.Unlock()
			cols, rows, err := w.pty.Size()
			if !unchanged || err != nil || cols != 80 || rows != 24 {
				t.Fatalf("incompatible takeover changed owner=%t or dimensions=%dx%d, %v", !unchanged, cols, rows, err)
			}
			_ = registration.Close()
			waitFor(t, func() bool { return len(w.inspectSession(80, 4).Nested) == 0 })
			incoming = nestingTestConnection(t, w, protocol.Control{Type: kind, LastSeq: &zero})
			if response := readNestingTestControl(t, incoming); response.Type != protocol.TypeAttached {
				t.Fatalf("legacy attachment after nesting ended = %#v", response)
			}
		})
	}
}

func TestNestingRejectsInvalidIdentityAndCycles(t *testing.T) {
	w := newNestingTestWorker(t)
	parent := protocol.SessionIdentity{HostID: "root", SessionID: "R000"}
	zero := uint64(0)
	outer := nestingTestConnection(t, w, protocol.Control{
		Type: protocol.TypeAttach, LastSeq: &zero, ContainingSessions: []protocol.SessionIdentity{parent}, NestingSupported: true,
	})
	readNestingTestControl(t, outer)
	w.mu.Lock()
	owner := w.client
	w.mu.Unlock()
	for _, target := range []*protocol.SessionIdentity{
		nil,
		{HostID: "pc", SessionID: "invalid"},
		{HostID: "host-a", SessionID: "A111"},
		&parent,
	} {
		conn := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeNest, SessionID: w.cfg.ID, NestedSession: target})
		if response := readNestingTestControl(t, conn); response.Type != protocol.TypeError {
			t.Fatalf("invalid registration accepted: %#v", response)
		}
	}
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "B222"}
	wrongWorker := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeNest, SessionID: "C333", NestedSession: &target})
	if response := readNestingTestControl(t, wrongWorker); response.Type != protocol.TypeError {
		t.Fatalf("wrong worker registration = %#v", response)
	}
	registerNestingTestClient(t, w, target)
	readNestingTestControl(t, outer)
	cycle := nestingTestConnection(t, w, protocol.Control{
		Type: protocol.TypeAttach, ContainingSessions: []protocol.SessionIdentity{target},
	})
	if response := readNestingTestControl(t, cycle); response.Type != protocol.TypeError {
		t.Fatalf("attachment introduced a nesting cycle: %#v", response)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != owner {
		t.Fatal("invalid request displaced the current attachment")
	}
}

func TestDetachedAttachHasOneWinnerWithoutLoserResize(t *testing.T) {
	w := newNestingTestWorker(t)
	type outcome struct {
		request  protocol.Control
		response protocol.Control
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, 8)
	zero := uint64(0)
	for index := range cap(results) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		go w.serve(server)
		go func() {
			<-start
			request := protocol.Control{Type: protocol.TypeAttachDetached, LastSeq: &zero, Cols: 80 + index, Rows: 24 + index}
			_ = client.SetDeadline(time.Now().Add(2 * time.Second))
			err := protocol.NewWriter(client).WriteControlMsg(request)
			var response protocol.Control
			if err == nil {
				var frame protocol.Frame
				frame, err = protocol.NewReader(client).ReadFrame()
				if err == nil {
					response, err = protocol.DecodeControl(frame.Payload)
				}
			}
			results <- outcome{request, response, err}
		}()
	}
	close(start)
	var winner protocol.Control
	winners := 0
	for range cap(results) {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		switch got.response.Type {
		case protocol.TypeAttached:
			winners++
			winner = got.request
		case protocol.TypeError:
			if got.response.Reason != protocol.ReasonAttached {
				t.Fatalf("detached-only rejection = %#v", got.response)
			}
		default:
			t.Fatalf("unexpected response = %#v", got.response)
		}
	}
	if winners != 1 {
		t.Fatalf("successful claims = %d, want 1", winners)
	}
	cols, rows, err := w.pty.Size()
	if err != nil || cols != winner.Cols || rows != winner.Rows {
		t.Fatalf("PTY size = %dx%d, %v; winner requested %dx%d", cols, rows, err, winner.Cols, winner.Rows)
	}
}

func TestFinishClosesNestingIncludingBlockedAcknowledgement(t *testing.T) {
	w := newNestingTestWorker(t)
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "B222"}
	first := registerNestingTestClient(t, w, target)
	blocked := nestingTestConnection(t, w, protocol.Control{Type: protocol.TypeNest, SessionID: w.cfg.ID, NestedSession: &target})
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.nesting) == 2
	})
	done := make(chan struct{})
	go func() { w.finish(0); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited on a registration reader or acknowledgement")
	}
	for _, conn := range []net.Conn{first, blocked} {
		buffer := make([]byte, 1)
		if _, err := conn.Read(buffer); err == nil {
			t.Fatal("registration connection survived worker shutdown")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.nesting) != 0 {
		t.Fatal("shutdown retained registrations")
	}
}

func TestNestingClientProcess(t *testing.T) {
	address := os.Getenv("MESH_NESTING_TEST_SOCKET")
	if address == "" {
		return
	}
	conn, err := net.Dial("unix", address) //nolint:gosec // address is this test's local Unix socket, never a network endpoint
	if err != nil {
		t.Fatal(err)
	}
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "B222"}
	if err := protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
		Type: protocol.TypeNest, SessionID: "A111", NestedSession: &target,
	}); err != nil {
		t.Fatal(err)
	}
	response := readNestingTestControl(t, conn)
	if response.Type != protocol.TypeNesting {
		t.Fatalf("registration = %#v", response)
	}
	fmt.Println("registered")
	_, _ = io.Copy(io.Discard, conn)
	_ = conn.Close()
}

func TestRunUnregistersCrashedClientAndClosesLiveRegistration(t *testing.T) {
	dir, err := os.MkdirTemp("", "mesh-nesting-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	done := make(chan error, 1)
	go func() {
		_, runErr := Run(Config{ID: "A111", HostID: "host-a", Dir: dir, Command: []string{"/bin/sh", "-c", "exec sleep 30"}})
		done <- runErr
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			conn, dialErr := net.DialTimeout("unix", paths.Socket(dir), time.Second)
			if dialErr == nil {
				_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeSignal, Signal: "kill"})
				_ = conn.Close()
			}
			select {
			case runErr := <-done:
				if runErr != nil {
					t.Errorf("Run = %v", runErr)
				}
			case <-time.After(2 * time.Second):
				t.Error("worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	waitFor(t, func() bool {
		conn, dialErr := net.DialTimeout("unix", paths.Socket(dir), 20*time.Millisecond)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	child := exec.Command(os.Args[0], "-test.run=^TestNestingClientProcess$") //nolint:gosec // re-execute this test binary as the registration client
	child.Env = append(os.Environ(), "MESH_NESTING_TEST_SOCKET="+paths.Socket(dir))
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "registered\n" {
			t.Fatalf("child readiness = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not register")
	}
	inspect := func() protocol.SessionInspection {
		conn, dialErr := net.DialTimeout("unix", paths.Socket(dir), time.Second)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		defer conn.Close() //nolint:errcheck // test resource cleanup
		if err := protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeInspect, PreviewCols: 80, PreviewRows: 4}); err != nil {
			t.Fatal(err)
		}
		response := readNestingTestControl(t, conn)
		if response.Inspection == nil {
			t.Fatalf("inspection = %#v", response)
		}
		return *response.Inspection
	}
	assertSessionIdentities(t, inspect().Nested, []protocol.SessionIdentity{{HostID: "pc", SessionID: "B222"}})
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waitFor(t, func() bool { return len(inspect().Nested) == 0 })
	registration, err := net.Dial("unix", paths.Socket(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close() //nolint:errcheck // test resource cleanup
	target := protocol.SessionIdentity{HostID: "pc", SessionID: "C333"}
	if err := protocol.NewWriter(registration).WriteControlMsg(protocol.Control{Type: protocol.TypeNest, SessionID: "A111", NestedSession: &target}); err != nil {
		t.Fatal(err)
	}
	if response := readNestingTestControl(t, registration); response.Type != protocol.TypeNesting {
		t.Fatalf("registration = %#v", response)
	}
	stop()
	if _, err := protocol.NewReader(registration).ReadFrame(); err == nil {
		t.Fatal("registration survived session process exit")
	}
}

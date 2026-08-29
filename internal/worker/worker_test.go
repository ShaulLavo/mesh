package worker

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func TestFreshAttachReceivesSnapshotThenLiveSequence(t *testing.T) {
	sid, err := protocol.NewSessionID("SNAP")
	if err != nil {
		t.Fatal(err)
	}
	pty := newPipePTY()
	defer pty.Close()
	screen := terminalstate.NewScreen(20, 6)
	output := []byte("history\r\n\x1b[?1049h\x1b[2J\x1b[3;4H\x1b[31mNOW\x1b[0m")
	if _, err := screen.Write(output); err != nil {
		t.Fatal(err)
	}
	ring := session.NewRing(ringSize)
	_, _ = ring.Write(output)
	w := &Worker{
		cfg:      Config{ID: sid.String()},
		sid:      sid,
		pty:      pty,
		ring:     ring,
		screen:   screen,
		exited:   make(chan struct{}),
		attached: make(chan struct{}),
	}

	client, server := net.Pipe()
	defer client.Close()
	go w.serve(server)
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: sid.String(),
	}); err != nil {
		t.Fatal(err)
	}

	r := protocol.NewReader(client)
	f, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	attached, err := protocol.DecodeControl(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	head := uint64(len(output))
	if f.Kind != protocol.KindControl || attached.Type != protocol.TypeAttached || !attached.Snapshot || attached.Seq != head {
		t.Fatalf("attach response = kind %v, message %+v", f.Kind, attached)
	}

	wantSnapshot := screen.Snapshot()
	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindSnapshot || f.Session != sid {
		t.Fatalf("snapshot frame = %+v", f)
	}
	if !bytes.Equal(f.Payload, wantSnapshot) {
		t.Fatalf("snapshot = %q, want %q", f.Payload, wantSnapshot)
	}

	live := []byte("!")
	w.mu.Lock()
	_, _ = w.ring.Write(live)
	_, _ = w.screen.Write(live)
	if !w.client.enqueueData(head, live) {
		w.mu.Unlock()
		t.Fatal("queue live output")
	}
	w.mu.Unlock()

	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindData || f.Seq != head || !bytes.Equal(f.Payload, live) {
		t.Fatalf("live frame = %+v", f)
	}
}

func TestExpiredAttachReceivesSnapshotAtCurrentHead(t *testing.T) {
	sid, err := protocol.NewSessionID("GAP")
	if err != nil {
		t.Fatal(err)
	}
	pty := newPipePTY()
	defer pty.Close()
	ring := session.NewRing(ringSize)
	output := make([]byte, ringSize+1)
	_, _ = ring.Write(output)
	screen := &recordingScreen{output: []byte("current screen")}
	w := &Worker{
		cfg:      Config{ID: sid.String()},
		sid:      sid,
		pty:      pty,
		ring:     ring,
		screen:   screen,
		exited:   make(chan struct{}),
		attached: make(chan struct{}),
	}

	client, server := net.Pipe()
	defer client.Close()
	go w.serve(server)
	lastSeq := uint64(0)
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: sid.String(),
		LastSeq:   &lastSeq,
	}); err != nil {
		t.Fatal(err)
	}

	r := protocol.NewReader(client)
	f, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	attached, err := protocol.DecodeControl(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !attached.Snapshot || attached.Seq != uint64(len(output)) {
		t.Fatalf("attach response = %+v", attached)
	}
	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindSnapshot || !bytes.Equal(f.Payload, screen.output) {
		t.Fatalf("snapshot frame = %+v", f)
	}
}

func TestResizeUpdatesPTYAndRenderedScreen(t *testing.T) {
	pty := newPipePTY()
	defer pty.Close()
	screen := &recordingScreen{}
	w := &Worker{pty: pty, screen: screen}

	w.resize(132, 43)

	cols, rows, err := pty.Size()
	if err != nil {
		t.Fatal(err)
	}
	if cols != 132 || rows != 43 {
		t.Fatalf("PTY size = %dx%d, want 132x43", cols, rows)
	}
	if screen.cols != 132 || screen.rows != 43 {
		t.Fatalf("screen size = %dx%d, want 132x43", screen.cols, screen.rows)
	}
}

func TestPumpFeedsRenderedScreenAndReplayRing(t *testing.T) {
	pty := newPipePTY()
	defer pty.Close()
	screen := &recordingScreen{}
	w := &Worker{
		pty:      pty,
		ring:     session.NewRing(ringSize),
		screen:   screen,
		pumpDone: make(chan struct{}),
	}
	go w.pump()

	output := []byte("render me")
	if _, err := pty.emit(output); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return bytes.Equal(screen.output, output)
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if !bytes.Equal(screen.output, output) {
		t.Fatalf("screen output = %q, want %q", screen.output, output)
	}
	replay, _, ok := w.ring.Since(0)
	if !ok || !bytes.Equal(replay, output) {
		t.Fatalf("ring replay = %q, ok %v", replay, ok)
	}
}

func TestPumpDisownsSlowClientWithoutBlockingPTY(t *testing.T) {
	const slowClientQueueBudget = 4 << 20

	pty := newPipePTY()
	defer pty.Close()

	sid, err := protocol.NewSessionID("SLOW")
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{
		cfg:      Config{ID: sid.String()},
		sid:      sid,
		pty:      pty,
		ring:     session.NewRing(ringSize),
		screen:   discardScreen{},
		exited:   make(chan struct{}),
		pumpDone: make(chan struct{}),
		attached: make(chan struct{}),
	}
	go w.pump()

	client, server := net.Pipe()
	defer client.Close()
	go w.serve(server)
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: sid.String(),
	}); err != nil {
		t.Fatal(err)
	}

	// net.Pipe has no socket buffer. Leaving the client unread pins the
	// attachment writer immediately and makes queue overflow deterministic.
	output := make([]byte, 2*slowClientQueueBudget+(64<<10))
	for i := range output {
		output[i] = byte(i) ^ byte(i>>15)
	}
	drained := make(chan error, 1)
	go func() {
		_, err := pty.emit(output)
		drained <- err
	}()

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("write PTY output: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PTY output blocked behind an unread client")
	}

	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.client == nil && w.ring.Head() == uint64(len(output))
	})

	// Resume at the oldest valid offset. The replacement client must receive a
	// full ring, split into writable protocol frames, without a gap.
	lastSeq := w.ring.Tail()
	client2, server2 := net.Pipe()
	defer client2.Close()
	go w.serve(server2)
	if err := protocol.NewWriter(client2).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: sid.String(),
		LastSeq:   &lastSeq,
	}); err != nil {
		t.Fatal(err)
	}

	r := protocol.NewReader(client2)
	f, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	attached, err := protocol.DecodeControl(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindControl || attached.Type != protocol.TypeAttached || attached.Seq != lastSeq {
		t.Fatalf("attach response = kind %v, message %+v", f.Kind, attached)
	}

	want := output[lastSeq:]
	got := make([]byte, 0, len(want))
	for len(got) < len(want) {
		f, err = r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		seq := lastSeq + uint64(len(got))
		if f.Kind != protocol.KindData || f.Seq != seq {
			t.Fatalf("replay header = kind %v, seq %d, want seq %d", f.Kind, f.Seq, seq)
		}
		got = append(got, f.Payload...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replay length = %d, want %d exact bytes", len(got), len(want))
	}
}

func TestAttachmentCopiesDataAndReservesControlCapacity(t *testing.T) {
	sid, err := protocol.NewSessionID("QUEUE")
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	a := newAttachment(server, sid)

	payload := []byte{0}
	for i := range outboundQueueFrameLimit {
		payload[0] = byte(i)
		if !a.enqueueData(uint64(i), payload) {
			t.Fatalf("data frame %d rejected before frame limit", i)
		}
	}
	payload[0] = 0xff
	if a.enqueueData(outboundQueueFrameLimit, payload) {
		t.Fatal("data frame accepted beyond frame limit")
	}
	if !a.enqueueControl(protocol.Control{
		Type:      protocol.TypeDetach,
		SessionID: sid.String(),
		Reason:    protocol.ReasonStolen,
	}, true) {
		t.Fatal("control frame rejected by a full data queue")
	}

	w := &Worker{}
	w.mu.Lock()
	a.startLocked(w)
	w.mu.Unlock()
	r := protocol.NewReader(client)
	for i := range outboundQueueFrameLimit {
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if f.Kind != protocol.KindData || f.Seq != uint64(i) || len(f.Payload) != 1 || f.Payload[0] != byte(i) {
			t.Fatalf("data frame %d = kind %v, seq %d, payload %v", i, f.Kind, f.Seq, f.Payload)
		}
	}
	f, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeControl(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindControl || msg.Type != protocol.TypeDetach || msg.Reason != protocol.ReasonStolen {
		t.Fatalf("reserved control = kind %v, message %+v", f.Kind, msg)
	}
	<-a.done
}

func TestAttachmentReservesLiveFrameAfterFullReplay(t *testing.T) {
	sid, err := protocol.NewSessionID("BYTES")
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	a := newAttachment(server, sid)
	defer a.close()

	chunk := make([]byte, outboundDataFrameSize)
	for seq := 0; seq < ringSize; seq += len(chunk) {
		if !a.enqueueData(uint64(seq), chunk) {
			t.Fatalf("replay rejected at sequence %d", seq)
		}
	}
	if !a.enqueueData(ringSize, chunk) {
		t.Fatal("one live PTY frame rejected after a full replay")
	}
	if a.enqueueData(outboundQueueByteLimit, []byte{0}) {
		t.Fatal("data accepted beyond byte limit")
	}
	if !a.enqueueControl(protocol.Control{Type: protocol.TypeExit}, true) {
		t.Fatal("control frame rejected at byte limit")
	}
}

func TestAttachmentKeepsSnapshotInOneBoundedFrame(t *testing.T) {
	sid, err := protocol.NewSessionID("SNAP")
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	maxSnapshot := protocol.MaxPayload - len(sid)
	a := newAttachment(server, sid)
	if !a.enqueueSnapshot(make([]byte, maxSnapshot)) {
		t.Fatal("maximum-size snapshot was rejected")
	}

	oversized := newAttachment(server, sid)
	if oversized.enqueueSnapshot(make([]byte, maxSnapshot+1)) {
		t.Fatal("snapshot larger than one protocol payload was accepted")
	}
}

func TestFinishWaitsForQueuedOutputAndExit(t *testing.T) {
	sid, err := protocol.NewSessionID("EXIT")
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	notifyingServer := &writeNotifyConn{Conn: server, entered: make(chan struct{})}
	a := newAttachment(notifyingServer, sid)
	defer a.close()
	w := &Worker{
		cfg:    Config{ID: sid.String()},
		sid:    sid,
		client: a,
		exited: make(chan struct{}),
	}
	if !a.enqueueData(0, []byte("final output")) {
		t.Fatal("queue final output")
	}
	w.mu.Lock()
	a.startLocked(w)
	w.mu.Unlock()
	<-notifyingServer.entered

	finished := make(chan struct{})
	go func() {
		w.finish(7)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("finish returned before the attachment writer flushed")
	case <-time.After(50 * time.Millisecond):
	}

	r := protocol.NewReader(client)
	f, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindData || string(f.Payload) != "final output" {
		t.Fatalf("final data frame = %+v", f)
	}
	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeControl(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != protocol.KindControl || msg.Type != protocol.TypeExit || msg.ExitCode == nil || *msg.ExitCode != 7 {
		t.Fatalf("exit frame = kind %v, message %+v", f.Kind, msg)
	}

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("finish did not return after the attachment writer flushed")
	}
}

func TestRunDoesNotWaitForDescendantHoldingPTY(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "descendant.pid")
	t.Cleanup(func() {
		b, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
		if err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := Run(Config{
			ID:      "CHILD",
			Dir:     dir,
			Command: []string{"/bin/sh", "-c", `sleep 30 & echo $! > "$1"; sleep .05`, "sh", pidPath},
			Cwd:     dir,
			Env:     os.Environ(),
			Cols:    80,
			Rows:    24,
		})
		done <- result{code: code, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil || got.code != 0 {
			t.Fatalf("Run = code %d, error %v", got.code, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker waited for a descendant that inherited the PTY")
	}
}

func TestFinishedWorkerDoesNotInstallNewAttachment(t *testing.T) {
	sid, err := protocol.NewSessionID("DONE")
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{
		cfg:    Config{ID: sid.String()},
		sid:    sid,
		ring:   session.NewRing(ringSize),
		exited: make(chan struct{}),
	}
	w.finish(7)

	client, server := net.Pipe()
	defer client.Close()
	go w.serve(server)
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: sid.String(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := protocol.NewReader(client).ReadFrame(); err == nil {
		t.Fatal("finished worker accepted a new attachment")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil || len(w.attachments) != 0 {
		t.Fatalf("finished worker retained client %p and %d writers", w.client, len(w.attachments))
	}
}

func TestFinishWaitsForDisplacedAttachmentWriters(t *testing.T) {
	sid, err := protocol.NewSessionID("ALL")
	if err != nil {
		t.Fatal(err)
	}
	oldClient, oldServer := net.Pipe()
	defer oldClient.Close()
	oldConn := &writeNotifyConn{Conn: oldServer, entered: make(chan struct{})}
	old := newAttachment(oldConn, sid)
	defer old.close()
	if !old.enqueueData(0, []byte("old")) || !old.enqueueControl(protocol.Control{
		Type:   protocol.TypeDetach,
		Reason: protocol.ReasonStolen,
	}, true) {
		t.Fatal("queue displaced attachment")
	}

	activeClient, activeServer := net.Pipe()
	defer activeClient.Close()
	activeConn := &writeNotifyConn{Conn: activeServer, entered: make(chan struct{})}
	active := newAttachment(activeConn, sid)
	defer active.close()
	if !active.enqueueData(3, []byte("active")) {
		t.Fatal("queue active attachment")
	}

	w := &Worker{
		cfg:    Config{ID: sid.String()},
		sid:    sid,
		client: active,
		exited: make(chan struct{}),
	}
	w.mu.Lock()
	old.startLocked(w)
	active.startLocked(w)
	w.mu.Unlock()
	<-oldConn.entered
	<-activeConn.entered

	finished := make(chan struct{})
	go func() {
		w.finish(9)
		close(finished)
	}()

	activeReader := protocol.NewReader(activeClient)
	if _, err := activeReader.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := activeReader.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		t.Fatal("finish returned while a displaced writer was still blocked")
	case <-time.After(50 * time.Millisecond):
	}

	oldReader := protocol.NewReader(oldClient)
	if _, err := oldReader.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := oldReader.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("finish did not return after every writer flushed")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

type pipePTY struct {
	r *io.PipeReader
	w *io.PipeWriter

	mu            sync.Mutex
	width, height int
}

func newPipePTY() *pipePTY {
	r, w := io.Pipe()
	return &pipePTY{r: r, w: w}
}

func (p *pipePTY) emit(b []byte) (int, error)  { return p.w.Write(b) }
func (p *pipePTY) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipePTY) Write(b []byte) (int, error) { return len(b), nil }
func (p *pipePTY) Close() error {
	err := p.r.Close()
	_ = p.w.Close()
	return err
}
func (p *pipePTY) Fd() uintptr { return 0 }
func (p *pipePTY) Resize(width, height int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width, p.height = width, height
	return nil
}
func (p *pipePTY) Size() (width, height int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.width, p.height, nil
}
func (p *pipePTY) Name() string          { return "pipe" }
func (p *pipePTY) Start(*exec.Cmd) error { return nil }

type writeNotifyConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

type discardScreen struct{}

func (discardScreen) Write(p []byte) (int, error) { return len(p), nil }
func (discardScreen) Resize(int, int)             {}
func (discardScreen) Snapshot() []byte            { return nil }

type recordingScreen struct {
	output     []byte
	cols, rows int
}

func (s *recordingScreen) Write(p []byte) (int, error) {
	s.output = append(s.output, p...)
	return len(p), nil
}
func (s *recordingScreen) Resize(cols, rows int) { s.cols, s.rows = cols, rows }
func (s *recordingScreen) Snapshot() []byte      { return bytes.Clone(s.output) }

func (c *writeNotifyConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

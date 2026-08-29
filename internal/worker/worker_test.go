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
)

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
func (p *pipePTY) Fd() uintptr                          { return 0 }
func (p *pipePTY) Resize(width, height int) error       { return nil }
func (p *pipePTY) Size() (width, height int, err error) { return 80, 24, nil }
func (p *pipePTY) Name() string                         { return "pipe" }
func (p *pipePTY) Start(*exec.Cmd) error                { return nil }

type writeNotifyConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *writeNotifyConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

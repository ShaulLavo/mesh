package worker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

func newReviewPipeInput(t *testing.T, kind string, limit int) (*pipeInput, *os.File, execution.Binding, *execution.Output) {
	t.Helper()
	root := t.TempDir()
	j, err := execution.Open(root, BootID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	key := execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "job"}
	if kind == execution.KindLine {
		key.Class = execution.InternalServiceClass
	}
	i, err := execution.Normalize(execution.Intent{Kind: kind, Stdin: execution.StdinOpen, Cwd: root, Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	b := claimReviewPipeInput(t, root, j, key, i)
	a, err := execution.StartAttempt(t.Context(), root, b, i, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.PrepareInput(); err != nil {
		t.Fatal(err)
	}
	reads, writes, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reads.Close() })
	p := newPipeInput(writes, a, session.MaxInputBytes, kind == execution.KindLine)
	t.Cleanup(p.stop)
	p.receiptLimit = limit
	out, err := execution.OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	return p, reads, b, out
}

func claimReviewPipeInput(t *testing.T, root string, j *execution.Journal, key execution.Key, i execution.Intent) execution.Binding {
	t.Helper()
	r, created, err := j.Reserve(t.Context(), key, i, func() (execution.Binding, error) {
		ref, err := Reserve(filepath.Join(root, "s"), execution.PurposeForKind(i.Kind))
		return execution.NewBinding(key, i, ref, nil), err
	})
	if err != nil || !created {
		t.Fatalf("reserve pipe input fixture: %v", err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err != nil {
		t.Fatal(err)
	}
	return r.Binding
}

func TestPipeInputFullReceiptBudgetStillAcceptsEOF(t *testing.T) {
	p, child, b, output := newReviewPipeInput(t, execution.KindPipe, 1)
	data := []byte("A")
	admitReviewWrite(t, p, session.InputReceipt{WriteKey: "bytes", Offset: 0, Length: 1}, data)
	extra := session.InputReceipt{WriteKey: "extra", Offset: 1, Length: 1}
	if _, err := p.admit(extra, data); !errors.Is(err, execution.ErrInputLimit) {
		t.Fatalf("full receipt budget: %v", err)
	}
	eof := session.InputReceipt{WriteKey: "eof", Offset: 1, Closed: true}
	admitReviewWrite(t, p, eof, nil)
	admitReviewWrite(t, p, eof, nil)
	saved, err := output.Input(b, "eof", &eof, nil)
	if err != nil || *saved != eof {
		t.Fatalf("lost EOF receipt: %+v %v", saved, err)
	}
	_ = child.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(child)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("expected exactly one byte followed by EOF: %q %v", got, err)
	}
}

func TestLineAcknowledgedReceiptsCanRetireWithoutReplay(t *testing.T) {
	p, child, b, out := newReviewPipeInput(t, execution.KindLine, 1)
	data := []byte("x\n")
	first := session.InputReceipt{WriteKey: "0:first", Offset: 0, Length: 2}
	second := session.InputReceipt{WriteKey: "2:second", Offset: 2, Length: 2}
	admitReviewWrite(t, p, first, data)
	admitReviewWrite(t, p, first, data)
	if _, err := p.admit(second, data); !errors.Is(err, execution.ErrInputLimit) {
		t.Fatalf("managed receipt budget: %v", err)
	}
	for range 2 {
		if err := p.retireLine(first.WriteKey); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := out.Input(b, first.WriteKey, &first, data); !errors.Is(err, execution.ErrInputUnknown) {
		t.Fatalf("retired receipt was still durable: %v", err)
	}
	if _, err := p.admit(first, data); !errors.Is(err, execution.ErrInputUnknown) {
		t.Fatalf("retired key was admitted again: %v", err)
	}
	changed := first
	changed.Offset = 2
	if _, err := p.admit(changed, data); !errors.Is(err, execution.ErrInputConflict) {
		t.Fatalf("retired key was accepted at a new offset: %v", err)
	}
	admitReviewWrite(t, p, second, data)
	_ = child.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(data)*2)
	if _, err := io.ReadFull(child, got); err != nil || string(got) != "x\nx\n" {
		t.Fatalf("managed worker input: %q %v", got, err)
	}
}

func admitReviewWrite(t *testing.T, p *pipeInput, intent session.InputReceipt, data []byte) {
	t.Helper()
	receipt, err := p.admit(intent, data)
	if err != nil || receipt == nil || *receipt != intent {
		t.Fatalf("unexpected write receipt: %+v %v", receipt, err)
	}
}

type slowLineFixture struct {
	net.Conn
	first    chan struct{}
	gap      chan struct{}
	release  <-chan struct{}
	dataOnce sync.Once
	gapOnce  sync.Once
}

func (c *slowLineFixture) Write(frame []byte) (int, error) {
	if len(frame) > 0 && frame[0] == byte(protocol.KindData) {
		c.dataOnce.Do(func() { close(c.first) })
	}
	if len(frame) > 0 && frame[0] == byte(protocol.KindControl) && bytes.Contains(frame, []byte(lineFrameGap)) {
		c.gapOnce.Do(func() {
			close(c.gap)
			<-c.release
		})
	}
	n, err := c.Conn.Write(frame)
	return n, workerExecutionError("write fixture frame", err)
}

func TestLineDuplexGapFencesInputBeforeFaultDelivery(t *testing.T) {
	p, _, b, _ := newReviewPipeInput(t, execution.KindLine, 1)
	sid, err := protocol.NewSessionID(b.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	release := make(chan struct{})
	var resume sync.Once
	unpause := func() { resume.Do(func() { close(release) }) }
	defer unpause()
	defer func() { _ = client.Close() }()
	slow := &slowLineFixture{Conn: server, first: make(chan struct{}), gap: make(chan struct{}), release: release}
	w := &Worker{cfg: Config{ID: b.Reservation.ID, Execution: &b}, sid: sid, pipe: &pipeExecution{input: p, lines: newLineOutput()}}
	done := make(chan struct{})
	go func() { defer close(done); w.serve(slow) }()
	t.Cleanup(func() { awaitPipeChannel(t, done) })

	writer, reader := protocol.NewWriter(client), protocol.NewReader(client)
	next := uint64(0)
	req := protocol.Control{Type: protocol.TypeWorkerLineAttach, SessionID: w.cfg.ID, Instance: b.Reservation.Instance, LaunchDigest: b.LaunchDigest, LastSeq: &next}
	if err := writer.WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	w.pipe.lines.offer([]byte("first\n"))
	awaitPipeChannel(t, slow.first)
	for range lineOutputFrames + 1 {
		w.pipe.lines.offer([]byte("past reader\n"))
	}
	if f, err := reader.ReadFrame(); err != nil || f.Seq != 0 {
		t.Fatalf("first held frame: %+v %v", f, err)
	}
	awaitPipeChannel(t, slow.gap)
	key := "0:fenced"
	payload := binary.BigEndian.AppendUint64(nil, 0)
	payload = append(payload, byte(len(key))) //nolint:gosec // fixed fixture key
	payload = append(payload, key...)
	payload = append(payload, "must not reach stdin\n"...)
	if err := writer.WriteInput(sid, payload); err != nil {
		t.Fatal(err)
	}
	unpause()
	faultFrame, err := reader.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.DecodeControl(faultFrame.Payload)
	if err != nil || reply.ErrorCode != lineFrameGap {
		t.Fatalf("expected the latched gap: %+v %v", reply, err)
	}
	_ = client.Close()
	awaitPipeChannel(t, done)
	if _, err := p.lookup(key); !errors.Is(err, execution.ErrInputUnknown) {
		t.Fatalf("input crossed the reported gap: %v", err)
	}
}

func awaitPipeChannel(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pipe fixture did not settle")
	}
}

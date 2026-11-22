package worker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

type terminalJobFixture struct {
	w       *Worker
	pty     *queryPTY
	binding execution.Binding
	output  *execution.Output
}

// newTerminalJobWorker builds a worker for one keyed terminal job with a
// durable attempt, so acknowledgments are recorded exactly as in production.
func newTerminalJobWorker(t *testing.T) terminalJobFixture {
	t.Helper()
	root := t.TempDir()
	j, err := execution.Open(root, BootID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	key := execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "terminal"}
	intent, err := execution.Normalize(execution.Intent{Kind: execution.KindPTY, Cwd: root, Command: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	r, created, err := j.Reserve(t.Context(), key, intent, func() (execution.Binding, error) {
		ref, err := Reserve(filepath.Join(root, "s"), execution.PurposeForKind(intent.Kind))
		return execution.NewBinding(key, intent, ref, nil), err
	})
	if err != nil || !created {
		t.Fatalf("reserve terminal fixture: %v", err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, intent); err != nil {
		t.Fatal(err)
	}
	attempt, err := execution.StartAttempt(t.Context(), root, r.Binding, intent, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = attempt.Close() })
	if err := attempt.PrepareInput(); err != nil {
		t.Fatal(err)
	}
	output, err := execution.OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	sid, err := protocol.NewSessionID(r.Binding.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	pty := &queryPTY{pipePTY: newPipePTY()}
	binding := r.Binding
	w := &Worker{
		cfg: Config{ID: sid.String(), Dir: t.TempDir(), Execution: &binding}, sid: sid, pty: pty,
		ring: session.NewRing(ringSize), screen: terminalstate.NewScreen(80, 24), attempt: attempt,
		exited: make(chan struct{}), attached: make(chan struct{}), pumpDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := queryStopWorker(ctx, w, pty); err != nil {
			t.Error(err)
		}
	})
	go w.pump()
	return terminalJobFixture{w: w, pty: pty, binding: binding, output: output}
}

func (f terminalJobFixture) input(t *testing.T, key string, offset uint64, data string) protocol.Control {
	t.Helper()
	return f.control(t, protocol.Control{Type: protocol.TypeExecutionInput, WriteKey: key, InputOffset: &offset, InputBytes: []byte(data)})
}

func (f terminalJobFixture) control(t *testing.T, req protocol.Control) protocol.Control {
	t.Helper()
	req.RequestID, req.SessionID, req.Instance = "terminal", f.w.cfg.ID, f.binding.Reservation.Instance
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = server.Close() }()
		f.w.serveTerminalControl(server, req)
	}()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.NewReader(client).ReadFrame()
	if err != nil {
		t.Fatalf("read terminal control reply: %v", err)
	}
	<-done
	reply, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestTerminalInputKeyedAcknowledgment(t *testing.T) {
	f := newTerminalJobWorker(t)
	first := f.input(t, "k1", 0, "ls\r")
	if first.Type != protocol.TypeOK || first.InputReceipt == nil || first.InputReceipt.Length != 3 {
		t.Fatalf("first write = %+v", first)
	}
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), []byte("ls\r")) })

	// A lost acknowledgment is recovered by an exact retry and by a lookup,
	// and neither writes the bytes again.
	if retry := f.input(t, "k1", 0, "ls\r"); retry.Type != protocol.TypeOK || *retry.InputReceipt != *first.InputReceipt {
		t.Fatalf("exact retry = %+v", retry)
	}
	lookup := f.control(t, protocol.Control{Type: protocol.TypeExecutionInput, WriteKey: "k1"})
	if lookup.Type != protocol.TypeOK || *lookup.InputReceipt != *first.InputReceipt {
		t.Fatalf("lookup = %+v", lookup)
	}
	if conflict := f.input(t, "k1", 0, "rm\r"); conflict.ErrorCode != "input.key_conflict" {
		t.Fatalf("conflicting retry = %+v", conflict)
	}
	if stale := f.input(t, "k2", 0, "pwd\r"); stale.ErrorCode != "input.offset_mismatch" {
		t.Fatalf("stale offset = %+v", stale)
	}
	if next := f.input(t, "k2", 3, "pwd\r"); next.Type != protocol.TypeOK {
		t.Fatalf("next write = %+v", next)
	}
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), []byte("ls\rpwd\r")) })
	time.Sleep(20 * time.Millisecond)
	if got := f.pty.inputs(); !bytes.Equal(got, []byte("ls\rpwd\r")) {
		t.Fatalf("PTY input = %q, want each acknowledged write exactly once", got)
	}

	// The durable record answers after the worker is gone.
	recovered, err := f.output.Input(f.binding, "k1", nil, nil)
	if err != nil || *recovered != *first.InputReceipt {
		t.Fatalf("durable acknowledgment = %+v, %v", recovered, err)
	}
	if eof := f.control(t, protocol.Control{Type: protocol.TypeExecutionInput, WriteKey: "eof", InputOffset: new(uint64(7)), InputEOF: true}); eof.ErrorCode != "usage.invalid" {
		t.Fatalf("terminal EOF = %+v", eof)
	}
}

func TestTerminalInputAndResizeRefusedWhileAttached(t *testing.T) {
	f := newTerminalJobWorker(t)
	if ok := f.input(t, "before", 0, "a"); ok.Type != protocol.TypeOK {
		t.Fatalf("write before attach = %+v", ok)
	}
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), []byte("a")) })
	_, reader, detach := queryAttach(t, f.w, nil)
	queryReadAttached(t, reader, f.w.sid, 0, true)
	width, height, _ := f.pty.Size()

	if refused := f.input(t, "during", 1, "b"); refused.ErrorCode != execution.CodeTerminalAttached {
		t.Fatalf("write while attached = %+v", refused)
	}
	resize := f.control(t, protocol.Control{Type: protocol.TypeExecutionResize, Cols: 132, Rows: 40})
	if resize.ErrorCode != execution.CodeTerminalAttached {
		t.Fatalf("resize while attached = %+v", resize)
	}
	if w, h, _ := f.pty.Size(); w != width || h != height {
		t.Fatalf("agent resize changed the attached terminal: %dx%d, want %dx%d", w, h, width, height)
	}
	// The write admitted before the person attached is still acknowledged.
	if retry := f.input(t, "before", 0, "a"); retry.Type != protocol.TypeOK {
		t.Fatalf("exact retry while attached = %+v", retry)
	}
	if got := f.pty.inputs(); !bytes.Equal(got, []byte("a")) {
		t.Fatalf("PTY input while attached = %q", got)
	}

	detach()
	if after := f.input(t, "after", 1, "c"); after.Type != protocol.TypeOK {
		t.Fatalf("write after detach = %+v", after)
	}
	if resized := f.control(t, protocol.Control{Type: protocol.TypeExecutionResize, Cols: 100, Rows: 30}); resized.Type != protocol.TypeOK || resized.Cols != 100 {
		t.Fatalf("resize after detach = %+v", resized)
	}
	if w, h, _ := f.pty.Size(); w != 100 || h != 30 {
		t.Fatalf("PTY size after agent resize = %dx%d", w, h)
	}
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), []byte("ac")) })
}

// A person attaching races agent writes. Every acknowledged write reaches the
// PTY once, and every refused write leaves no bytes.
func TestTerminalInputRacesHumanAttach(t *testing.T) {
	f := newTerminalJobWorker(t)
	stop := make(chan struct{})
	var attaching sync.WaitGroup
	attaching.Add(1)
	go func() {
		defer attaching.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, reader, detach := queryAttach(t, f.w, nil)
			_, _ = reader.ReadFrame()
			detach()
		}
	}()
	var want bytes.Buffer
	var offset uint64
	refused := 0
	for i := range 200 {
		payload := fmt.Sprintf("<%03d>", i)
		reply := f.input(t, fmt.Sprintf("w%03d", i), offset, payload)
		switch {
		case reply.Type == protocol.TypeOK:
			want.WriteString(payload)
			offset += uint64(len(payload))
		case reply.ErrorCode == execution.CodeTerminalAttached:
			refused++
		default:
			t.Fatalf("write %d = %+v", i, reply)
		}
	}
	close(stop)
	attaching.Wait()
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), want.Bytes()) })
	t.Logf("%d of 200 writes refused while attached", refused)
}

package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	terminalstate "github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/testenv"
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
	return f.raw(t, req)
}

// raw sends req exactly as given, for requests naming another instance.
func (f terminalJobFixture) raw(t *testing.T, req protocol.Control) protocol.Control {
	t.Helper()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = server.Close() }()
		f.w.serveTerminalControl(server, req)
	}()
	if err := client.SetDeadline(time.Now().Add(testenv.HangTimeout)); err != nil {
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

// personAttachment is one mesh attach client in a test. Every wait is
// bounded only by the hang cap.
type personAttachment struct {
	client net.Conn
	served chan struct{}
}

// attachPerson attaches like mesh attach and returns once the worker has
// confirmed the attachment, so the person holds the terminal.
func attachPerson(w *Worker) (*personAttachment, error) {
	client, server := net.Pipe()
	a := &personAttachment{client: client, served: make(chan struct{})}
	go func() {
		defer close(a.served)
		w.serve(server)
		w.writers.Wait()
	}()
	if err := client.SetDeadline(time.Now().Add(testenv.HangTimeout)); err != nil {
		return a, errors.Join(err, a.detach())
	}
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{Type: protocol.TypeAttach, SessionID: w.sid.String()}); err != nil {
		return a, errors.Join(err, a.detach())
	}
	frame, err := protocol.NewReader(client).ReadFrame()
	if err == nil && frame.Kind != protocol.KindControl {
		err = fmt.Errorf("first attach frame is %v", frame.Kind)
	}
	if err != nil {
		return a, errors.Join(err, a.detach())
	}
	go drainAttachment(client)
	return a, nil
}

// drainAttachment keeps reading so the worker's writer never blocks on a
// person who stopped looking.
func drainAttachment(client net.Conn) {
	reader := protocol.NewReader(client)
	for {
		if _, err := reader.ReadFrame(); err != nil {
			return
		}
	}
}

// detach closes the client and waits until the worker has let it go.
func (a *personAttachment) detach() error {
	_ = a.client.Close()
	select {
	case <-a.served:
		return nil
	case <-time.After(testenv.HangTimeout):
		return errors.New("worker did not release the attachment")
	}
}

func (f terminalJobFixture) detached() bool {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	return f.w.client == nil
}

// Both sides of the attach decision, proved in order: while a person holds
// the terminal, input and resize are refused; once they have left, both are
// admitted.
func TestTerminalInputAndResizeFollowAttachment(t *testing.T) {
	f := newTerminalJobWorker(t)
	var want bytes.Buffer
	var offset uint64
	for round := range 5 {
		person, err := attachPerson(f.w)
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("held-%d", round)
		if reply := f.input(t, key, offset, "x"); reply.ErrorCode != execution.CodeTerminalAttached {
			t.Fatalf("round %d: input while attached = %+v", round, reply)
		}
		if reply := f.control(t, protocol.Control{Type: protocol.TypeExecutionResize, Cols: 90 + round, Rows: 30}); reply.ErrorCode != execution.CodeTerminalAttached {
			t.Fatalf("round %d: resize while attached = %+v", round, reply)
		}
		if err := person.detach(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, f.detached)
		payload := fmt.Sprintf("<%d>", round)
		if reply := f.input(t, fmt.Sprintf("free-%d", round), offset, payload); reply.Type != protocol.TypeOK {
			t.Fatalf("round %d: input after detach = %+v", round, reply)
		}
		want.WriteString(payload)
		offset += uint64(len(payload))
		if reply := f.control(t, protocol.Control{Type: protocol.TypeExecutionResize, Cols: 100 + round, Rows: 30}); reply.Type != protocol.TypeOK {
			t.Fatalf("round %d: resize after detach = %+v", round, reply)
		}
	}
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), want.Bytes()) })
}

// A person attaching and detaching races agent writes and resizes. Every
// acknowledged write reaches the PTY once, every refused write leaves no
// bytes, and a resize is either applied or refused as attached.
func TestTerminalInputAndResizeRaceHumanAttach(t *testing.T) {
	f := newTerminalJobWorker(t)
	stop := make(chan struct{})
	attachErr := make(chan error, 1)
	var attaching sync.WaitGroup
	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		attaching.Wait()
	})
	attaching.Add(1)
	go func() {
		defer attaching.Done()
		attachErr <- attachUntilStopped(f.w, stop)
	}()
	var want bytes.Buffer
	var offset uint64
	outcomes := map[string]int{}
	for i := range 100 {
		payload := fmt.Sprintf("<%03d>", i)
		reply := f.input(t, fmt.Sprintf("w%03d", i), offset, payload)
		outcomes["input "+raceOutcome(t, reply)]++
		if reply.Type == protocol.TypeOK {
			want.WriteString(payload)
			offset += uint64(len(payload))
		}
		resize := f.control(t, protocol.Control{Type: protocol.TypeExecutionResize, Cols: 80 + i%40, Rows: 24})
		outcomes["resize "+raceOutcome(t, resize)]++
	}
	close(stop)
	attaching.Wait()
	if err := <-attachErr; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bytes.Equal(f.pty.inputs(), want.Bytes()) })
	t.Logf("outcomes: %v", outcomes)
}

func raceOutcome(t *testing.T, reply protocol.Control) string {
	t.Helper()
	switch {
	case reply.Type == protocol.TypeOK:
		return "admitted"
	case reply.ErrorCode == execution.CodeTerminalAttached:
		return "refused"
	default:
		t.Fatalf("unexpected terminal reply %+v", reply)
		return ""
	}
}

// attachUntilStopped attaches and detaches until stop closes, and returns
// the first failure so the loop never outlives its test.
func attachUntilStopped(w *Worker, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		person, err := attachPerson(w)
		if err != nil {
			return err
		}
		if err := person.detach(); err != nil {
			return err
		}
	}
}

// The worker answers a screen read only for its own instance; a read meant
// for an earlier job under the same session ID gets nothing.
func TestTerminalScreenIsBoundToTheInstance(t *testing.T) {
	f := newTerminalJobWorker(t)
	queryEmit(t, f.pty, []byte("SCREEN-MARK\r\n"))
	waitFor(t, func() bool {
		reply := f.control(t, protocol.Control{Type: protocol.TypeExecutionScreen, PreviewCols: 80, PreviewRows: 24})
		return reply.Type == protocol.TypeOK && reply.Inspection != nil && strings.Contains(strings.Join(reply.Inspection.Preview, "\n"), "SCREEN-MARK")
	})
	other := protocol.Control{Type: protocol.TypeExecutionScreen, RequestID: "screen", SessionID: f.w.cfg.ID, Instance: "0123456789abcdef0123456789abcdef", PreviewCols: 80, PreviewRows: 24}
	reply := f.raw(t, other)
	if reply.ErrorCode != protocol.ErrorCodeInstanceMismatch || reply.Inspection != nil {
		t.Fatalf("screen for another instance = %+v", reply)
	}
}

// An acknowledgment and its durable receipt always agree with what the
// relay took. A relay that closes while the receipt is being recorded keeps
// the reserved write; one that is closed beforehand takes nothing and leaves
// no receipt.
func TestTerminalInputReceiptMatchesRelayAdmission(t *testing.T) {
	f := newTerminalJobWorker(t)
	t.Cleanup(func() { beforeTerminalInputRecord = func(*Worker) {} })
	beforeTerminalInputRecord = func(w *Worker) { w.input.stop() }
	during := f.input(t, "during-close", 0, "a")
	_, recorded := f.output.Input(f.binding, "during-close", nil, nil)
	if (during.Type == protocol.TypeOK) != (recorded == nil) {
		t.Fatalf("reply %+v disagrees with durable receipt (lookup err %v)", during, recorded)
	}
	beforeTerminalInputRecord = func(*Worker) {}
	after := f.input(t, "after-close", 1, "b")
	if after.Type == protocol.TypeOK {
		t.Fatalf("closed relay acknowledged a write: %+v", after)
	}
	if _, err := f.output.Input(f.binding, "after-close", nil, nil); !errors.Is(err, execution.ErrInputUnknown) {
		t.Fatalf("refused write left a durable receipt: %v", err)
	}
}

package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

const pipeNativeFixture = "MESH_PIPE_NATIVE_FIXTURE"

func runNativePipeDaemon(role string, args []string) error {
	if len(args) != 0 && args[0] == "session-worker" {
		return runNativePipeWorker(args[1:])
	}
	return Run(context.Background(), Config{StateDir: filepath.Join(role, "state"), ConfigDir: filepath.Join(role, "config")})
}

func runNativePipeWorker(args []string) error {
	options := flag.NewFlagSet("pipe worker", flag.ContinueOnError)
	cfg, b := worker.Config{}, execution.Binding{Host: os.Getenv(worker.MeshHostIDVariable)}
	options.StringVar(&cfg.ID, "id", "", "")
	options.StringVar(&cfg.Dir, "dir", "", "")
	options.IntVar(&cfg.Cols, "cols", 0, "")
	options.IntVar(&cfg.Rows, "rows", 0, "")
	options.StringVar(&cfg.Label, "label", "", "")
	options.StringVar(&cfg.Cwd, "request-cwd", "", "")
	options.StringVar(&b.Reservation.Instance, "instance", "", "")
	options.StringVar(&b.Reservation.BootID, "boot", "", "")
	options.StringVar(&b.RequestHash, "request-hash", "", "")
	options.StringVar(&b.LaunchDigest, "launch-digest", "", "")
	options.StringVar(&b.Kind, "execution-kind", "", "")
	options.StringVar(&b.Stdin, "execution-stdin", "", "")
	var purpose, imageHint string
	options.StringVar(&purpose, "purpose", "", "")
	options.StringVar(&imageHint, "image-hint", "", "")
	if err := options.Parse(args); err != nil {
		return fmt.Errorf("parse fixture worker: %w", err)
	}
	release.AdoptDigestHint(imageHint)
	b.Reservation.ID = cfg.ID
	b.Reservation.Purpose = execution.PurposeForKind(b.Kind)
	if string(b.Reservation.Purpose) != purpose {
		return fmt.Errorf("fixture worker: unexpected purpose %q", purpose)
	}
	cfg.Reservation, cfg.Execution, cfg.Command = &b.Reservation, &b, options.Args()
	_, err := worker.Run(cfg)
	return executionBoundaryError("run native fixture worker", err)
}

type nativePipeDaemon struct {
	root, state, sessions, executable string
	cmd                               *exec.Cmd
	log                               *os.File
}

func newNativePipeDaemon(t *testing.T) *nativePipeDaemon {
	t.Helper()
	root := compactSocketTempDir(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "s"), 0700); err != nil {
		t.Fatal(err)
	}
	f := &nativePipeDaemon{root: root, state: state, sessions: filepath.Join(state, "s"), executable: bin}
	t.Cleanup(func() {
		f.stop(t)
		f.stopWorkers(t)
	})
	f.start(t)
	return f
}

func (f *nativePipeDaemon) env() []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + f.root, "TMPDIR=" + f.root, "MESH_STATE_DIR=" + f.state, "MESH_CONFIG_DIR=" + filepath.Join(f.root, "config"), pipeNativeFixture + "=" + f.root}
}

func (f *nativePipeDaemon) start(t *testing.T) {
	t.Helper()
	var err error
	f.log, err = os.OpenFile(filepath.Join(f.root, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.cmd = exec.Command(f.executable) //nolint:gosec // run this same test binary as the isolated daemon
	f.cmd.Env = f.env()
	f.cmd.Stdout, f.cmd.Stderr = f.log, f.log
	if err := f.cmd.Start(); err != nil {
		_ = f.log.Close()
		f.cmd = nil
		t.Fatal(err)
	}
	pipeEventually(t, func() bool {
		reply, err := f.exchange(protocol.Control{Type: protocol.TypeHostInfo, RequestID: "ready"})
		return err == nil && reply.Host != nil
	})
}

func (f *nativePipeDaemon) stop(t *testing.T) {
	t.Helper()
	if f.cmd == nil {
		return
	}
	if err := f.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	_ = f.cmd.Wait()
	_ = f.log.Close()
	f.cmd = nil
}

func (f *nativePipeDaemon) stopWorkers(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.sessions)
	if err != nil {
		t.Error(err)
		return
	}
	for _, entry := range entries {
		f.stopWorker(t, entry.Name())
	}
}

func (f *nativePipeDaemon) stopWorker(t *testing.T, id string) {
	t.Helper()
	dir := filepath.Join(f.sessions, id)
	meta, err := worker.ReadMeta(dir)
	if err != nil || meta.Reservation == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	req := protocol.Control{Type: protocol.TypeExecutionKill, RequestID: "cleanup", SessionID: id, Instance: meta.Reservation.Instance}
	_, _ = worker.ExchangeExecution(ctx, f.sessions, req)
	pipeEventually(t, func() bool {
		_, err := os.Lstat(paths.Socket(dir))
		return errors.Is(err, os.ErrNotExist)
	})
}

func (f *nativePipeDaemon) exchange(req protocol.Control) (protocol.Control, error) {
	conn, err := net.DialTimeout("unix", SocketPath(f.state), 100*time.Millisecond)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("dial native pipe daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		return protocol.Control{}, fmt.Errorf("write native pipe control: %w", err)
	}
	return readPipeControl(protocol.NewReader(conn))
}

func readPipeControl(reader *protocol.Reader) (protocol.Control, error) {
	frame, err := reader.ReadFrame()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("read native pipe control: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("native pipe: expected control, got %v", frame.Kind)
	}
	reply, err := protocol.DecodeControl(frame.Payload)
	return reply, executionBoundaryError("decode native pipe control", err)
}

func (f *nativePipeDaemon) request(t *testing.T, req protocol.Control) protocol.Control {
	t.Helper()
	reply, err := f.exchange(req)
	if err != nil || reply.Type == protocol.TypeError {
		t.Fatalf("%s: %+v, %v", req.Type, reply, err)
	}
	return reply
}

func pipeEventually(t *testing.T, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("native pipe fixture did not settle")
		case <-ticker.C:
		}
	}
}

func boundPipeRequest(kind string, job *protocol.ExecutionReceipt) protocol.Control {
	return protocol.Control{Type: kind, RequestID: "pipe-test", SessionID: job.SessionID, Instance: job.Instance}
}

func TestPipeExecutionNative(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "create", RequestKey: "pipe", ExecutionKind: execution.KindPipe, Stdin: execution.StdinOpen, Cwd: f.root, Command: []string{"/bin/sh", "-c", "cat; printf '\\303\\251\\000\\377' >&2; exit 7"}}
	job := f.request(t, req).ExecutionReceipt
	input := boundPipeRequest(protocol.TypeExecutionInput, job)
	offset := uint64(0)
	input.WriteKey, input.InputOffset, input.InputBytes = "bytes", &offset, []byte{0, 0xff, '\r', '\n'}
	first := f.request(t, input).InputReceipt
	f.stop(t)
	f.start(t)
	replay := f.request(t, req).ExecutionReceipt
	if replay.Instance != job.Instance || *f.request(t, input).InputReceipt != *first {
		t.Fatal("restart lost exact launch or accepted stdin")
	}
	end := uint64(len(input.InputBytes))
	eof := input
	eof.WriteKey, eof.InputOffset, eof.InputBytes, eof.InputEOF = "eof", &end, nil, true
	ack := f.request(t, eof).InputReceipt
	wait := boundPipeRequest(protocol.TypeExecutionWait, job)
	wait.TimeoutMillis = 10000
	done := f.request(t, wait)
	if done.ExecutionWait != "completed" || done.ExecutionReceipt.Result.Termination.Code == nil || *done.ExecutionReceipt.Result.Termination.Code != 7 {
		t.Fatalf("incorrect completion: %+v", done)
	}
	if retry := f.request(t, eof).InputReceipt; *retry != *ack || !retry.Closed || retry.Length != 0 {
		t.Fatalf("EOF receipt changed: %+v", retry)
	}
	assertPipeBytes(t, f, job, execution.StreamStdout, input.InputBytes)
	assertPipeBytes(t, f, job, execution.StreamStderr, []byte{0xc3, 0xa9, 0, 0xff})
}

func assertPipeBytes(t *testing.T, f *nativePipeDaemon, job *protocol.ExecutionReceipt, stream string, want []byte) {
	t.Helper()
	log := boundPipeRequest(protocol.TypeExecutionLogs, job)
	log.Stream = stream
	page := f.request(t, log).ExecutionPage
	if page == nil || !page.EOF || !bytes.Equal(page.Bytes, want) {
		t.Fatalf("stream %s = %+v, want %x", stream, page, want)
	}
}

func (f *nativePipeDaemon) launchLineWorker(t *testing.T, argv []string) execution.Binding {
	t.Helper()
	host := f.request(t, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "host"}).Host.ID
	journal, err := execution.Open(f.state, worker.BootID(), "line-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	key := execution.Key{Host: host, Class: execution.InternalServiceClass, Principal: "fixture-revision", Opaque: "generation-1"}
	intent := execution.Intent{Kind: execution.KindLine, Stdin: execution.StdinOpen, Command: argv, Cwd: f.root}
	var env []string
	r, created, err := journal.Reserve(t.Context(), key, intent, func() (execution.Binding, error) {
		ref, err := worker.Reserve(f.sessions, execution.PurposeForKind(intent.Kind))
		if err != nil {
			return execution.Binding{}, fmt.Errorf("reserve fixture line worker: %w", err)
		}
		env = worker.ExecutionEnvironment(f.env(), host, ref, "")
		return execution.NewBinding(key, intent, ref, env), nil
	})
	if err != nil || !created {
		t.Fatalf("reserve line worker: %+v %v", r, err)
	}
	b := r.Binding
	if err := journal.Dispatch(t.Context(), b, intent); err != nil {
		t.Fatal(err)
	}
	cfg := worker.LaunchConfig{SessionsDir: f.sessions, HostID: host, Executable: f.executable, Command: argv, Cwd: f.root, Env: env, Reservation: &b.Reservation, Execution: &b, Label: "agent request " + b.Reservation.Instance}
	if _, err := worker.LaunchDetached(cfg); err != nil {
		var started *worker.StartedError
		if !errors.As(err, &started) {
			t.Fatal(err)
		}
		pipeEventually(t, func() bool { return worker.ProbePipe(t.Context(), f.sessions, b) == nil })
	}
	return b
}

func TestPipeFramedWorkerNativeRecovery(t *testing.T) {
	f := newNativePipeDaemon(t)
	b := f.launchLineWorker(t, []string{"/bin/sh", "-c", `
IFS= read -r first
printf 'late:%s' "$first"
printf x >> partial
IFS= read -r second
printf ':%s\nnext\n' "$second"
IFS= read -r third
printf partial
exec 1>&-
IFS= read -r keep_alive
`})
	attached := openFixtureLines(t, f.sessions, b, 0)
	writeFixtureLine(t, attached, b, 0, "0:first", "one\n")
	// Drop the acknowledgement while the child has written half its reply.
	_ = attached.Close()
	pipeEventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(f.root, "partial"))
		return err == nil && string(data) == "x"
	})
	f.stop(t)
	f.start(t)
	if err := worker.ProbePipe(t.Context(), f.sessions, b); err != nil {
		t.Fatal(err)
	}

	other := b
	other.LaunchDigest = string(bytes.Repeat([]byte{'0'}, 64))
	wrong, err := dialFixtureLines(t.Context(), f.sessions, other, 0)
	if err == nil {
		_ = wrong.Close()
		t.Fatal("a different launch digest attached")
	}
	attached = openFixtureLines(t, f.sessions, b, 0)
	otherConn, err := dialFixtureLines(t.Context(), f.sessions, b, 0)
	if err == nil {
		_ = otherConn.Close()
		t.Fatal("second controller attached concurrently")
	}
	reader := protocol.NewReader(attached)
	checkFixtureLineReceipt(t, attached, reader, b)
	retireFixtureLine(t, attached, reader, b)
	writeFixtureLine(t, attached, b, 4, "4:second", "two\n")
	receiveFixtureLines(t, reader, b, "late:one:two\n", "next\n")
	_ = attached.Close()

	attached = openFixtureLines(t, f.sessions, b, 2)
	reader = protocol.NewReader(attached)
	writeFixtureLine(t, attached, b, 8, "8:third", "three\n")
	assertFixtureLineFault(t, reader, "partial_line")
	assertLineWorkerPrivate(t, f, b)
}

func dialFixtureLines(ctx context.Context, sessions string, b execution.Binding, offset uint64) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(filepath.Join(sessions, b.Reservation.ID)))
	if err != nil {
		return nil, fmt.Errorf("dial fixture line worker: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	req := protocol.Control{Type: protocol.TypeWorkerLineAttach, SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, LaunchDigest: b.LaunchDigest, LastSeq: &offset}
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write line attach: %w", err)
	}
	reply, err := readPipeControl(protocol.NewReader(conn))
	if err != nil || reply.Type != protocol.TypeOK || reply.SessionID != req.SessionID || reply.Instance != req.Instance || reply.LaunchDigest != req.LaunchDigest {
		_ = conn.Close()
		return nil, fmt.Errorf("line attach: %s %w", reply.ErrorCode, err)
	}
	return conn, nil
}

func openFixtureLines(t *testing.T, sessions string, b execution.Binding, offset uint64) net.Conn {
	t.Helper()
	var conn net.Conn
	pipeEventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		var err error
		conn, err = dialFixtureLines(ctx, sessions, b, offset)
		return err == nil
	})
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeFixtureLine(t *testing.T, conn net.Conn, b execution.Binding, offset uint64, key, line string) {
	t.Helper()
	sid, err := protocol.NewSessionID(b.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload := binary.BigEndian.AppendUint64(nil, offset)
	payload = append(payload, byte(len(key))) //nolint:gosec // fixture keys fit the one-byte header
	payload = append(payload, key...)
	payload = append(payload, line...)
	if err := protocol.NewWriter(conn).WriteInput(sid, payload); err != nil {
		t.Fatal(err)
	}
}

func checkFixtureLineReceipt(t *testing.T, conn net.Conn, reader *protocol.Reader, b execution.Binding) {
	t.Helper()
	req := protocol.Control{Type: protocol.TypeWorkerLineInput, RequestID: "recover-first", SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, WriteKey: "0:first"}
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	reply, err := readPipeControl(reader)
	if err != nil || reply.InputReceipt == nil || reply.InputReceipt.WriteKey != "0:first" || reply.InputReceipt.Offset != 0 || reply.InputReceipt.Length != 4 {
		t.Fatalf("lost first line receipt: %+v %v", reply, err)
	}
}

func receiveFixtureLines(t *testing.T, reader *protocol.Reader, b execution.Binding, want ...string) {
	t.Helper()
	index, acknowledged := 0, false
	for index < len(want) || !acknowledged {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Kind == protocol.KindControl {
			reply, err := protocol.DecodeControl(frame.Payload)
			if err != nil || reply.InputReceipt == nil || reply.InputReceipt.WriteKey != "4:second" {
				t.Fatalf("second line receipt: %+v %v", reply, err)
			}
			acknowledged = true
			continue
		}
		if index == len(want) || frame.Kind != protocol.KindData || frame.Seq != uint64(index) || frame.Session.String() != b.Reservation.ID || string(frame.Payload) != want[index] {
			t.Fatalf("line %d: %+v", index, frame)
		}
		index++
	}
}

func assertFixtureLineFault(t *testing.T, reader *protocol.Reader, fault string) {
	t.Helper()
	for range 2 {
		reply, err := readPipeControl(reader)
		if err != nil {
			t.Fatal(err)
		}
		if reply.ErrorCode == fault {
			return
		}
		if reply.InputReceipt == nil || reply.InputReceipt.WriteKey != "8:third" {
			t.Fatalf("unexpected framing reply: %+v", reply)
		}
	}
	t.Fatalf("worker did not report %s", fault)
}

func assertLineWorkerPrivate(t *testing.T, f *nativePipeDaemon, b execution.Binding) {
	t.Helper()
	req := protocol.Control{Type: protocol.TypeExecutionLogs, RequestID: "forbidden", SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, Stream: execution.StreamStderr}
	reply, err := f.exchange(req)
	if err != nil || reply.ErrorCode != "session.instance_mismatch" || reply.ExecutionPage != nil {
		t.Fatalf("line worker leaked through CLI logs: %+v %v", reply, err)
	}
	list := f.request(t, protocol.Control{Type: protocol.TypeList, RequestID: "list"})
	for _, row := range list.Sessions {
		if row.ID == b.Reservation.ID {
			t.Fatal("line worker entered the terminal catalog")
		}
	}
}

func retireFixtureLine(t *testing.T, conn net.Conn, reader *protocol.Reader, b execution.Binding) {
	t.Helper()
	req := protocol.Control{Type: protocol.TypeWorkerLineRetire, RequestID: "retire-first", SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, WriteKey: "0:first"}
	writer := protocol.NewWriter(conn)
	if err := writer.WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	reply, err := readPipeControl(reader)
	if err != nil || reply.Type != protocol.TypeOK {
		t.Fatalf("retire line receipt: %+v %v", reply, err)
	}
	req.Type, req.RequestID = protocol.TypeWorkerLineInput, "lookup-retired"
	if err := writer.WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	reply, err = readPipeControl(reader)
	if err != nil || reply.InputReceipt != nil || reply.ErrorCode != "input.outcome_unknown" {
		t.Fatalf("retired line receipt: %+v %v", reply, err)
	}
}

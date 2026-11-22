package mcphost

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

// LineConn is the daemon's exclusive framed duplex attach to one stdio
// worker. The worker owns the pipes and keeps draining them while no
// controller is attached.
type LineConn interface {
	// WriteLine admits one complete newline-terminated line at the next stdin
	// offset and returns once the worker acknowledged it. Errors wrap
	// ErrNotSent when the worker refused the line without writing it.
	WriteLine(ctx context.Context, line []byte) error
	// ReadLine returns the next complete stdout line with its delimiter
	// removed. It returns an error once stdout ended or framing was lost.
	ReadLine() ([]byte, error)
	Close() error
}

// WorkerRef names the exact worker reservation a controller may attach to.
type WorkerRef struct {
	SessionsDir  string `json:"-"`
	SessionID    string `json:"sessionId"`
	Instance     string `json:"instance"`
	LaunchDigest string `json:"launchDigest"`
}

const lineAckTimeout = 30 * time.Second

// ErrLineFault reports a worker that ended its stdout stream, with the
// worker's classified reason.
var ErrLineFault = errors.New("mcp host: worker line stream ended")

type workerLines struct {
	conn   net.Conn
	writer *protocol.Writer
	sid    protocol.SessionID
	ref    WorkerRef

	write   sync.Mutex
	offset  uint64
	counter uint64

	lines chan []byte
	acks  chan protocol.Control
	done  chan struct{}
	once  sync.Once
	err   error
}

// DialWorkerLines attaches to the worker at the current end of its output,
// so an adopting daemon never reprocesses replies sent to an earlier run.
func DialWorkerLines(ctx context.Context, ref WorkerRef) (LineConn, error) {
	sid, err := protocol.NewSessionID(ref.SessionID)
	if err != nil {
		return nil, fmt.Errorf("worker %s: %w", ref.SessionID, err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(filepath.Join(ref.SessionsDir, ref.SessionID)))
	if err != nil {
		return nil, fmt.Errorf("dial worker %s: %w", ref.SessionID, err)
	}
	l := &workerLines{conn: conn, writer: protocol.NewWriter(conn), sid: sid, ref: ref, lines: make(chan []byte, 64), acks: make(chan protocol.Control, 1), done: make(chan struct{})}
	if err := l.attach(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go l.readLoop(protocol.NewReader(conn))
	return l, nil
}

func (l *workerLines) attach(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = l.conn.SetDeadline(time.Now()) })
	defer stop()
	tail := protocol.LineTail
	req := protocol.Control{Type: protocol.TypeWorkerLineAttach, SessionID: l.ref.SessionID, Instance: l.ref.Instance, LaunchDigest: l.ref.LaunchDigest, LastSeq: &tail}
	if err := l.writer.WriteControlMsg(req); err != nil {
		return fmt.Errorf("attach worker %s: %w", l.ref.SessionID, err)
	}
	frame, err := protocol.NewReader(l.conn).ReadFrame()
	if err != nil {
		return fmt.Errorf("attach worker %s: %w", l.ref.SessionID, err)
	}
	reply, err := protocol.DecodeControl(frame.Payload)
	if err != nil || reply.Type != protocol.TypeOK || reply.Instance != l.ref.Instance || reply.LaunchDigest != l.ref.LaunchDigest {
		return fmt.Errorf("attach worker %s: refused %q: %w", l.ref.SessionID, reply.ErrorCode, ErrNotSent)
	}
	if reply.InputOffset == nil {
		return fmt.Errorf("attach worker %s: no stdin offset in reply", l.ref.SessionID)
	}
	l.offset = *reply.InputOffset
	_ = l.conn.SetDeadline(time.Time{})
	return nil
}

func (l *workerLines) readLoop(reader *protocol.Reader) {
	defer close(l.lines)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			l.fail(fmt.Errorf("read worker %s: %w", l.ref.SessionID, err))
			return
		}
		if !l.handleFrame(frame) {
			return
		}
	}
}

func (l *workerLines) handleFrame(frame protocol.Frame) bool {
	if frame.Kind == protocol.KindData && frame.Session == l.sid {
		select {
		case l.lines <- frame.Payload:
			return true
		case <-l.done:
			return false
		}
	}
	if frame.Kind != protocol.KindControl {
		l.fail(fmt.Errorf("worker %s sent an unexpected frame: %w", l.ref.SessionID, ErrLineFault))
		return false
	}
	reply, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		l.fail(fmt.Errorf("worker %s: %w", l.ref.SessionID, err))
		return false
	}
	if reply.Type == protocol.TypeExit || (reply.Type == protocol.TypeError && reply.RequestID == "") {
		l.fail(fmt.Errorf("worker %s: %s: %w", l.ref.SessionID, lineReason(reply), ErrLineFault))
		return false
	}
	select {
	case l.acks <- reply:
		return true
	case <-l.done:
		return false
	}
}

func lineReason(reply protocol.Control) string {
	if reply.ErrorCode == "" {
		return "stdout closed"
	}
	return reply.ErrorCode
}

func (l *workerLines) fail(err error) {
	l.once.Do(func() {
		l.err = err
		close(l.done)
		_ = l.conn.Close()
	})
}

func (l *workerLines) ReadLine() ([]byte, error) {
	line, ok := <-l.lines
	if !ok {
		return nil, l.err
	}
	return line, nil
}

func (l *workerLines) WriteLine(ctx context.Context, line []byte) error {
	l.write.Lock()
	defer l.write.Unlock()
	l.counter++
	key := strconv.FormatUint(l.offset, 10) + ":w" + strconv.FormatUint(l.counter, 36)
	payload := binary.BigEndian.AppendUint64(make([]byte, 0, 9+len(key)+len(line)), l.offset)
	payload = append(payload, byte(len(key))) //nolint:gosec // A decimal offset, ":w" and a base-36 counter stay far below 255 bytes.
	payload = append(payload, key...)
	payload = append(payload, line...)
	_ = l.conn.SetWriteDeadline(time.Now().Add(lineAckTimeout))
	if err := l.writer.WriteInput(l.sid, payload); err != nil {
		l.fail(fmt.Errorf("write worker %s: %w", l.ref.SessionID, err))
		return fmt.Errorf("write worker %s: %w", l.ref.SessionID, ErrOutcomeUnknown)
	}
	reply, err := l.await(ctx, key)
	if err != nil {
		return err
	}
	if reply.Type != protocol.TypeOK || reply.InputReceipt == nil {
		return l.refused(reply)
	}
	l.offset += uint64(len(line))
	return l.retire(ctx, key)
}

func (l *workerLines) refused(reply protocol.Control) error {
	if reply.ErrorCode == "input.backpressure" || reply.ErrorCode == "resource.limit" {
		return fmt.Errorf("worker %s refused input (%s): %w", l.ref.SessionID, reply.ErrorCode, ErrNotSent)
	}
	return fmt.Errorf("worker %s input %s: %w", l.ref.SessionID, reply.ErrorCode, ErrOutcomeUnknown)
}

// retire drops the worker's acknowledgment record once it is known here, so
// a long-lived instance does not exhaust the worker's receipt table.
func (l *workerLines) retire(ctx context.Context, key string) error {
	req := protocol.Control{Type: protocol.TypeWorkerLineRetire, RequestID: "retire " + key, SessionID: l.ref.SessionID, Instance: l.ref.Instance, WriteKey: key}
	if err := l.writer.WriteControlMsg(req); err != nil {
		l.fail(fmt.Errorf("retire worker %s input: %w", l.ref.SessionID, err))
		return nil
	}
	_, err := l.await(ctx, req.RequestID)
	if errors.Is(err, ErrOutcomeUnknown) {
		// The line itself was acknowledged; only the cleanup is uncertain.
		return nil
	}
	return err
}

func (l *workerLines) await(ctx context.Context, requestID string) (protocol.Control, error) {
	timer := time.NewTimer(lineAckTimeout)
	defer timer.Stop()
	for {
		select {
		case reply := <-l.acks:
			if reply.RequestID == requestID {
				return reply, nil
			}
		case <-l.done:
			return protocol.Control{}, fmt.Errorf("worker %s: %w: %w", l.ref.SessionID, l.err, ErrOutcomeUnknown)
		case <-timer.C:
			l.fail(fmt.Errorf("worker %s did not acknowledge input: %w", l.ref.SessionID, io.ErrNoProgress))
		case <-ctx.Done():
			// The line may already be in the worker's buffer; the stream can no
			// longer be trusted to stay aligned with this controller.
			l.fail(fmt.Errorf("worker %s: %w", l.ref.SessionID, ctx.Err()))
		}
	}
}

func (l *workerLines) Close() error {
	l.fail(fmt.Errorf("worker %s: attachment closed: %w", l.ref.SessionID, net.ErrClosed))
	return nil
}

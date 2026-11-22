package worker

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// An adopting controller attaches at the tail and learns the stdin offset.
// Acknowledgments sent while stdout advances must not share the attachment's
// moving frame counter.
func TestLineTailAttachReportsPositionAndAcksStayIndependent(t *testing.T) {
	p, reads, b, _ := newReviewPipeInput(t, execution.KindLine, 4096)
	// Drain the child's stdin so admitted lines never back up.
	go func() { _, _ = io.Copy(io.Discard, reads) }()
	sid, err := protocol.NewSessionID(b.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	// A hang cap only; under -race on a busy host the run can be slow.
	_ = client.SetDeadline(time.Now().Add(2 * time.Minute))
	defer func() { _ = client.Close() }()
	w := &Worker{cfg: Config{ID: b.Reservation.ID, Execution: &b}, sid: sid, pipe: &pipeExecution{input: p, lines: newLineOutput()}}
	for range 3 {
		w.pipe.lines.offer([]byte("before attach\n"))
	}
	done := make(chan struct{})
	go func() { defer close(done); w.serve(server) }()
	t.Cleanup(func() { _ = client.Close(); awaitPipeChannel(t, done) })

	writer, reader := protocol.NewWriter(client), protocol.NewReader(client)
	tail := protocol.LineTail
	req := protocol.Control{Type: protocol.TypeWorkerLineAttach, SessionID: w.cfg.ID, Instance: b.Reservation.Instance, LaunchDigest: b.LaunchDigest, LastSeq: &tail}
	if err := writer.WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	attached := readLineControl(t, reader)
	if attached.Type != protocol.TypeOK || attached.LastSeq == nil || *attached.LastSeq != 3 || attached.InputOffset == nil || *attached.InputOffset != 0 {
		t.Fatalf("tail attach reply: %+v", attached)
	}

	controls := make(chan protocol.Control, 64)
	go func() {
		defer close(controls)
		for {
			f, err := reader.ReadFrame()
			if err != nil {
				return
			}
			if f.Kind != protocol.KindControl {
				continue
			}
			reply, err := protocol.DecodeControl(f.Payload)
			if err == nil {
				controls <- reply
			}
		}
	}()
	offset, repeated := uint64(0), 0
	for i := range 32 {
		line := []byte("input " + strconv.Itoa(i) + "\n")
		key := strconv.FormatUint(offset, 10) + ":k" + strconv.Itoa(i)
		payload := binary.BigEndian.AppendUint64(nil, offset)
		payload = append(payload, byte(len(key))) //nolint:gosec // short fixture key
		payload = append(payload, key...)
		payload = append(payload, line...)
		// Advance stdout while this input's acknowledgment is encoded.
		go func() {
			for range 4 {
				w.pipe.lines.offer([]byte("output\n"))
			}
		}()
		if err := writer.WriteInput(sid, payload); err != nil {
			t.Fatal(err)
		}
		ack, open := <-controls
		if !open {
			t.Fatalf("the attachment ended before acknowledgment %d", i)
		}
		if ack.InputReceipt == nil || ack.InputReceipt.WriteKey != key {
			t.Fatalf("acknowledgment %d: %+v", i, ack)
		}
		if ack.LastSeq != nil || ack.InputOffset != nil {
			repeated++
		}
		offset += uint64(len(line))
	}
	if repeated != 0 {
		t.Fatalf("%d acknowledgments repeated the attach position", repeated)
	}
}

func readLineControl(t *testing.T, reader *protocol.Reader) protocol.Control {
	t.Helper()
	f, err := reader.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.DecodeControl(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

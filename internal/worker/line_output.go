package worker

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/shaul/mesh/internal/mcpwire"
)

const lineOutputBytes = 8 << 20
const lineOutputFrames = 4096
const lineFrameGap = "frame_gap"

// maxFramedLine allows mcpwire.MaxMessage JSON bytes plus a CRLF delimiter.
const maxFramedLine = mcpwire.MaxMessage + 2

type lineFrame struct {
	data       []byte
	oversizeID []byte
}

func (f lineFrame) present() bool { return len(f.data) > 0 || len(f.oversizeID) > 0 }
func (f lineFrame) size() int     { return len(f.data) + len(f.oversizeID) }

type lineOutput struct {
	mu       sync.Mutex
	frames   [lineOutputFrames]lineFrame
	earliest uint64
	next     uint64
	bytes    int
	changed  chan struct{}
	closed   bool
	fault    string
	attached chan struct{}
}

func newLineOutput() *lineOutput {
	return &lineOutput{changed: make(chan struct{})}
}

func (o *lineOutput) readFrom(file *os.File) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.readLines(file)
	}()
	return done
}

func (o *lineOutput) readLines(file *os.File) {
	reader := bufio.NewReaderSize(file, 32<<10)
	for {
		frame, err := readLineFrame(reader)
		if err == nil {
			o.offerFrame(frame)
			continue
		}
		o.finish(lineFault(frame.data, err))
		if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
			// Once framing is lost, continue draining the child without keeping bytes.
			_, _ = io.Copy(io.Discard, reader)
		}
		return
	}
}

var errLineTooLarge = errors.New("line exceeds worker frame limit")

func readLineFrame(reader *bufio.Reader) (lineFrame, error) {
	line, err := reader.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return finishLineFrame(reader, bytes.Clone(line), err)
	}
	total := bytes.Clone(line)
	for {
		line, err = reader.ReadSlice('\n')
		if len(line) > maxFramedLine-len(total) {
			return recoverOversizedLine(reader, total, line, err)
		}
		total = append(total, line...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return finishLineFrame(reader, total, err)
		}
	}
}

func finishLineFrame(reader *bufio.Reader, total []byte, err error) (lineFrame, error) {
	if err != nil {
		return lineFrame{data: total}, workerExecutionError("read line", err)
	}
	body := bytes.TrimSuffix(total[:len(total)-1], []byte("\r"))
	if len(body) > mcpwire.MaxMessage {
		return recoverOversizedLine(reader, total, nil, nil)
	}
	return lineFrame{data: total}, nil
}

func recoverOversizedLine(reader *bufio.Reader, total, tail []byte, readErr error) (lineFrame, error) {
	if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) {
		return lineFrame{}, errLineTooLarge
	}
	total = append(total, tail...)
	id, err := mcpwire.DrainOversizedReply(reader, total, readErr == nil)
	if err != nil {
		return lineFrame{}, errLineTooLarge
	}
	rawID, err := id.MarshalJSON()
	if err != nil {
		return lineFrame{}, errLineTooLarge
	}
	return lineFrame{oversizeID: rawID}, nil
}

func lineFault(partial []byte, err error) string {
	switch {
	case errors.Is(err, errLineTooLarge):
		return "line_too_large"
	case errors.Is(err, os.ErrClosed):
		return "drain_timeout"
	case !errors.Is(err, io.EOF):
		return "read_error"
	case len(partial) != 0:
		return "partial_line"
	default:
		return ""
	}
}

func (o *lineOutput) notify() {
	close(o.changed)
	o.changed = make(chan struct{})
}

func (o *lineOutput) offerFrame(frame lineFrame) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	size := frame.size()
	for o.next-o.earliest == lineOutputFrames || o.bytes+size > lineOutputBytes {
		index := o.earliest % lineOutputFrames
		o.bytes -= o.frames[index].size()
		o.frames[index] = lineFrame{}
		o.earliest++
	}
	o.frames[o.next%lineOutputFrames] = frame
	o.next++
	o.bytes += size
	o.notify()
}

func (o *lineOutput) finish(fault string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed, o.fault = true, fault
		o.notify()
	}
}

func (o *lineOutput) positionError(seq uint64) string {
	if seq < o.earliest {
		return lineFrameGap
	}
	if seq > o.next {
		return "future_frame"
	}
	return ""
}

func (o *lineOutput) attach(seq uint64) (func(), string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.attached != nil {
		return nil, "already_attached"
	}
	if fault := o.positionError(seq); fault != "" {
		return nil, fault
	}
	o.attached = make(chan struct{})
	return o.detach, ""
}

// end is the sequence number the next stdout frame will receive.
func (o *lineOutput) end() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.next
}

func (o *lineOutput) detach() {
	o.mu.Lock()
	defer o.mu.Unlock()
	close(o.attached)
	o.attached = nil
}

func (o *lineOutput) attachmentDone() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.attached
}

func (o *lineOutput) inputFault(next uint64) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if fault := o.positionError(next); fault != "" {
		return fault
	}
	if !o.closed {
		return ""
	}
	if o.fault == "" {
		return "stdout_closed"
	}
	return o.fault
}

func (o *lineOutput) readLineFrame(seq uint64) (lineFrame, string, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if fault := o.positionError(seq); fault != "" {
		return lineFrame{}, fault, nil
	}
	if seq < o.next {
		return o.frames[seq%lineOutputFrames], "", nil
	}
	if o.closed {
		return lineFrame{}, o.fault, nil
	}
	return lineFrame{}, "", o.changed
}

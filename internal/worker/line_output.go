package worker

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/shaul/mesh/internal/session"
)

const lineOutputBytes = 8 << 20
const lineOutputFrames = 4096
const lineFrameGap = "frame_gap"

type lineOutput struct {
	mu       sync.Mutex
	frames   [lineOutputFrames][]byte
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
		line, err := readLine(reader)
		if err == nil {
			o.offer(line)
			continue
		}
		o.finish(lineFault(line, err))
		if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
			// Once framing is lost, continue draining the child without keeping bytes.
			_, _ = io.Copy(io.Discard, reader)
		}
		return
	}
}

var errLineTooLarge = errors.New("line exceeds worker frame limit")

func readLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return bytes.Clone(line), workerExecutionError("read line", err)
	}
	total := bytes.Clone(line)
	for {
		line, err = reader.ReadSlice('\n')
		if len(line) > session.MaxLineBytes-len(total) {
			return nil, errLineTooLarge
		}
		total = append(total, line...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return total, workerExecutionError("read line", err)
		}
	}
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

func (o *lineOutput) offer(line []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	for o.next-o.earliest == lineOutputFrames || o.bytes+len(line) > lineOutputBytes {
		index := o.earliest % lineOutputFrames
		o.bytes -= len(o.frames[index])
		o.frames[index] = nil
		o.earliest++
	}
	o.frames[o.next%lineOutputFrames] = line
	o.next++
	o.bytes += len(line)
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

func (o *lineOutput) readFrame(seq uint64) ([]byte, string, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if fault := o.positionError(seq); fault != "" {
		return nil, fault, nil
	}
	if seq < o.next {
		return o.frames[seq%lineOutputFrames], "", nil
	}
	if o.closed {
		return nil, o.fault, nil
	}
	return nil, "", o.changed
}

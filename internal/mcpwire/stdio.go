package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type StdioReader struct {
	r *bufio.Reader
}

func NewStdioReader(r io.Reader) *StdioReader { return &StdioReader{r: bufio.NewReader(r)} }

// Read returns ErrTooLarge after draining an oversized client line.
func (s *StdioReader) Read() ([]byte, error) { return s.read(false) }

// ReadReply recovers a reply ID only when the entire envelope and line boundary
// can be verified within the drain limits. Callers must match it to an active
// request before reporting a request-level error. ErrFramingLost faults the pipe.
func (s *StdioReader) ReadReply() ([]byte, error) { return s.read(true) }

func (s *StdioReader) read(reply bool) ([]byte, error) {
	line, err := s.r.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		body, err := finishStdioLine(line, err)
		return bytes.Clone(body), err
	}
	body := append([]byte(nil), line...)
	for errors.Is(err, bufio.ErrBufferFull) {
		line, err = s.r.ReadSlice('\n')
		body = append(body, line...)
		if len(body) > MaxMessage+2 {
			return nil, s.rejectOversize(body, err, reply)
		}
	}
	raw, err := finishStdioLine(body, err)
	if errors.Is(err, ErrTooLarge) {
		return nil, s.rejectOversize(body, nil, reply)
	}
	return raw, err
}

func finishStdioLine(line []byte, err error) ([]byte, error) {
	if err != nil {
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: stdio line ended without a delimiter: %w", ErrFramingLost, err)
	}
	line = bytes.TrimSuffix(line[:len(line)-1], []byte("\r"))
	if len(line) > MaxMessage {
		return nil, ErrTooLarge
	}
	return line, nil
}

// WriteStdio emits a compact line. The caller serializes writes on its channel.
func WriteStdio(w io.Writer, raw []byte) error {
	if _, fault := Parse(raw); fault != nil {
		return fault
	}
	var line bytes.Buffer
	if err := json.Compact(&line, raw); err != nil {
		return fmt.Errorf("compact MCP stdio message: %w", err)
	}
	line.WriteByte('\n')
	n, err := w.Write(line.Bytes())
	if err != nil {
		return fmt.Errorf("write MCP stdio message: %w", err)
	}
	if n != line.Len() {
		return io.ErrShortWrite
	}
	return nil
}

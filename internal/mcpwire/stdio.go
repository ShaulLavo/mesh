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

// Read returns ErrTooLarge only after discarding the complete oversized line.
// ErrFramingLost means the caller must close the channel.
func (s *StdioReader) Read() ([]byte, error) {
	line, err := s.r.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		body, err := finishStdioLine(line, err)
		return bytes.Clone(body), err
	}

	body := append([]byte(nil), line...)
	size := len(line)
	for errors.Is(err, bufio.ErrBufferFull) {
		line, err = s.r.ReadSlice('\n')
		size += len(line)
		if size > MaxMessage+MaxStdioDrain {
			return nil, fmt.Errorf("%w: oversized stdio line did not end within the drain limit", ErrFramingLost)
		}
		if size <= MaxMessage+2 {
			body = append(body, line...)
		}
	}
	if err != nil {
		return finishStdioLine(line, err)
	}
	if len(body) != size {
		return nil, ErrTooLarge
	}
	return finishStdioLine(body, nil)
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

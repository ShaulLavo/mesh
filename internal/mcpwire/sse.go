package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

type SSEReader struct {
	scanner *bufio.Scanner
	first   bool
}

func NewSSEReader(r io.Reader) *SSEReader {
	scanner := bufio.NewScanner(r)
	scanner.Split(scanSSELine)
	scanner.Buffer(make([]byte, 4096), MaxMessage+16)
	return &SSEReader{scanner: scanner, first: true}
}

// Read returns the next MCP event, ignoring comments and empty SSE frames.
func (s *SSEReader) Read() ([]byte, error) {
	var data []byte
	var name string
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if s.first {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			s.first = false
		}
		if len(line) == 0 && data != nil {
			return finishSSEEvent(data, name)
		}
		var err error
		data, name, err = readSSEField(line, data, name)
		if err != nil {
			return nil, err
		}
	}
	if err := s.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, ErrTooLarge
		}
		return nil, fmt.Errorf("read MCP SSE line: %w", err)
	}
	if data != nil {
		return nil, io.ErrUnexpectedEOF
	}
	return nil, io.EOF
}

func readSSEField(line, data []byte, name string) ([]byte, string, error) {
	if !utf8.Valid(line) {
		return nil, "", peerError("SSE line is not UTF-8")
	}
	if len(line) == 0 {
		return nil, "", nil
	}
	field, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(field) {
	case "event":
		name = string(value)
	case "id", "retry":
		return nil, "", peerError("SSE resumption field is outside the Mesh profile")
	case "data":
		if data != nil {
			data = append(data, '\n')
		} else {
			data = make([]byte, 0, len(value))
		}
		if len(data)+len(value) > MaxMessage {
			return nil, "", ErrTooLarge
		}
		data = append(data, value...)
	}
	return data, name, nil
}

func finishSSEEvent(data []byte, name string) ([]byte, error) {
	if name != "" && name != "message" {
		return nil, peerError("expected an MCP SSE message event")
	}
	return data, nil
}

func scanSSELine(data []byte, atEOF bool) (int, []byte, error) {
	i := bytes.IndexAny(data, "\r\n")
	if i < 0 {
		if atEOF && len(data) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	}
	if data[i] == '\n' {
		return i + 1, data[:i], nil
	}
	if i+1 == len(data) && !atEOF {
		return 0, nil, nil
	}
	if i+1 < len(data) && data[i+1] == '\n' {
		return i + 2, data[:i], nil
	}
	return i + 1, data[:i], nil
}

func WriteSSE(w io.Writer, raw []byte) error {
	if _, fault := Parse(raw); fault != nil {
		return fault
	}
	var event bytes.Buffer
	event.WriteString("event: message\ndata: ")
	if err := json.Compact(&event, raw); err != nil {
		return fmt.Errorf("compact MCP sse message: %w", err)
	}
	event.WriteString("\n\n")
	n, err := w.Write(event.Bytes())
	if err != nil {
		return fmt.Errorf("write MCP sse message: %w", err)
	}
	if n != event.Len() {
		return io.ErrShortWrite
	}
	return nil
}

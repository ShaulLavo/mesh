package transport

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/shaul/mesh/internal/protocol"
)

type streamConn struct {
	stream    io.ReadWriteCloser
	reader    *protocol.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// NewStreamConn adapts a Unix socket or another byte stream to Conn.
func NewStreamConn(stream io.ReadWriteCloser) (Conn, error) {
	if stream == nil {
		return nil, fmt.Errorf("transport: nil protocol stream")
	}
	return &streamConn{stream: stream, reader: protocol.NewReader(stream)}, nil
}

func (c *streamConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.reader.ReadFrame()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("transport: read stream frame: %w", err)
	}
	return frame, nil
}

func (c *streamConn) WriteFrame(frame protocol.Frame) error {
	payload, err := encodeFrame(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := io.Copy(c.stream, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("transport: write stream frame: %w", err)
	}
	return nil
}

func (c *streamConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.stream.Close()
	})
	return c.closeErr
}

var _ Conn = (*streamConn)(nil)

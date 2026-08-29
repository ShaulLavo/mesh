// Package protocol defines Mesh's transport-agnostic wire format.
//
// The same framing is used over the local Unix socket (daemon/CLI to worker)
// and, later, over WebSocket binary frames between hosts. Keeping one format
// means the daemon is a relay rather than a translator.
//
// Every frame is:
//
//	kind:1 | length:4 (big endian) | payload:length
//
// Data and input payloads carry their own session header so that many sessions
// can share one connection:
//
//	data:  session:8 | seq:8 | pty bytes
//	input: session:8 | key bytes
//
// Control payloads are JSON and name their session in the message body.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Kind identifies what a frame carries.
type Kind byte

const (
	KindControl Kind = 0x01 // JSON control message
	KindData    Kind = 0x02 // PTY output, worker to client
	KindInput   Kind = 0x03 // PTY input, client to worker
)

const (
	headerLen  = 5
	sessionLen = 8
	seqLen     = 8

	// MaxPayload bounds a single frame so a hostile or confused peer cannot
	// make us allocate without limit.
	MaxPayload = 4 << 20
)

// ErrPayloadTooLarge is returned when a frame exceeds MaxPayload.
var ErrPayloadTooLarge = errors.New("protocol: payload too large")

// SessionID is a fixed-width session identifier, zero padded on the wire.
type SessionID [sessionLen]byte

// NewSessionID converts s to a SessionID. Names longer than 8 bytes are
// rejected rather than silently truncated into a different session.
func NewSessionID(s string) (SessionID, error) {
	var id SessionID
	if len(s) == 0 {
		return id, errors.New("protocol: empty session id")
	}
	if len(s) > sessionLen {
		return id, fmt.Errorf("protocol: session id %q longer than %d bytes", s, sessionLen)
	}
	copy(id[:], s)
	return id, nil
}

// String returns the session ID without its zero padding.
func (id SessionID) String() string {
	for i, b := range id {
		if b == 0 {
			return string(id[:i])
		}
	}
	return string(id[:])
}

// Frame is one decoded protocol frame.
type Frame struct {
	Kind    Kind
	Session SessionID
	Seq     uint64 // meaningful for KindData only
	Payload []byte // control JSON, or PTY bytes for data/input
}

// Writer serializes frames onto a stream.
type Writer struct {
	w   io.Writer
	buf []byte
}

// NewWriter returns a Writer that emits frames to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteControl sends a JSON control payload.
func (fw *Writer) WriteControl(payload []byte) error {
	return fw.write(KindControl, payload)
}

// WriteData sends PTY output for a session. seq is the byte offset of the
// first byte of b within the session's output stream.
func (fw *Writer) WriteData(id SessionID, seq uint64, b []byte) error {
	hdr := make([]byte, sessionLen+seqLen)
	copy(hdr, id[:])
	binary.BigEndian.PutUint64(hdr[sessionLen:], seq)
	return fw.write(KindData, hdr, b)
}

// WriteInput sends keystrokes for a session.
func (fw *Writer) WriteInput(id SessionID, b []byte) error {
	return fw.write(KindInput, id[:], b)
}

func (fw *Writer) write(kind Kind, parts ...[]byte) error {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	if n > MaxPayload {
		return ErrPayloadTooLarge
	}
	fw.buf = append(fw.buf[:0], byte(kind), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(fw.buf[1:headerLen], uint32(n))
	for _, p := range parts {
		fw.buf = append(fw.buf, p...)
	}
	// One Write call so concurrent writers cannot interleave a frame; callers
	// still serialize access to a Writer.
	_, err := fw.w.Write(fw.buf)
	return err
}

// Reader deserializes frames from a stream.
type Reader struct {
	r      io.Reader
	header [headerLen]byte
}

// NewReader returns a Reader that decodes frames from r.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// ReadFrame returns the next frame. The returned Payload is only valid until
// the next call to ReadFrame.
func (fr *Reader) ReadFrame() (Frame, error) {
	var f Frame
	if _, err := io.ReadFull(fr.r, fr.header[:]); err != nil {
		return f, err
	}
	f.Kind = Kind(fr.header[0])
	n := binary.BigEndian.Uint32(fr.header[1:])
	if n > MaxPayload {
		return f, ErrPayloadTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(fr.r, body); err != nil {
		return f, err
	}

	switch f.Kind {
	case KindControl:
		f.Payload = body
	case KindData:
		if len(body) < sessionLen+seqLen {
			return f, errors.New("protocol: short data frame")
		}
		copy(f.Session[:], body[:sessionLen])
		f.Seq = binary.BigEndian.Uint64(body[sessionLen : sessionLen+seqLen])
		f.Payload = body[sessionLen+seqLen:]
	case KindInput:
		if len(body) < sessionLen {
			return f, errors.New("protocol: short input frame")
		}
		copy(f.Session[:], body[:sessionLen])
		f.Payload = body[sessionLen:]
	default:
		return f, fmt.Errorf("protocol: unknown frame kind 0x%02x", byte(f.Kind))
	}
	return f, nil
}

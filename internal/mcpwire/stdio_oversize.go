package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
)

// OversizedStdioReply identifies an oversized response on a recovered line.
type OversizedStdioReply struct {
	ID ID
}

func (*OversizedStdioReply) Error() string { return ErrTooLarge.Error() }
func (*OversizedStdioReply) Unwrap() error { return ErrTooLarge }

// OversizedStdioRequest identifies an oversized client request whose ID and
// envelope were verified within the stdio drain budget.
type OversizedStdioRequest struct {
	ID ID
}

func (*OversizedStdioRequest) Error() string { return ErrTooLarge.Error() }
func (*OversizedStdioRequest) Unwrap() error { return ErrTooLarge }

// DrainOversizedReply drains an oversized stdio reply line through its newline
// delimiter within MaxStdioDrain and returns its verified JSON-RPC response ID
// when both framing and envelope attribution remain intact.
func DrainOversizedReply(r *bufio.Reader, prefix []byte, delimited bool) (ID, error) {
	tail := &stdioDrain{
		reader:    r,
		remaining: MaxMessage + MaxStdioDrain - len(prefix),
		delimited: delimited,
	}
	id := oversizedReplyID(io.MultiReader(bytes.NewReader(prefix), tail))
	if _, err := io.Copy(io.Discard, tail); err != nil {
		return ID{}, fmt.Errorf("%w: drain oversized stdio line: %w", ErrFramingLost, err)
	}
	if !id.Valid() {
		return ID{}, fmt.Errorf("%w: oversized stdio reply has no verified response ID: %w", ErrFramingLost, ErrTooLarge)
	}
	return id, nil
}

func (s *StdioReader) rejectOversize(prefix []byte, readErr error, reply bool) error {
	if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) {
		return fmt.Errorf("%w: drain oversized stdio line: %w", ErrFramingLost, readErr)
	}
	if reply {
		id, err := DrainOversizedReply(s.r, prefix, readErr == nil)
		if err != nil {
			return err
		}
		return &OversizedStdioReply{ID: id}
	}
	tail := &stdioDrain{
		reader:    s.r,
		remaining: MaxMessage + MaxStdioDrain - len(prefix),
		delimited: readErr == nil,
	}
	id := oversizedRequestID(io.MultiReader(bytes.NewReader(prefix), tail))
	if _, err := io.Copy(io.Discard, tail); err != nil {
		return fmt.Errorf("%w: drain oversized stdio line: %w", ErrFramingLost, err)
	}
	if id.Valid() {
		return &OversizedStdioRequest{ID: id}
	}
	return ErrTooLarge
}

type stdioDrain struct {
	reader    *bufio.Reader
	remaining int
	delimited bool
	err       error
}

func (d *stdioDrain) Read(p []byte) (int, error) {
	switch {
	case d.err != nil:
		return 0, d.err
	case d.delimited:
		return 0, io.EOF
	case d.remaining == 0:
		d.err = errors.New("stdio drain limit reached before delimiter")
		return 0, d.err
	case len(p) == 0:
		return 0, nil
	}
	if _, err := d.reader.Peek(1); err != nil {
		// The decoder expects EOF after a valid object. EOF from the pipe
		// cannot stand in for the missing frame delimiter.
		d.err = fmt.Errorf("stdio drain ended without a delimiter: %w", err)
		return 0, d.err
	}
	n := min(len(p), d.reader.Buffered(), d.remaining)
	buffered, _ := d.reader.Peek(n)
	if lineEnd := bytes.IndexByte(buffered, '\n'); lineEnd >= 0 {
		n = lineEnd + 1
		d.delimited = true
	}
	copy(p, buffered[:n])
	_, _ = d.reader.Discard(n)
	d.remaining -= n
	return n, nil
}

// The JSON decoder skips compound values token by token. The reader budget
// also bounds buffering of indivisible strings and numbers.
const maxStdioAttributionToken = MaxMessage + 4096

type attributionReader struct {
	io.Reader
	offset int64
	limit  int64
}

func (r *attributionReader) Read(p []byte) (int, error) {
	remaining := r.limit - r.offset
	if remaining <= 0 {
		return 0, errors.New("stdio attribution token exceeds the byte limit")
	}
	n, err := r.Reader.Read(p[:min(int64(len(p)), remaining)])
	r.offset += int64(n)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read oversized stdio envelope: %w", err)
	}
	return n, nil
}

type oversizedEnvelope struct {
	source    *attributionReader
	decoder   *jsontext.Decoder
	id        ID
	version   bool
	hasResult bool
	hasMethod bool
	request   bool
}

func oversizedReplyID(r io.Reader) ID {
	return oversizedEnvelopeID(r, false)
}

func oversizedRequestID(r io.Reader) ID {
	return oversizedEnvelopeID(r, true)
}

func oversizedEnvelopeID(r io.Reader, request bool) ID {
	source := &attributionReader{Reader: r}
	m := oversizedEnvelope{source: source, decoder: jsontext.NewDecoder(source), request: request}
	token, err := m.next()
	if err != nil || token.Kind() != '{' {
		return ID{}
	}
	if !m.readFields() || !m.version {
		return ID{}
	}
	if request && !m.hasMethod {
		return ID{}
	}
	if !request && !m.hasResult {
		return ID{}
	}
	if _, err := m.next(); !errors.Is(err, io.EOF) {
		return ID{}
	}
	return m.id
}

func (m *oversizedEnvelope) next() (jsontext.Token, error) {
	m.source.limit = m.decoder.InputOffset() + maxStdioAttributionToken
	token, err := m.decoder.ReadToken()
	if errors.Is(err, io.EOF) {
		return token, io.EOF
	}
	if err != nil {
		return token, fmt.Errorf("read oversized stdio token: %w", err)
	}
	return token, nil
}

func (m *oversizedEnvelope) readFields() bool {
	for {
		token, err := m.next()
		if err != nil {
			return false
		}
		if token.Kind() == '}' {
			return true
		}
		if token.Kind() != '"' || !m.readField(token.String()) {
			return false
		}
	}
}

func (m *oversizedEnvelope) readField(name string) bool {
	switch name {
	case "id":
		return m.readID()
	case "jsonrpc":
		token, err := m.next()
		m.version = err == nil && token.Kind() == '"' && token.String() == jsonRPCVersion
		return m.version
	case "method":
		if !m.request || m.hasMethod {
			return false
		}
		token, err := m.next()
		m.hasMethod = err == nil && token.Kind() == '"' && token.String() != ""
		return m.hasMethod
	case "params":
		if !m.request {
			return false
		}
		return m.skipValue(false)
	case keyResult, "error":
		if m.request || m.hasResult {
			return false
		}
		m.hasResult = true
		return m.skipValue(true)
	default:
		return m.skipValue(false)
	}
}

func (m *oversizedEnvelope) readID() bool {
	m.source.limit = m.decoder.InputOffset() + maxStdioAttributionToken
	kind := m.decoder.PeekKind()
	if kind != '"' && kind != '0' {
		return false
	}
	raw, err := m.decoder.ReadValue()
	if err != nil || len(raw) > MaxMessage {
		return false
	}
	m.id, _ = ParseID(raw)
	return m.id.Valid()
}

func (m *oversizedEnvelope) skipValue(objectOnly bool) bool {
	// SkipValue streams without buffering the skipped JSON value, while
	// stdioDrain still enforces the 16 MiB drain cap on the underlying pipe.
	m.source.limit = MaxMessage + MaxStdioDrain + 2
	if objectOnly && m.decoder.PeekKind() != '{' {
		return false
	}
	return m.decoder.SkipValue() == nil
}

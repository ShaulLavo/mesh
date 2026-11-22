package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
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
		// The scanner expects EOF after a valid object. EOF from the pipe
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

const (
	oversizeScanBufSize = 4096
	maxOversizeKeyBytes = 4096
	maxOversizeDepth    = 10000
)

func oversizedReplyID(r io.Reader) ID {
	return oversizedEnvelopeID(r, false)
}

func oversizedRequestID(r io.Reader) ID {
	return oversizedEnvelopeID(r, true)
}

// oversizeScanner validates a JSON-RPC envelope directly from a small fixed
// buffer, stream-discarding skipped scalars (even multi-megabyte strings or
// numbers) without buffering them in memory.
type oversizeScanner struct {
	r         *bufio.Reader
	request   bool
	id        ID
	seenID    bool
	version   bool
	hasResult bool
	hasMethod bool
	hasParams bool
}

func oversizedEnvelopeID(r io.Reader, request bool) ID {
	sc := oversizeScanner{r: bufio.NewReaderSize(r, oversizeScanBufSize), request: request}
	if !sc.expectByte('{') || !sc.readTopFields() || !sc.version || !sc.seenID {
		return ID{}
	}
	if request && !sc.hasMethod {
		return ID{}
	}
	if !request && !sc.hasResult {
		return ID{}
	}
	if !sc.expectEOF() {
		return ID{}
	}
	return sc.id
}

func (s *oversizeScanner) readTopFields() bool {
	c, ok := s.nextNonSpace()
	if !ok {
		return false
	}
	if c == '}' {
		return true
	}
	if err := s.r.UnreadByte(); err != nil {
		return false
	}
	for {
		name, known, ok := s.readTopKey()
		if !ok || !s.expectByte(':') {
			return false
		}
		if !s.readTopField(name, known) {
			return false
		}
		c, ok := s.nextNonSpace()
		if !ok {
			return false
		}
		if c == '}' {
			return true
		}
		if c != ',' {
			return false
		}
	}
}

func (s *oversizeScanner) readTopKey() (string, bool, bool) {
	c, ok := s.nextNonSpace()
	if !ok || c != '"' {
		return "", false, false
	}
	raw, truncated, ok := s.scanString(maxOversizeKeyBytes)
	if !ok {
		return "", false, false
	}
	if truncated {
		return "", false, true
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return "", false, false
	}
	return name, true, true
}

func (s *oversizeScanner) readTopField(name string, known bool) bool {
	if !known {
		return s.skipValue(false)
	}
	switch name {
	case "id":
		return s.readID()
	case "jsonrpc":
		return s.readVersion()
	case "method":
		return s.readMethod()
	case "params":
		if !s.request || s.hasParams {
			return false
		}
		s.hasParams = true
		return s.skipValue(false)
	case keyResult, "error":
		if s.request || s.hasResult {
			return false
		}
		s.hasResult = true
		return s.skipValue(true)
	default:
		return s.skipValue(false)
	}
}

func (s *oversizeScanner) readVersion() bool {
	if s.version {
		return false
	}
	c, ok := s.nextNonSpace()
	if !ok || c != '"' {
		return false
	}
	raw, truncated, ok := s.scanString(64)
	if !ok || truncated {
		return false
	}
	var v string
	s.version = json.Unmarshal(raw, &v) == nil && v == jsonRPCVersion
	return s.version
}

func (s *oversizeScanner) readMethod() bool {
	if !s.request || s.hasMethod {
		return false
	}
	c, ok := s.nextNonSpace()
	if !ok || c != '"' {
		return false
	}
	raw, truncated, ok := s.scanString(maxOversizeKeyBytes)
	if !ok || truncated {
		return false
	}
	var method string
	s.hasMethod = json.Unmarshal(raw, &method) == nil && method != ""
	return s.hasMethod
}

func (s *oversizeScanner) readID() bool {
	if s.seenID {
		return false
	}
	c, ok := s.nextNonSpace()
	if !ok {
		return false
	}
	var raw []byte
	switch {
	case c == '"':
		var truncated bool
		raw, truncated, ok = s.scanString(MaxMessage)
		if !ok || truncated {
			return false
		}
	case c == '-' || (c >= '0' && c <= '9'):
		var truncated bool
		raw, truncated, ok = s.scanNumber(c, MaxMessage)
		if !ok || truncated {
			return false
		}
	default:
		return false
	}
	s.id, ok = ParseID(raw)
	s.seenID = ok && s.id.Valid()
	return s.seenID
}

type containerState uint8

const (
	expectObjKeyOrEnd containerState = iota + 1
	expectObjColon
	expectObjValue
	expectObjCommaOrEnd
	expectArrValueOrEnd
	expectArrCommaOrEnd
)

// skipValue stream-validates one JSON value without buffering skipped scalars
// or relying on Go call-stack recursion.
func (s *oversizeScanner) skipValue(objectOnly bool) bool {
	c, ok := s.nextNonSpace()
	if !ok {
		return false
	}
	if objectOnly && c != '{' {
		return false
	}
	stack := make([]containerState, 0, 16)
	if !s.consumeValueStart(c, &stack) {
		return false
	}
	for len(stack) > 0 {
		top := len(stack) - 1
		if !s.stepContainer(&stack, top) {
			return false
		}
	}
	return true
}

func (s *oversizeScanner) stepContainer(stack *[]containerState, top int) bool {
	c, ok := s.nextNonSpace()
	if !ok {
		return false
	}
	switch (*stack)[top] {
	case expectObjKeyOrEnd, expectObjColon, expectObjValue, expectObjCommaOrEnd:
		return s.stepObject(stack, top, c)
	case expectArrValueOrEnd, expectArrCommaOrEnd:
		return s.stepArray(stack, top, c)
	default:
		return false
	}
}

func (s *oversizeScanner) stepObject(stack *[]containerState, top int, c byte) bool {
	switch (*stack)[top] {
	case expectObjKeyOrEnd:
		if c == '}' {
			*stack = (*stack)[:top]
			return true
		}
		if c != '"' || !s.skipStringBody() {
			return false
		}
		(*stack)[top] = expectObjColon
		return true
	case expectObjColon:
		if c != ':' {
			return false
		}
		(*stack)[top] = expectObjValue
		return true
	case expectObjValue:
		(*stack)[top] = expectObjCommaOrEnd
		return s.consumeValueStart(c, stack)
	case expectObjCommaOrEnd:
		if c == '}' {
			*stack = (*stack)[:top]
			return true
		}
		if c != ',' {
			return false
		}
		next, ok := s.nextNonSpace()
		if !ok || next != '"' || !s.skipStringBody() {
			return false
		}
		(*stack)[top] = expectObjColon
		return true
	default:
		return false
	}
}

func (s *oversizeScanner) stepArray(stack *[]containerState, top int, c byte) bool {
	if c == ']' {
		*stack = (*stack)[:top]
		return true
	}
	if (*stack)[top] == expectArrValueOrEnd {
		(*stack)[top] = expectArrCommaOrEnd
		return s.consumeValueStart(c, stack)
	}
	if c != ',' {
		return false
	}
	next, ok := s.nextNonSpace()
	if !ok {
		return false
	}
	return s.consumeValueStart(next, stack)
}

func (s *oversizeScanner) consumeValueStart(c byte, stack *[]containerState) bool {
	switch {
	case c == '{':
		if len(*stack) >= maxOversizeDepth {
			return false
		}
		*stack = append(*stack, expectObjKeyOrEnd)
		return true
	case c == '[':
		if len(*stack) >= maxOversizeDepth {
			return false
		}
		*stack = append(*stack, expectArrValueOrEnd)
		return true
	case c == '"':
		return s.skipStringBody()
	case c == '-' || (c >= '0' && c <= '9'):
		_, _, ok := s.scanNumber(c, 0)
		return ok
	case c == 't':
		return s.expectLiteral("true"[1:])
	case c == 'f':
		return s.expectLiteral("false"[1:])
	case c == 'n':
		return s.expectLiteral("null"[1:])
	default:
		return false
	}
}

func (s *oversizeScanner) nextNonSpace() (byte, bool) {
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			return 0, false
		}
		if !isJSONSpace(c) {
			return c, true
		}
	}
}

func (s *oversizeScanner) expectByte(want byte) bool {
	c, ok := s.nextNonSpace()
	return ok && c == want
}

func (s *oversizeScanner) expectEOF() bool {
	for {
		c, err := s.r.ReadByte()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil || !isJSONSpace(c) {
			return false
		}
	}
}

func (s *oversizeScanner) expectLiteral(rest string) bool {
	for i := range len(rest) {
		c, err := s.r.ReadByte()
		if err != nil || c != rest[i] {
			return false
		}
	}
	return true
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// skipStringBody validates a JSON string after its opening '"' in bounded
// chunks without allocating or accumulating the scalar bytes.
func (s *oversizeScanner) skipStringBody() bool {
	_, _, ok := s.scanString(0)
	return ok
}

type boundedCapture struct {
	max       int
	out       []byte
	truncated bool
}

func newBoundedCapture(maxCapture int) boundedCapture {
	if maxCapture <= 0 {
		return boundedCapture{}
	}
	return boundedCapture{max: maxCapture, out: make([]byte, 0, min(maxCapture, 128))}
}

func (c *boundedCapture) put(b byte) {
	if c.max <= 0 || c.truncated {
		return
	}
	if len(c.out) >= c.max {
		c.out = nil
		c.truncated = true
		return
	}
	c.out = append(c.out, b)
}

// scanString validates a JSON string after its opening '"'. When maxCapture > 0
// it captures the raw JSON token (including quotes) up to maxCapture bytes;
// if the string exceeds maxCapture, capture stops and truncated is true while
// the remainder is still strictly validated in O(1) memory.
func (s *oversizeScanner) scanString(maxCapture int) ([]byte, bool, bool) {
	capBuf := newBoundedCapture(maxCapture)
	capBuf.put('"')
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			return nil, false, false
		}
		if c == '"' {
			capBuf.put('"')
			return capBuf.out, capBuf.truncated, true
		}
		if !s.scanStringChar(c, capBuf.put) {
			return nil, false, false
		}
	}
}

func (s *oversizeScanner) scanStringChar(c byte, capture func(byte)) bool {
	if c < 0x20 {
		return false
	}
	if c == '\\' {
		capture('\\')
		return s.scanEscape(capture)
	}
	if c < 0x80 {
		capture(c)
		return true
	}
	return s.scanUTF8(c, capture)
}

func (s *oversizeScanner) scanEscape(capture func(byte)) bool {
	c, err := s.r.ReadByte()
	if err != nil {
		return false
	}
	switch c {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		capture(c)
		return true
	case 'u':
		capture('u')
		for range 4 {
			h, err := s.r.ReadByte()
			if err != nil || !isHexDigit(h) {
				return false
			}
			capture(h)
		}
		return true
	default:
		return false
	}
}

func (s *oversizeScanner) scanUTF8(b0 byte, capture func(byte)) bool {
	capture(b0)
	var extra int
	var minNext, maxNext byte = 0x80, 0xBF
	switch {
	case b0 >= 0xC2 && b0 <= 0xDF:
		extra = 1
	case b0 == 0xE0:
		extra, minNext = 2, 0xA0
	case b0 >= 0xE1 && b0 <= 0xEC || b0 >= 0xEE && b0 <= 0xEF:
		extra = 2
	case b0 == 0xED:
		extra, maxNext = 2, 0x9F
	case b0 == 0xF0:
		extra, minNext = 3, 0x90
	case b0 >= 0xF1 && b0 <= 0xF3:
		extra = 3
	case b0 == 0xF4:
		extra, maxNext = 3, 0x8F
	default:
		return false
	}
	for i := range extra {
		b, err := s.r.ReadByte()
		if err != nil {
			return false
		}
		lo, hi := byte(0x80), byte(0xBF)
		if i == 0 {
			lo, hi = minNext, maxNext
		}
		if b < lo || b > hi {
			return false
		}
		capture(b)
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// scanNumber validates a JSON number starting with first in O(1) memory,
// capturing up to maxCapture bytes when maxCapture > 0.
func (s *oversizeScanner) scanNumber(first byte, maxCapture int) ([]byte, bool, bool) {
	capBuf := newBoundedCapture(maxCapture)
	if !s.scanIntegerPart(first, capBuf.put) {
		return nil, false, false
	}
	if !s.scanNumberTail(capBuf.put) {
		return nil, false, false
	}
	return capBuf.out, capBuf.truncated, true
}

func (s *oversizeScanner) scanIntegerPart(first byte, capture func(byte)) bool {
	c := first
	capture(c)
	if c == '-' {
		var err error
		c, err = s.r.ReadByte()
		if err != nil || c < '0' || c > '9' {
			return false
		}
		capture(c)
	}
	if c == '0' {
		return true
	}
	return s.scanDigits(capture)
}

func (s *oversizeScanner) scanNumberTail(capture func(byte)) bool {
	next, err := s.r.ReadByte()
	if errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
		return false
	}
	if next == '.' {
		capture('.')
		if !s.scanRequiredDigits(capture) {
			return false
		}
		next, err = s.r.ReadByte()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
	if next == 'e' || next == 'E' {
		capture(next)
		return s.scanExponent(capture)
	}
	return s.r.UnreadByte() == nil
}

func (s *oversizeScanner) scanDigits(capture func(byte)) bool {
	for {
		c, err := s.r.ReadByte()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
		if c < '0' || c > '9' {
			return s.r.UnreadByte() == nil
		}
		capture(c)
	}
}

func (s *oversizeScanner) scanRequiredDigits(capture func(byte)) bool {
	c, err := s.r.ReadByte()
	if err != nil || c < '0' || c > '9' {
		return false
	}
	capture(c)
	return s.scanDigits(capture)
}

func (s *oversizeScanner) scanExponent(capture func(byte)) bool {
	c, err := s.r.ReadByte()
	if err != nil {
		return false
	}
	if c == '+' || c == '-' {
		capture(c)
		return s.scanRequiredDigits(capture)
	}
	if c < '0' || c > '9' {
		return false
	}
	capture(c)
	return s.scanDigits(capture)
}

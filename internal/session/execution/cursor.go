package execution

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const DefaultPage = 64 << 10
const MaxPage = 1 << 20

var ErrCursorExpired = errors.New("output cursor expired")
var ErrCursorInvalid = errors.New("output cursor is in the future")
var ErrCursorFormat = errors.New("invalid output cursor")
var ErrResultExpired = errors.New("detailed execution result expired")
var ErrInstance = errors.New("execution instance does not match")

type cursor struct {
	Version    int    `json:"version"`
	Host       string `json:"host"`
	Session    string `json:"session"`
	Instance   string `json:"instance"`
	Stream     string `json:"stream"`
	Generation string `json:"generation"`
	Offset     string `json:"offset"`
}

func offsetText(value uint64) string { return strconv.FormatUint(value, 10) }
func parseOffset(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || offsetText(n) != value {
		return 0, ErrCursorFormat
	}
	return n, nil
}
func makeCursor(b Binding, generation string, offset uint64) string {
	raw, _ := json.Marshal(cursor{1, b.Host, b.Reservation.ID, b.Reservation.Instance, ptyStream, generation, offsetText(offset)})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func readCursor(value string) (cursor, uint64, error) {
	var c cursor
	if len(value) > 2048 {
		return c, 0, ErrCursorFormat
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return c, 0, ErrCursorFormat
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 {
		return c, 0, ErrCursorFormat
	}
	canonical, _ := json.Marshal(c)
	if !bytes.Equal(raw, canonical) {
		return c, 0, ErrCursorFormat
	}
	n, err := parseOffset(c.Offset)
	return c, n, err
}

type Page struct {
	HostID         string `json:"hostId"`
	SessionID      string `json:"sessionId"`
	Instance       string `json:"instance"`
	Stream         string `json:"stream"`
	Generation     string `json:"generation"`
	Bytes          []byte `json:"bytesBase64"`
	StartCursor    string `json:"startCursor"`
	NextCursor     string `json:"nextCursor"`
	EarliestCursor string `json:"earliestCursor"`
	EndCursor      string `json:"endCursor"`
	ObservedEnd    string `json:"observedEnd"`
	StreamClosed   bool   `json:"streamClosed"`
	EOF            bool   `json:"eof"`
	CloseReason    string `json:"closeReason"`
	Loss           string `json:"loss"`
	Available      bool   `json:"available"`
	Gap            bool   `json:"gap"`
	Reason         string `json:"reason,omitempty"`
}

func (m manifest) page(b Binding, value string) (Page, uint64, error) {
	p := Page{HostID: b.Host, SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, Stream: ptyStream, Generation: m.Generation, Bytes: []byte{}, EarliestCursor: makeCursor(b, m.Generation, m.Earliest), EndCursor: makeCursor(b, m.Generation, m.End), ObservedEnd: offsetText(m.Observed), StreamClosed: m.Closed, CloseReason: m.CloseReason, Loss: m.Loss, Available: !m.Expired}
	offset := m.Earliest
	if value != "" {
		c, n, err := readCursor(value)
		if err != nil {
			return Page{}, 0, err
		}
		if c.Host != b.Host || c.Session != b.Reservation.ID || c.Instance != b.Reservation.Instance || c.Stream != ptyStream || c.Generation != m.Generation {
			p.Gap = true
			p.Reason = "binding_mismatch"
			return p, 0, ErrCursorExpired
		}
		offset = n
	}
	p.StartCursor = makeCursor(b, m.Generation, offset)
	p.NextCursor = p.StartCursor
	if m.Expired || offset < m.Earliest {
		p.Gap = true
		p.Reason = "retained_range"
		return p, offset, ErrCursorExpired
	}
	if offset > m.End {
		p.Reason = "future_offset"
		return p, offset, ErrCursorInvalid
	}
	return p, offset, nil
}
func validatePageSize(size int) (int, error) {
	if size == 0 {
		return DefaultPage, nil
	}
	if size < 1 || size > MaxPage {
		return 0, fmt.Errorf("output page must contain 1 to %d bytes", MaxPage)
	}
	return size, nil
}

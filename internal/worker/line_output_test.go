package worker

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/session"
)

func TestLineReadMaxSizeLFAndCRLF(t *testing.T) {
	// Exactly mcpwire.MaxMessage (3,670,016) JSON bytes framed with LF or CRLF
	// must be accepted as a normal frame, while MaxMessage+1 bytes must be
	// rejected as oversized on both LF and CRLF.
	prefix := `{"jsonrpc":"2.0","id":"exact","result":{"t":"`
	suffix := `"}}`
	exactPayload := prefix + strings.Repeat("a", mcpwire.MaxMessage-len(prefix)-len(suffix)) + suffix
	if len(exactPayload) != mcpwire.MaxMessage {
		t.Fatalf("setup len = %d, want %d", len(exactPayload), mcpwire.MaxMessage)
	}
	for _, tc := range []struct {
		name  string
		delim string
	}{
		{"LF", "\n"},
		{"CRLF", "\r\n"},
	} {
		t.Run("exact_"+tc.name, func(t *testing.T) {
			raw := []byte(exactPayload + tc.delim)
			reader := bufio.NewReaderSize(bytes.NewReader(raw), 32<<10)
			frame, err := readLineFrame(reader)
			if err != nil {
				t.Fatalf("exact %s frame error: %v", tc.name, err)
			}
			if len(frame.oversizeID) != 0 {
				t.Fatalf("exact %s frame misclassified as oversized: id=%s", tc.name, frame.oversizeID)
			}
			trimmed := bytes.TrimSuffix(bytes.TrimSuffix(frame.data, []byte("\n")), []byte("\r"))
			if string(trimmed) != exactPayload {
				t.Fatalf("exact %s payload length = %d, want %d", tc.name, len(trimmed), len(exactPayload))
			}
		})
		t.Run("over_by_one_"+tc.name, func(t *testing.T) {
			overPayload := prefix + strings.Repeat("a", mcpwire.MaxMessage-len(prefix)-len(suffix)+1) + suffix
			if len(overPayload) != mcpwire.MaxMessage+1 {
				t.Fatalf("setup len = %d, want %d", len(overPayload), mcpwire.MaxMessage+1)
			}
			next := `{"jsonrpc":"2.0","id":"next","result":{"ok":true}}` + tc.delim
			reader := bufio.NewReaderSize(strings.NewReader(overPayload+tc.delim+next), 32<<10)
			frame, err := readLineFrame(reader)
			if err != nil {
				t.Fatalf("over-by-one %s JSON reply faulted framing: %v", tc.name, err)
			}
			if string(frame.oversizeID) != `"exact"` || len(frame.data) != 0 {
				t.Fatalf("over-by-one %s frame = %+v, want oversizeID \"exact\"", tc.name, frame)
			}
			following, err := readLineFrame(reader)
			if err != nil || string(following.data) != next {
				t.Fatalf("following %s frame = %q, %v", tc.name, following.data, err)
			}
		})
	}
}

func TestLineReadBoundsAndPartialInput(t *testing.T) {
	raw := append(bytes.Repeat([]byte{'x'}, session.MaxLineBytes-1), '\n')
	reader := bufio.NewReaderSize(bytes.NewReader(raw), 32<<10)
	frame, err := readLineFrame(reader)
	if err != nil || !bytes.Equal(frame.data, raw) {
		t.Fatalf("maximum line: %d bytes, %v", len(frame.data), err)
	}
	raw[len(raw)-1] = 'x'
	reader = bufio.NewReaderSize(bytes.NewReader(append(raw, '\n')), 32<<10)
	if _, err := readLineFrame(reader); !errors.Is(err, errLineTooLarge) {
		t.Fatalf("oversized line: %v", err)
	}
	reader = bufio.NewReader(bytes.NewReader([]byte("partial")))
	frame, err = readLineFrame(reader)
	if fault := lineFault(frame.data, err); fault != "partial_line" {
		t.Fatalf("incomplete line fault: %s", fault)
	}
}

func TestOversizedJSONRPCReplyRecoversFraming(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":"mesh-1-7","result":{"text":"` + strings.Repeat("a", 4<<20) + `"}}` + "\n"
	next := `{"jsonrpc":"2.0","id":"mesh-1-8","result":{"resultType":"complete"}}` + "\n"
	reader := bufio.NewReaderSize(strings.NewReader(big+next), 32<<10)
	frame, err := readLineFrame(reader)
	if err != nil {
		t.Fatalf("oversized attributable reply faulted framing: %v", err)
	}
	if string(frame.oversizeID) != `"mesh-1-7"` || len(frame.data) != 0 {
		t.Fatalf("oversize frame = %+v, want id \"mesh-1-7\"", frame)
	}
	following, err := readLineFrame(reader)
	if err != nil || string(following.data) != next {
		t.Fatalf("following line = %q, %v", following.data, err)
	}
}

func TestLineSlowReaderGetsGap(t *testing.T) {
	output := newLineOutput()
	for range lineOutputFrames + 1 {
		output.offerFrame(lineFrame{data: []byte("whole line\n")})
	}
	if _, fault := output.attach(0); fault != "frame_gap" {
		t.Fatalf("old cursor fault: %s", fault)
	}
	if _, fault := output.attach(lineOutputFrames + 2); fault != "future_frame" {
		t.Fatalf("future cursor fault: %s", fault)
	}
	if frame, fault, _ := output.readLineFrame(1); fault != "" || string(frame.data) != "whole line\n" {
		t.Fatalf("first retained frame: %q, %s", frame.data, fault)
	}
}

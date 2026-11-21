package worker

import (
	"bufio"
	"bytes"
	"errors"
	"testing"

	"github.com/shaul/mesh/internal/session"
)

func TestLineReadBoundsAndPartialInput(t *testing.T) {
	raw := append(bytes.Repeat([]byte{'x'}, session.MaxLineBytes-1), '\n')
	reader := bufio.NewReaderSize(bytes.NewReader(raw), 32<<10)
	line, err := readLine(reader)
	if err != nil || !bytes.Equal(line, raw) {
		t.Fatalf("maximum line: %d bytes, %v", len(line), err)
	}
	raw[len(raw)-1] = 'x'
	reader = bufio.NewReaderSize(bytes.NewReader(append(raw, '\n')), 32<<10)
	if _, err := readLine(reader); !errors.Is(err, errLineTooLarge) {
		t.Fatalf("oversized line: %v", err)
	}
	reader = bufio.NewReader(bytes.NewReader([]byte("partial")))
	line, err = readLine(reader)
	if fault := lineFault(line, err); fault != "partial_line" {
		t.Fatalf("incomplete line fault: %s", fault)
	}
}

func TestLineSlowReaderGetsGap(t *testing.T) {
	output := newLineOutput()
	for range lineOutputFrames + 1 {
		output.offer([]byte("whole line\n"))
	}
	if _, fault := output.attach(0); fault != "frame_gap" {
		t.Fatalf("old cursor fault: %s", fault)
	}
	if _, fault := output.attach(lineOutputFrames + 2); fault != "future_frame" {
		t.Fatalf("future cursor fault: %s", fault)
	}
	if line, fault, _ := output.readFrame(1); fault != "" || string(line) != "whole line\n" {
		t.Fatalf("first retained frame: %q, %s", line, fault)
	}
}

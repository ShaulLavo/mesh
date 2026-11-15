package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCaptureKeepsTheLatestFrameBeyondOneMiB(t *testing.T) {
	input := append([]byte("\x1b[2J\x1b[HFixture Beta"), bytes.Repeat([]byte{'\r'}, 1<<20)...)
	input = append(input, []byte("\x1b[2J\x1b[HHosts")...)
	var output bytes.Buffer
	if err := captureScreen(bytes.NewReader(input), &output, 160, 48); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Hosts") || strings.Contains(output.String(), "Fixture Beta") {
		t.Fatal("capture used an earlier frame instead of the latest terminal state")
	}
}

func TestCaptureAcceptsTheTerminalBudget(t *testing.T) {
	input := append([]byte("Hosts"), bytes.Repeat([]byte{'\r'}, maxCaptureBytes-len("Hosts"))...)
	var output bytes.Buffer
	if err := captureScreen(bytes.NewReader(input), &output, 160, 48); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Hosts") {
		t.Fatal("bounded capture lost the frame")
	}
}

func TestCaptureRejectsOversizedInputWithoutReportingAPrefixFrame(t *testing.T) {
	var output bytes.Buffer
	err := captureScreen(bytes.NewReader(bytes.Repeat([]byte{'\r'}, maxCaptureBytes+1)), &output, 160, 48)
	if err == nil || output.Len() != 0 {
		t.Fatal("oversized input produced a misleading prefix frame")
	}
}

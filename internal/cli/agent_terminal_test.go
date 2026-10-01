package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/testenv"
)

func TestAgentTerminalControlCReachesNativeProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestAgentTerminalProcess$") //nolint:gosec // this test binary is its own isolated PTY helper
	command.Env = append(testenv.ForProcess(t.TempDir()), "MESH_AGENT_TERMINAL_TEST=1")
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close(); _ = command.Process.Kill() })
	chunks := make(chan []byte, 8)
	go readAgentTestTerminal(terminal, chunks)
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	var output bytes.Buffer
	awaitAgentTerminalMarker(t, ctx, chunks, &output, "NATIVE_READY")
	if _, err := terminal.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	awaitAgentTerminalMarker(t, ctx, chunks, &output, "NATIVE_INTERRUPT")
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 42 {
			t.Fatalf("Ctrl+C changed the provider's exit status: %v; output=%q", err, output.String())
		}
	case <-ctx.Done():
		t.Fatalf("Ctrl+C did not finish the native process: %q", output.String())
	}
}

func TestAgentTerminalProcess(t *testing.T) {
	if os.Getenv("MESH_AGENT_TERMINAL_TEST") != "1" {
		return
	}
	command := &cobra.Command{}
	command.SetIn(os.Stdin)
	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(testenv.ForProcess(os.Getenv("HOME")), "MESH_AGENT_TERMINAL_TEST=native")
	err = runNativeAgent(command, executable, []string{"-test.run=^TestAgentNativeTerminalProcess$"}, "", env)
	if code, ok := StatusCode(err); ok {
		os.Exit(code)
	}
	t.Fatalf("native test process returned unexpectedly: %v", err)
}

func TestAgentNativeTerminalProcess(t *testing.T) {
	if os.Getenv("MESH_AGENT_TERMINAL_TEST") != "native" {
		return
	}
	// A shell can defer its INT trap until a blocking read returns. This
	// fixture must handle even a prompt-time interrupt without further input.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	if _, err := fmt.Fprintln(os.Stdout, "NATIVE_READY"); err != nil {
		t.Fatal(err)
	}
	<-interrupts
	if _, err := fmt.Fprintln(os.Stdout, "NATIVE_INTERRUPT"); err != nil {
		t.Fatal(err)
	}
	os.Exit(42)
}

func readAgentTestTerminal(terminal *os.File, chunks chan<- []byte) {
	defer close(chunks)
	buffer := make([]byte, 1024)
	for {
		count, err := terminal.Read(buffer)
		if count > 0 {
			chunks <- bytes.Clone(buffer[:count])
		}
		if err != nil {
			return
		}
	}
}

func awaitAgentTerminalMarker(t *testing.T, ctx context.Context, chunks <-chan []byte, output *bytes.Buffer, marker string) {
	t.Helper()
	for !strings.Contains(output.String(), marker) {
		chunk, err := nextAgentTerminalChunk(ctx, chunks)
		if err != nil {
			t.Fatalf("terminal did not print %s: %v; output=%q", marker, err, output.String())
		}
		output.Write(chunk)
	}
}

func nextAgentTerminalChunk(ctx context.Context, chunks <-chan []byte) ([]byte, error) {
	select {
	case chunk, ok := <-chunks:
		if !ok {
			return nil, io.EOF
		}
		return chunk, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

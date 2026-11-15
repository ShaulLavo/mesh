//go:build unix

package closelog

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

func TestCloseDiagnosticsPreserveInteractiveScreen(t *testing.T) {
	master, screen := diagnosticPTY(t)
	useLogSink(t, screen)
	const before = "\x1b[2J\x1b[HFixture Beta"
	const after = "\x1b[2;1Hconnected"
	_, _ = io.WriteString(screen, before)
	Printf("transport: stage=tls-handshake error=%q", io.EOF)
	Fprintf(screen, "mesh: stage=established-stream error=%q\r\n", io.EOF)
	_, _ = io.WriteString(screen, after)
	assertPTYBytes(t, master, before+after)
}

func TestCloseDiagnosticsKeepRedirectedCausesWithTerminalStdout(t *testing.T) {
	master, screen := diagnosticPTY(t)
	previous := os.Stdout
	os.Stdout = screen
	t.Cleanup(func() { os.Stdout = previous })
	_, _ = io.WriteString(os.Stdout, "{\"ok\":true}")
	file, err := os.Create(filepath.Join(t.TempDir(), "client.err"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	writeCloseDiagnostics(t, file)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	fileOutput, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	assertCloseCauses(t, string(fileOutput))
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	writeCloseDiagnostics(t, writer)
	_ = writer.Close()
	pipeOutput, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	assertCloseCauses(t, string(pipeOutput))
	var capture bytes.Buffer
	writeCloseDiagnostics(t, &capture)
	assertCloseCauses(t, capture.String())
	assertPTYBytes(t, master, "{\"ok\":true}")
}

func TestConcurrentCloseDiagnosticsKeepAllCauses(t *testing.T) {
	var capture bytes.Buffer
	useLogSink(t, &capture)
	var group sync.WaitGroup
	for index := range 32 {
		group.Go(func() { Printf("stage=stream-read generation=%d error=%q", index, io.EOF) })
	}
	group.Wait()
	if got := strings.Count(capture.String(), `error="EOF"`); got != 32 {
		t.Fatalf("captured %d close causes, want 32", got)
	}
}

func writeCloseDiagnostics(t *testing.T, sink io.Writer) {
	t.Helper()
	useLogSink(t, sink)
	Printf("transport: stage=reconnect-dial reconnect=true error=%q", io.EOF)
	Fprintf(sink, "mesh: stage=attach-response classification=terminal error=%q\r\n", io.EOF)
}

func assertCloseCauses(t *testing.T, output string) {
	t.Helper()
	for _, want := range []string{`stage=reconnect-dial reconnect=true error="EOF"`, `stage=attach-response classification=terminal error="EOF"`} {
		if !strings.Contains(output, want) {
			t.Errorf("diagnostics %q missing %q", output, want)
		}
	}
}

func diagnosticPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, screen, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = screen.Close() })
	if _, err := term.MakeRaw(screen.Fd()); err != nil {
		t.Fatal(err)
	}
	return master, screen
}

func assertPTYBytes(t *testing.T, master *os.File, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(master, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("interactive bytes = %q, want %q", got, want)
	}
}

func useLogSink(t *testing.T, sink io.Writer) {
	t.Helper()
	previous := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(previous) })
}

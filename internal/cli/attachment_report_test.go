package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

type attachmentReportEnding struct {
	name    string
	message protocol.Control
	want    string
	status  int
}

func TestLocalAttachmentReports(t *testing.T) {
	for _, entry := range []string{"local", "attach", "resolved", "window"} {
		testAttachmentReportEndings(t, entry)
	}
}

func testAttachmentReportEndings(t *testing.T, entry string) {
	t.Helper()
	zero, failure := 0, 7
	for _, ending := range []attachmentReportEnding{
		{"exited", protocol.Control{Type: protocol.TypeExit, ExitCode: &zero}, "\r\nsession 7K3D exited (0)\r\n", 0},
		{"failed", protocol.Control{Type: protocol.TypeExit, ExitCode: &failure}, "\r\nsession 7K3D exited (7)\r\n", 7},
		{"detached", protocol.Control{Type: protocol.TypeDetach}, "\r\ndetached from 7K3D, still running\r\n", 0},
		{"disconnected", protocol.Control{}, "\r\ndisconnected from 7K3D\r\n", 0},
	} {
		t.Run(entry+"/"+ending.name, func(t *testing.T) { checkAttachmentReport(t, entry, ending) })
	}
}

func checkAttachmentReport(t *testing.T, entry string, ending attachmentReportEnding) {
	t.Helper()
	setupCommandTestHost(t)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	writeLocalSessionDir(t, "7K3D", worker.StateDetached)
	dir, err := paths.SessionDir("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	serveAttachmentEnding(t, paths.Socket(dir), ending.message)
	stderr, err := runAttachmentReport(t, entry, dir)
	if want := expectedAttachmentReport(entry, ending); stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if got, _ := StatusCode(err); got != ending.status {
		t.Errorf("status = %d, want %d (error %v)", got, ending.status, err)
	}
	if ending.status == 0 && err != nil {
		t.Fatal(err)
	}
}

func runAttachmentReport(t *testing.T, entry, dir string) (string, error) {
	t.Helper()
	if entry != "window" {
		return runAttachmentReportCommand(t, entry)
	}
	input, output := attachmentReportFiles(t)
	app := &application{dependencies: Dependencies{
		Stdin: input, Stdout: output, Stderr: output,
		Containment: func(context.Context) []protocol.SessionIdentity { return nil },
		Terminal:    func() (TerminalIdentity, bool) { return TerminalIdentity{}, false },
	}}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetErr(output)
	err := app.attachWindow(cmd, Session{Meta: worker.Meta{ID: "7K3D"}, Dir: dir}, paths.Socket(dir), nil, true, "", true)
	return readCommandFile(t, output), err
}

func runAttachmentReportCommand(t *testing.T, entry string) (string, error) {
	t.Helper()
	args := []string{"7K3D", "--raw"}
	switch entry {
	case "local":
		args = []string{"local", "-r", "--raw"}
	case "attach":
		args = []string{"attach", "7K3D", "--raw"}
	}
	_, stderr, err := executeCommand(t, Dependencies{Terminal: func() (TerminalIdentity, bool) { return TerminalIdentity{}, false }}, args...)
	return stderr, err
}

func expectedAttachmentReport(entry string, ending attachmentReportEnding) string {
	diagnostic := ""
	if ending.name == "disconnected" {
		diagnostic = "mesh: session=7K3D stage=established-stream classification=terminal failed=false error=\"transport: read stream frame: EOF\"\r\n"
	}
	if entry == "window" {
		return diagnostic
	}
	want := diagnostic + ending.want
	if entry == "local" {
		return "resuming 7K3D\n" + want
	}
	return want
}

func attachmentReportFiles(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	return input, output
}

func serveAttachmentEnding(t *testing.T, socket string, ending protocol.Control) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var servers sync.WaitGroup
	servers.Go(func() {
		for {
			stream, err := listener.Accept()
			if err != nil {
				return
			}
			servers.Go(func() {
				defer func() { _ = stream.Close() }()
				frame, err := protocol.NewReader(stream).ReadFrame()
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					t.Errorf("read attach request: %v", err)
					return
				}
				request, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					t.Errorf("decode attach request: %v", err)
					return
				}
				writer := protocol.NewWriter(stream)
				if err := writer.WriteControlMsg(protocol.Control{Type: protocol.TypeAttached, SessionID: request.SessionID}); err != nil {
					t.Errorf("acknowledge attach: %v", err)
					return
				}
				if ending.Type != "" {
					ending.SessionID = request.SessionID
					if err := writer.WriteControlMsg(ending); err != nil {
						t.Errorf("end attach: %v", err)
					}
				}
			})
		}
	})
	t.Cleanup(func() { _ = listener.Close(); servers.Wait() })
}

func TestAttachmentReportPreservesWriterErrors(t *testing.T) {
	failure := errors.New("report writer failed")
	for _, result := range []AttachResult{{Exited: true}, {Exited: true, ExitCode: 7}, {Detached: true}, {}} {
		err := writeAttachResult(attachmentReportErrorWriter{failure}, "7K3D", result)
		if !errors.Is(err, failure) {
			t.Errorf("result %+v error = %v, want writer failure", result, err)
		}
	}
}

type attachmentReportErrorWriter struct{ err error }

func (w attachmentReportErrorWriter) Write([]byte) (int, error) { return 0, w.err }

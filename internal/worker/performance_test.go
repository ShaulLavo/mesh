package worker

import (
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func BenchmarkQuietCheckpoint(b *testing.B) {
	w := &Worker{
		cfg:    Config{ID: "7K3D", HostID: "bench-host", Dir: b.TempDir(), Cwd: "/work", Command: []string{"/bin/sh"}},
		screen: terminalstate.NewScreen(120, 40), ring: session.NewRing(ringSize),
		recoveryState: recovery.Record{Version: recovery.Version, HostID: "bench-host", SessionID: "7K3D",
			Command: []string{"/bin/sh"}, Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryShell},
	}
	_, _ = w.screen.Write([]byte(strings.Repeat("\x1b[32mINFO\x1b[0m compiled package; tests passed\r\n", 400)))
	w.checkpointWriter = recovery.NewWriter(w.cfg.Dir, func(err error) { b.Error(err) })
	b.Cleanup(w.checkpointWriter.Close)
	if err := <-w.checkpoint(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := <-w.checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScreenWriteLogs(b *testing.B) {
	screen := terminalstate.NewScreen(120, 40)
	data := []byte(strings.Repeat("\x1b[32mINFO\x1b[0m build completed; tests passed; duration=12ms\r\n", 512))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = screen.Write(data)
	}
}

func BenchmarkAttachmentQueue32K(b *testing.B) {
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		b.Fatal(err)
	}
	attachment := newAttachment(nil, sid)
	data := make([]byte, 32<<10)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if !attachment.enqueueData(0, data) {
			b.Fatal("empty attachment queue refused a frame")
		}
		frame := <-attachment.queue
		attachment.releasePayload(frame)
	}
}

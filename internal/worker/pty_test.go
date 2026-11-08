//go:build linux || darwin

package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"golang.org/x/sys/unix"
)

func TestPTYDrainJoinsPumpWithRetainedSlave(t *testing.T) {
	for _, mode := range []string{"ordinary", "inspected", "captured"} {
		t.Run(mode, func(t *testing.T) { testRetainedSlaveDrain(t, mode) })
	}
}

func testRetainedSlaveDrain(t *testing.T, mode string) {
	t.Helper()
	pty, err := newWorkerPTY(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pty.Close() })
	slave := pty.(interface{ Slave() *os.File }).Slave()
	fd, err := unix.FcntlInt(slave.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	retained := os.NewFile(uintptr(fd), slave.Name())
	blocked := make(chan struct{})
	observed := &drainReadPTY{Pty: pty, blocked: blocked}
	w := &Worker{
		pty: observed, ring: session.NewRing(ringSize), screen: discardScreen{},
		pumpDone: make(chan struct{}), exited: make(chan struct{}),
		observeProcess: func(int, int) processObservation { return processObservation{} },
	}
	var pump, drain <-chan struct{}
	t.Cleanup(func() {
		// The retained slave releases even a raw blocking read on the red path.
		_ = retained.Close()
		_ = pty.Close()
		joinDrainFixture(t, pump, "pump cleanup")
		joinDrainFixture(t, drain, "drain cleanup")
	})
	if mode == "captured" {
		installDrainCapture(t, w)
	}
	w.cmd = exec.Command("/bin/sh", "-c", `printf 'head\000tail\033[31m'`)
	if err := pty.Start(w.cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !w.reaped {
			_ = w.cmd.Process.Kill()
			_, _ = w.waitChild()
		}
	})
	if mode != "ordinary" {
		w.currentProcessObservation()
		if err := pty.Resize(100, 30); err != nil {
			t.Fatal(err)
		}
		if cols, rows, err := pty.Size(); err != nil || cols != 100 || rows != 30 {
			t.Fatalf("size after observation = %dx%d, %v", cols, rows, err)
		}
	}
	_ = slave.Close()
	state, err := w.waitChild()
	if err != nil || state.ExitCode() != 0 {
		t.Fatalf("wait child = %v, %v", state, err)
	}
	pump = w.pumpDone
	go w.pump()
	joinDrainFixture(t, blocked, "pump entering read after child's output")
	done := make(chan struct{})
	drain = done
	go func() { defer close(done); w.drainPTY() }()
	joinDrainFixture(t, done, "drain joining pump while slave remains open")
	select {
	case <-w.pumpDone:
	default:
		t.Fatal("drain returned without joining the actual PTY pump")
	}
	if got := w.finalCaptureReason(); got != "drain_timeout" {
		t.Fatalf("closure = %q, want drain_timeout", got)
	}
	want := []byte("head\x00tail\x1b[31m")
	if got := w.ring.Last(len(want) + 1); !bytes.Equal(got, want) {
		t.Fatalf("retained PTY bytes = %q, want %q", got, want)
	}
	w.finish(0)
	if w.commitPTYChunk([]byte("late")) || !bytes.Equal(w.ring.Last(len(want)+1), want) {
		t.Fatal("output accepted after session.exit")
	}
}

type drainReadPTY struct {
	xpty.Pty
	reads   int
	blocked chan struct{}
}

func (p *drainReadPTY) Read(data []byte) (int, error) {
	p.reads++
	if p.reads == 2 {
		close(p.blocked)
	}
	n, err := p.Pty.Read(data)
	return n, workerExecutionError("read drain fixture PTY", err)
}

func joinDrainFixture(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
}

func installDrainCapture(t *testing.T, w *Worker) {
	t.Helper()
	root := t.TempDir()
	ref, err := session.NewReservation("7ABC", "boot", session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	binding := execution.NewBinding(execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "drain"}, execution.Intent{Kind: "pty"}, ref, nil)
	w.capture, err = execution.OpenCapture(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		summary, err := w.capture.Finish(context.Background(), w.finalCaptureReason())
		if err != nil {
			t.Error(err)
			return
		}
		if summary.EOF || summary.CloseReason != "drain_timeout" || captureStatus(summary) != "incomplete" {
			t.Errorf("retained slave capture = %+v", summary)
		}
		reader, err := execution.OpenOutput(root)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = reader.Close() }()
		page, err := reader.Page(context.Background(), binding, "pty", "", 4096)
		if err != nil || !bytes.Equal(page.Bytes, []byte("head\x00tail\x1b[31m")) || page.EOF {
			t.Errorf("captured PTY bytes = %+v, %v", page, err)
		}
	})
}

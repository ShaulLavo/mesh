package edge

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestEventLoggerCloseAcknowledgesCleanProcessExit(t *testing.T) {
	if os.Getenv("MESH_EDGE_CLOSE_CHILD") == "1" {
		sink := &eventCapture{}
		logger := newEventLogger(log.New(&captureAndStdout{sink}, "", 0), time.Now)
		for range 61 {
			logger.Print("edge event=invalid-public-host")
		}
		deadline := time.Now().Add(time.Second)
		for sink.count() < 60 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if sink.count() != 60 {
			os.Exit(2)
		}
		logger.Close()
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestEventLoggerCloseAcknowledgesCleanProcessExit$") //nolint:gosec // The OS identifies this test binary; arguments are fixed and no request controls the program.
	command.Env = append(os.Environ(), "MESH_EDGE_CLOSE_CHILD=1", "GORACE=atexit_sleep_ms=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("clean-exit child: %v %s", err, output)
	}
	if !bytes.Contains(output, []byte("event=events-dropped category=invalid-public-host dropped=1")) {
		t.Fatal("clean process exit lost its current-window drop summary")
	}
}

type captureAndStdout struct{ capture *eventCapture }

func (w *captureAndStdout) Write(raw []byte) (int, error) {
	_, _ = w.capture.Write(raw)
	n, err := os.Stdout.Write(raw)
	if err != nil {
		return n, fmt.Errorf("write child edge event: %w", err)
	}
	return n, nil
}
func (w *eventCapture) count() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.lines) }

func TestEventLoggerCloseBoundsBlockedSink(t *testing.T) {
	writer := &blockingEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	logger := newEventLogger(log.New(writer, "", 0), time.Now)
	logger.Print("edge event=invalid-public-host")
	<-writer.started
	start := time.Now()
	logger.Close()
	elapsed := time.Since(start)
	close(writer.release)
	if elapsed > 2*time.Second {
		t.Fatalf("blocked sink held shutdown for %s", elapsed)
	}
	logger.Print(strings.Repeat("ignored after close", 2))
}

package worker

import (
	"context"
	"errors"
	"github.com/shaul/mesh/internal/session/execution"
	"io"
	"syscall"
)

func (w *Worker) recordCaptureClosure(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.captureReason != "" {
		return
	}
	w.captureReason = "read_error"
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) {
		w.captureReason = "eof"
	}
}
func (w *Worker) finalCaptureReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.captureReason == "" {
		return "interrupted"
	}
	return w.captureReason
}
func captureStatus(s execution.StreamResult) string {
	if s.EOF && (s.Loss == "" || s.Loss == "retention") {
		return "complete"
	}
	return "incomplete"
}
func finishUnstartedCapture(a *execution.Attempt, c *execution.Capture, b execution.Binding) error {
	result := execution.Result{Binding: b, Termination: execution.Termination{Kind: "not_started", Reason: "child_start_failed"}, Capture: "not_recorded"}
	if c != nil {
		summary, err := c.Finish(context.Background(), "interrupted")
		if err != nil {
			return workerExecutionError("finish unstarted capture", err)
		}
		result.Capture = "incomplete"
		result.Streams = []execution.StreamResult{summary}
	}
	return workerExecutionError("publish unstarted capture", a.Finish(result))
}

var executionBoundary = func(string, string) {}
